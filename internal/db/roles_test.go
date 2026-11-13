package db_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/audit"
	"geoint/internal/claims"
	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/db"
	"geoint/internal/freshness"
	"geoint/internal/identity"
	"geoint/internal/jobs"
	"geoint/internal/search"
	"geoint/internal/sources"
	"geoint/internal/testutil"
)

// grants is the privilege matrix of migration 0021_roles.sql, table by
// table. Privileges are table-level; cols are columns the role may UPDATE
// without a table-level UPDATE. workerOwns: the worker owns the table (and
// so has every privilege on it); otherwise the migration role owns it.
//
// A table missing from this list fails TestRoleGrantsMatrix: a migration
// that adds a table must decide its grants (in SQL) and record them here.
type grant struct {
	workerOwns          bool
	worker, api         []string
	workerCols, apiCols []string
}

var (
	s    = []string{"SELECT"}
	si   = []string{"SELECT", "INSERT"}
	siu  = []string{"SELECT", "INSERT", "UPDATE"}
	sium = []string{"SELECT", "INSERT", "UPDATE", "MAINTAIN"}
	su   = []string{"SELECT", "UPDATE"}
)

var grantsMatrix = map[string]grant{
	"goose_db_version":       {},
	"source":                 {worker: s, api: s},
	"collection":             {worker: siu, api: s},
	"collection_file":        {worker: si, api: s},
	"collection_request":     {worker: si, workerCols: []string{"published_at"}, api: si, apiCols: []string{"published_at"}},
	"source_check":           {worker: siu, api: s},
	"entity":                 {worker: sium, api: si},
	"person_record":          {worker: sium, api: s, apiCols: []string{"entity_id", "identity_confidence", "locked_by_decision_id"}},
	"relationship":           {worker: sium, api: s, apiCols: []string{"src_entity_id", "dst_entity_id"}},
	"company_version":        {worker: sium, api: s},
	"identity_decision":      {worker: si, api: si},
	"identity_proposal":      {worker: siu, api: s, apiCols: []string{"status", "decided_by", "decided_at", "decision_id"}},
	"identity_ambiguous_key": {worker: s, api: si},
	"identity_resolve_done":  {worker: si, api: s},
	"job":                    {worker: []string{"SELECT", "UPDATE", "DELETE"}, api: si, apiCols: []string{"status", "error", "finished_at", "updated_at"}},
	"audit_log":              {api: []string{"INSERT"}},
	"search_state":           {worker: su, api: s},
	// Replaced by every search rebuild (internal/search/rebuild.go).
	"search_company":        {workerOwns: true, api: s},
	"search_person":         {workerOwns: true, api: s},
	"search_other":          {workerOwns: true, api: s},
	"search_frequent_token": {workerOwns: true, api: s},
	// Loader staging (truncated before and after every load).
	"receita_stg_file":        {workerOwns: true},
	"receita_stg_empresa":     {workerOwns: true},
	"receita_stg_estab":       {workerOwns: true},
	"receita_stg_socio":       {workerOwns: true},
	"receita_stg_scope":       {workerOwns: true},
	"receita_stg_company_src": {workerOwns: true},
	"receita_stg_company":     {workerOwns: true},
	"receita_stg_person":      {workerOwns: true},
	"receita_stg_person_new":  {workerOwns: true},
	"receita_stg_partner":     {workerOwns: true},
	"receita_stg_rel":         {workerOwns: true},
	"tse_stg_file":            {workerOwns: true},
	"tse_stg_cand":            {workerOwns: true},
	"tse_stg_receipt":         {workerOwns: true},
	"tse_stg_candidate":       {workerOwns: true},
	"tse_stg_person":          {workerOwns: true},
	"tse_stg_person_new":      {workerOwns: true},
	"tse_stg_donation":        {workerOwns: true},
	"tse_stg_claim":           {workerOwns: true},
	"tse_stg_rel":             {workerOwns: true},
}

var allPrivileges = []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER", "MAINTAIN"}

// claimTables can never lose a row, whatever the role.
var claimTables = []string{"entity", "person_record", "relationship", "company_version", "collection",
	"collection_file", "collection_request", "identity_decision", "identity_proposal", "identity_ambiguous_key",
	"identity_resolve_done", "audit_log", "source"}

type roleEnv struct {
	owner       *pgxpool.Pool
	worker, api *pgxpool.Pool
	schema      string
}

var roleCtx = context.Background()

// newRoleEnv migrates a fresh schema as the owner, gives the two roles
// random passwords through db.SetRolePasswords (as `geoint migrate` does)
// and opens one pool per role that logs in with them.
func newRoleEnv(t *testing.T) roleEnv {
	t.Helper()
	u := testutil.EmptySchemaURL(t)
	if err := db.Migrate(roleCtx, u); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	owner, err := db.Open(roleCtx, u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	testutil.SeedSources(t, owner)
	pw := map[string]string{db.WorkerRole: randomPassword(t), db.APIRole: randomPassword(t)}
	if err := db.SetRolePasswords(roleCtx, owner, pw); err != nil {
		t.Fatalf("set role passwords: %v", err)
	}
	parsed, _ := url.Parse(u)
	env := roleEnv{owner: owner, schema: parsed.Query().Get("search_path")}
	env.worker = openAs(t, u, db.WorkerRole, pw[db.WorkerRole])
	env.api = openAs(t, u, db.APIRole, pw[db.APIRole])
	return env
}

func randomPassword(t *testing.T) string {
	t.Helper()
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func openAs(t *testing.T, base, role, password string) *pgxpool.Pool {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	p, err := db.Open(roleCtx, u.String())
	if err != nil {
		t.Fatalf("log in as %s: %v", role, err)
	}
	t.Cleanup(p.Close)
	var who string
	if err := p.QueryRow(roleCtx, `SELECT current_user`).Scan(&who); err != nil || who != role {
		t.Fatalf("current_user = %q (%v), want %s", who, err, role)
	}
	return p
}

// denied asserts that sql fails with insufficient_privilege (42501).
func denied(t *testing.T, p *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	_, err := p.Exec(roleCtx, sql, args...)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("%s: err = %v, want permission denied (42501)", sql, err)
	}
}

func allowed(t *testing.T, p *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := p.Exec(roleCtx, sql, args...); err != nil {
		t.Errorf("%s: %v", sql, err)
	}
}

func TestRoleGrantsMatrix(t *testing.T) {
	env := newRoleEnv(t)
	var ownerRole string
	if err := env.owner.QueryRow(roleCtx, `SELECT current_user`).Scan(&ownerRole); err != nil {
		t.Fatal(err)
	}
	rows, err := env.owner.Query(roleCtx, `SELECT c.relname, pg_get_userbyid(c.relowner) FROM pg_class c
		WHERE c.relnamespace = (SELECT oid FROM pg_namespace WHERE nspname = current_schema()) AND c.relkind IN ('r', 'p')
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	owners := map[string]string{}
	for rows.Next() {
		var name, own string
		if err := rows.Scan(&name, &own); err != nil {
			t.Fatal(err)
		}
		owners[name] = own
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for name := range owners {
		if _, ok := grantsMatrix[name]; !ok {
			t.Errorf("table %s is not in the grants matrix: decide its privileges in a migration "+
				"(see 0021_roles.sql) and add it to grantsMatrix", name)
		}
	}
	for name, g := range grantsMatrix {
		own, ok := owners[name]
		if !ok {
			t.Errorf("grants matrix names %s, which does not exist", name)
			continue
		}
		wantOwner := ownerRole
		if g.workerOwns {
			wantOwner = db.WorkerRole
		}
		if own != wantOwner {
			t.Errorf("%s is owned by %s, want %s", name, own, wantOwner)
		}
		worker := g.worker
		if g.workerOwns {
			worker = allPrivileges
		}
		checkPrivileges(t, env.owner, name, db.WorkerRole, worker, g.workerCols)
		checkPrivileges(t, env.owner, name, db.APIRole, g.api, g.apiCols)
	}
	// Sequences: usable exactly by the roles that may insert into their table.
	seqRows, err := env.owner.Query(roleCtx, `SELECT seq.relname, tbl.relname,
			has_sequence_privilege($1, seq.oid, 'USAGE'), has_table_privilege($1, tbl.oid, 'INSERT'),
			has_sequence_privilege($2, seq.oid, 'USAGE'), has_table_privilege($2, tbl.oid, 'INSERT')
		FROM pg_class seq
		JOIN pg_depend d ON d.objid = seq.oid AND d.classid = 'pg_class'::regclass AND d.deptype IN ('a', 'i')
		JOIN pg_class tbl ON tbl.oid = d.refobjid
		WHERE seq.relkind = 'S' AND seq.relnamespace = (SELECT oid FROM pg_namespace WHERE nspname = current_schema())`,
		db.WorkerRole, db.APIRole)
	if err != nil {
		t.Fatal(err)
	}
	for seqRows.Next() {
		var seq, tbl string
		var wUse, wIns, aUse, aIns bool
		if err := seqRows.Scan(&seq, &tbl, &wUse, &wIns, &aUse, &aIns); err != nil {
			t.Fatal(err)
		}
		if wUse != wIns || aUse != aIns {
			t.Errorf("sequence %s of %s: worker usage %v (insert %v), api usage %v (insert %v)", seq, tbl, wUse, wIns, aUse, aIns)
		}
	}
	if err := seqRows.Err(); err != nil {
		t.Fatal(err)
	}
	// Role attributes: no way around the privileges.
	for _, role := range []string{db.WorkerRole, db.APIRole} {
		var super, createRole, createDB, bypass, repl, login bool
		if err := env.owner.QueryRow(roleCtx, `SELECT rolsuper, rolcreaterole, rolcreatedb, rolbypassrls, rolreplication, rolcanlogin
			FROM pg_roles WHERE rolname = $1`, role).Scan(&super, &createRole, &createDB, &bypass, &repl, &login); err != nil {
			t.Fatal(err)
		}
		if super || createRole || createDB || bypass || repl || !login {
			t.Errorf("%s: super %v createrole %v createdb %v bypassrls %v replication %v login %v", role, super, createRole, createDB, bypass, repl, login)
		}
	}
}

func checkPrivileges(t *testing.T, owner *pgxpool.Pool, table, role string, want, wantCols []string) {
	t.Helper()
	for _, priv := range allPrivileges {
		var has bool
		if err := owner.QueryRow(roleCtx, `SELECT has_table_privilege($1, $2::regclass, $3)`, role, table, priv).Scan(&has); err != nil {
			t.Fatal(err)
		}
		if has != slices.Contains(want, priv) {
			t.Errorf("%s on %s: %s = %v, want %v", role, table, priv, has, !has)
		}
	}
	if slices.Contains(want, "UPDATE") {
		return
	}
	rows, err := owner.Query(roleCtx, `SELECT attname FROM pg_attribute
		WHERE attrelid = $2::regclass AND attnum > 0 AND NOT attisdropped
			AND has_column_privilege($1, $2::regclass, attnum, 'UPDATE') ORDER BY attname`, role, table)
	if err != nil {
		t.Fatal(err)
	}
	cols, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	sortedWant := slices.Clone(wantCols)
	slices.Sort(sortedWant)
	if !slices.Equal(cols, sortedWant) && !(len(cols) == 0 && len(sortedWant) == 0) {
		t.Errorf("%s may UPDATE columns %v of %s, want %v", role, cols, table, sortedWant)
	}
}

// TestWorkerRole runs the worker's own write paths as geoint_worker and
// checks what it can never do.
func TestWorkerRole(t *testing.T) {
	env := newRoleEnv(t)
	w := env.worker
	receita, _ := sources.ByKey("receita")

	// Cannot remove or hide claims, nor switch the rules off.
	denied(t, w, `DELETE FROM relationship`)
	denied(t, w, `ALTER TABLE relationship DISABLE TRIGGER ALL`)
	denied(t, w, `ALTER TABLE relationship DISABLE TRIGGER relationship_no_delete`)
	denied(t, w, `TRUNCATE entity`)
	denied(t, w, `DROP TABLE relationship`)
	denied(t, w, `CREATE TRIGGER x BEFORE UPDATE ON relationship FOR EACH ROW EXECUTE FUNCTION forbid_update()`)
	denied(t, w, `SET session_replication_role = replica`)
	denied(t, w, `ALTER TABLE collection OWNER TO geoint_worker`)
	denied(t, w, `DROP FUNCTION forbid_delete() CASCADE`)
	denied(t, w, `SELECT * FROM audit_log`)
	for _, tbl := range claimTables {
		denied(t, w, `DELETE FROM `+tbl)
		denied(t, w, `TRUNCATE `+tbl)
	}

	// Staging: truncated before and after every load.
	for name, g := range grantsMatrix {
		if g.workerOwns && (strings.HasPrefix(name, "receita_stg_") || strings.HasPrefix(name, "tse_stg_")) {
			allowed(t, w, `TRUNCATE `+name)
		}
	}
	allowed(t, w, `ANALYZE entity`)
	allowed(t, w, `ANALYZE relationship`)

	// A load as the worker: collection, records, relationships, a company
	// version, the close of what the next collection did not see, identity
	// resolution and two search rebuilds.
	store := claims.NewStore(w)
	col1 := beginDone(t, w, receita.ID, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	var rels []claims.RelationshipInput
	var companies []int64
	for i := range 10 {
		company, err := store.UpsertEntity(roleCtx, claims.KindCompany, fmt.Sprintf("%08d", 100+i), fmt.Sprintf("EMPRESA %d", i))
		if err != nil {
			t.Fatal(err)
		}
		prov := claims.Provenance{CollectionID: col1.ID, RecordRef: fmt.Sprintf("r%d", i)}
		if err := store.PutCompanyVersion(roleCtx, claims.CompanyVersionInput{EntityID: company, Name: "EMPRESA", Prov: prov}); err != nil {
			t.Fatalf("company version: %v", err)
		}
		rec, ent, err := store.UpsertPersonRecord(roleCtx, claims.PersonRecordInput{SourceID: receita.ID,
			ContextKey: fmt.Sprintf("%08d", 100+i), Name: "MARIA DA SILVA", CPFFragment: "123456", Prov: prov})
		if err != nil {
			t.Fatalf("person record: %v", err)
		}
		in := claims.RelationshipInput{Type: claims.PartnerOf, SrcEntityID: ent, DstEntityID: company, SrcRecordID: &rec,
			Kind: claims.Fact, Prov: prov}
		if _, err := store.AssertRelationship(roleCtx, in); err != nil {
			t.Fatalf("relationship: %v", err)
		}
		rels = append(rels, in)
		companies = append(companies, company)
	}
	finish(t, w, col1.ID)
	resolver := &identity.Resolver{Pool: w, CommonNameThreshold: 100}
	if _, err := resolver.Resolve(roleCtx, col1.ID); err != nil {
		t.Fatalf("resolve as worker: %v", err)
	}
	col2 := beginDone(t, w, receita.ID, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	for i, in := range rels[:9] {
		in.Prov.CollectionID = col2.ID
		if err := store.PutCompanyVersion(roleCtx, claims.CompanyVersionInput{EntityID: companies[i], Name: "EMPRESA",
			Prov: in.Prov}); err != nil {
			t.Fatalf("company version seen again: %v", err)
		}
		if _, err := store.AssertRelationship(roleCtx, in); err != nil {
			t.Fatalf("relationship seen again: %v", err)
		}
	}
	closed, err := store.CloseUnseen(roleCtx, receita, col2, 0.9)
	// One relationship and one company version were not seen again.
	if err != nil || closed != 2 {
		t.Fatalf("close unseen as worker: closed %d, err %v", closed, err)
	}
	finish(t, w, col2.ID)
	if _, err := resolver.Resolve(roleCtx, col2.ID); err != nil {
		t.Fatalf("resolve as worker: %v", err)
	}
	if _, err := collection.FailDead(roleCtx, w); err != nil {
		t.Fatalf("fail dead runs as worker: %v", err)
	}

	// Job sweeper and retention (jobs.RunWorker).
	workerJobs := jobs.New(w, testutil.Queue(t))
	if _, _, err := workerJobs.Sweep(roleCtx); err != nil {
		t.Fatalf("job sweep as worker: %v", err)
	}
	if _, err := workerJobs.Purge(roleCtx); err != nil {
		t.Fatalf("job purge as worker: %v", err)
	}

	srch := search.New(w)
	for i := range 2 {
		if _, err := srch.Rebuild(roleCtx); err != nil {
			t.Fatalf("search rebuild %d as worker: %v", i+1, err)
		}
	}
	// The tables the rebuild created belong to the worker and are readable by the API.
	for _, tbl := range []string{"search_company", "search_person", "search_other", "search_frequent_token"} {
		var own string
		if err := env.owner.QueryRow(roleCtx, `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = $1::regclass`, tbl).Scan(&own); err != nil {
			t.Fatal(err)
		}
		if own != db.WorkerRole {
			t.Errorf("%s owned by %s after a rebuild, want %s", tbl, own, db.WorkerRole)
		}
		allowed(t, env.api, `SELECT count(*) FROM `+tbl)
		denied(t, env.api, `DELETE FROM `+tbl)
	}
	found, err := search.New(env.api).Query(roleCtx, "EMPRESA 3", claims.KindCompany, 10)
	if err != nil || len(found) == 0 {
		t.Fatalf("search as api after a worker rebuild: %v, %v", found, err)
	}
}

// TestAPIRole runs every write the API performs as geoint_api (identity
// decisions, audit, collection requests, jobs) and checks that it cannot
// write claims any other way.
func TestAPIRole(t *testing.T) {
	env := newRoleEnv(t)
	a := env.api
	receita, _ := sources.ByKey("receita")

	// Fixture, as the owner: three records of one name with two fragments
	// (one proposal at threshold 1) and two loose records to merge and split.
	ownerStore := claims.NewStore(env.owner)
	col := beginDone(t, env.owner, receita.ID, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	person := func(ctxKey, name, frag string) int64 {
		prov := claims.Provenance{CollectionID: col.ID, RecordRef: ctxKey + name + frag}
		rec, ent, err := ownerStore.UpsertPersonRecord(roleCtx, claims.PersonRecordInput{SourceID: receita.ID,
			ContextKey: ctxKey, Name: name, CPFFragment: frag, Prov: prov})
		if err != nil {
			t.Fatal(err)
		}
		company, err := ownerStore.UpsertEntity(roleCtx, claims.KindCompany, "c"+ctxKey, "EMPRESA "+ctxKey)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ownerStore.AssertRelationship(roleCtx, claims.RelationshipInput{Type: claims.PartnerOf,
			SrcEntityID: ent, DstEntityID: company, SrcRecordID: &rec, Kind: claims.Fact, Prov: prov}); err != nil {
			t.Fatal(err)
		}
		return rec
	}
	person("01", "JOAO PEREIRA", "111111")
	person("02", "JOAO PEREIRA", "111111")
	person("06", "JOAO PEREIRA", "222222")
	r1 := person("03", "ANA LIMA", "")
	r2 := person("04", "ANA LIMA", "")
	r3 := person("05", "ANA LIMA", "")
	finish(t, env.owner, col.ID)
	if _, err := (&identity.Resolver{Pool: env.owner, CommonNameThreshold: 1}).Resolve(roleCtx, col.ID); err != nil {
		t.Fatal(err)
	}

	// The identity package's legitimate paths, as the API.
	api := &identity.Resolver{Pool: a}
	if _, err := api.Merge(roleCtx, []int64{r1, r2, r3}, "analyst-1", "same person"); err != nil {
		t.Fatalf("merge as api: %v", err)
	}
	if _, err := api.Split(roleCtx, []int64{r3}, "analyst-1", "not the same"); err != nil {
		t.Fatalf("split as api: %v", err)
	}
	pending, err := api.Pending(roleCtx, 10)
	if err != nil || len(pending) == 0 {
		t.Fatalf("pending proposals: %v, %v", pending, err)
	}
	if err := api.Decide(roleCtx, pending[0].ID, false, "analyst-1"); err != nil {
		t.Fatalf("reject as api: %v", err)
	}
	if err := audit.Log(roleCtx, a, "analyst-1", "test.action", nil, map[string]any{"k": "v"}); err != nil {
		t.Fatalf("audit as api: %v", err)
	}
	pub := &recordingPublisher{}
	err = freshness.New(a, time.Now).Request(roleCtx, pub, receita,
		connector.Request{ReferenceDate: time.Now().UTC().Truncate(24 * time.Hour), Scope: collection.Scope{Full: true}}, "analyst-1")
	if err != nil || pub.n != 1 {
		t.Fatalf("collection request as api: published %d, err %v", pub.n, err)
	}
	q := testutil.Queue(t)
	jobSvc := jobs.New(a, q)
	jobSvc.Register("noop", func(context.Context, json.RawMessage, func(float64)) (any, error) { return nil, nil })
	id, err := jobSvc.Enqueue(roleCtx, "noop", map[string]any{}, "analyst-1")
	if err != nil {
		t.Fatalf("enqueue as api: %v", err)
	}
	// The enqueue-failure path of jobs.Enqueue.
	allowed(t, a, `UPDATE job SET status = 'failed', error = 'could not enqueue', finished_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'queued'`, id)

	// Nothing else.
	denied(t, a, `INSERT INTO relationship (rel_type) VALUES ('partner_of')`)
	denied(t, a, `UPDATE relationship SET valid_to_before = now()`)
	denied(t, a, `UPDATE relationship SET closed_collection_id = $1`, col.ID)
	denied(t, a, `UPDATE person_record SET name = 'X'`)
	denied(t, a, `INSERT INTO person_record (source_id) VALUES (1)`)
	denied(t, a, `UPDATE company_version SET closed_collection_id = $1`, col.ID)
	denied(t, a, `INSERT INTO collection (source_id) VALUES (1)`)
	denied(t, a, `UPDATE collection SET status = 'failed'`)
	denied(t, a, `INSERT INTO collection_file (collection_id) VALUES ($1)`, col.ID)
	denied(t, a, `UPDATE job SET params = '{}'`)
	denied(t, a, `UPDATE search_state SET generation = 0`)
	denied(t, a, `SELECT * FROM audit_log`)
	denied(t, a, `SELECT * FROM receita_stg_file`)
	denied(t, a, `TRUNCATE receita_stg_file`)
	denied(t, a, `CREATE TABLE x (id int)`)
	denied(t, a, `ALTER TABLE relationship DISABLE TRIGGER ALL`)
	denied(t, a, `SET session_replication_role = replica`)
	for _, tbl := range claimTables {
		denied(t, a, `DELETE FROM `+tbl)
		denied(t, a, `TRUNCATE `+tbl)
	}
}

type recordingPublisher struct{ n int }

func (p *recordingPublisher) Publish(context.Context, string, any) error { p.n++; return nil }

func beginDone(t *testing.T, p *pgxpool.Pool, source int16, ref time.Time) collection.Collection {
	t.Helper()
	c, err := collection.Begin(roleCtx, p, source, ref, collection.Scope{Full: true})
	if err != nil {
		t.Fatalf("begin collection: %v", err)
	}
	return c
}

func finish(t *testing.T, p *pgxpool.Pool, id int64) {
	t.Helper()
	if err := collection.Finish(roleCtx, p, id, map[string]int64{"rows_read": 1, "rows_skipped": 0}); err != nil {
		t.Fatalf("finish collection: %v", err)
	}
}

// TestSetRolePasswordsQuoting: a password with quotes, backslashes and
// SQL-looking text is stored as given and works for login; invalid ones are
// refused before anything reaches the server.
func TestSetRolePasswordsQuoting(t *testing.T) {
	base := testutil.EmptySchemaURL(t)
	owner, err := db.Open(roleCtx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	role := "geoint_pwtest_" + hex.EncodeToString(b)
	if _, err := owner.Exec(roleCtx, `CREATE ROLE `+role+` NOLOGIN`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(context.Background(), `DROP ROLE `+role) })
	pw := `a'b"c\d$e%f;g--h'OR'1'='1\\';DROP-ROLE`
	if err := db.SetRolePasswords(roleCtx, owner, map[string]string{role: pw}); err != nil {
		t.Fatalf("set password: %v", err)
	}
	conn := openAs(t, base, role, pw)
	conn.Close()
	// Stored as a SCRAM verifier, never in clear.
	var stored string
	if err := owner.QueryRow(roleCtx, `SELECT rolpassword FROM pg_authid WHERE rolname = $1`, role).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, "SCRAM-SHA-256$4096:") || strings.Contains(stored, pw) {
		t.Errorf("stored password is not a SCRAM verifier: %.20q…", stored)
	}
	u, _ := url.Parse(base)
	u.User = url.UserPassword(role, pw+"x")
	if c, err := pgx.Connect(roleCtx, u.String()); err == nil {
		c.Close(roleCtx)
		t.Error("login with a wrong password succeeded")
	}
	for _, bad := range []string{"short", "has a space in it ok", "naïve-password-1234", "tab\tin-password-xx"} {
		if err := db.SetRolePasswords(roleCtx, owner, map[string]string{role: bad}); err == nil {
			t.Errorf("password %q accepted", bad)
		}
	}
}
