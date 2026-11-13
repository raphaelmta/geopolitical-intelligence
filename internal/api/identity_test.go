package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"geoint/internal/graph/graphtest"
)

type proposalPage struct {
	Items []struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Status      string `json:"status"`
		Connections int    `json:"connections"`
		Reason      string `json:"reason"`
		ReasonCode  string `json:"reason_code"`
		Records     []struct {
			ID         int64  `json:"id"`
			EntityID   int64  `json:"entity_id"`
			SourceKey  string `json:"source_key"`
			ContextKey string `json:"context_key"`
			CPFMasked  string `json:"cpf_masked"`
		} `json:"records"`
		TargetEntity struct {
			Entity       entityRef `json:"entity"`
			OtherRecords []struct {
				ID int64 `json:"id"`
			} `json:"other_records"`
		} `json:"target_entity"`
	} `json:"items"`
	NextCursor string `json:"next_cursor"`
}

// proposalFixture writes two records of one invented person in two companies
// (two entities, the second also holding a third record) and a pending
// proposal to join the first two.
func proposalFixture(e *env) (proposal, r1, e1, r2, e2, r3 int64) {
	e.t.Helper()
	b, rc := graphtest.FreshReceita(e.t, e.pool)
	r1, e1 = b.Person(rc, "60000001", "PESSOA REPETIDA FICTICIA", "***777777**")
	r2, e2 = b.Person(rc, "60000002", "PESSOA REPETIDA FICTICIA", "***777777**")
	r3, _ = b.Person(rc, "60000003", "OUTRO NOME FICTICIO", "***888888**")
	if _, err := e.pool.Exec(bg, `UPDATE person_record SET entity_id = $1 WHERE id = $2`, e1, r3); err != nil {
		e.t.Fatal(err)
	}
	if err := e.pool.QueryRow(bg, `INSERT INTO identity_proposal (group_key, record_ids, reason, connections)
		VALUES ('PESSOA REPETIDA FICTICIA|777777', $1, 'test proposal', 3) RETURNING id`, []int64{r1, r2}).Scan(&proposal); err != nil {
		e.t.Fatal(err)
	}
	return proposal, r1, e1, r2, e2, r3
}

func TestProposalDecisionAppliesMerge(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	prop, r1, e1, r2, _, r3 := proposalFixture(e)

	rec := e.do("GET", "/api/v1/identity/proposals", tokAnalyst, "")
	page := want[proposalPage](t, rec, http.StatusOK)
	if len(page.Items) != 1 || page.NextCursor != "" {
		t.Fatalf("proposals %+v", page)
	}
	it := page.Items[0]
	if it.ID != prop || it.Status != "pending" || it.Connections != 3 || it.Reason != "test proposal" || it.ReasonCode != "other" ||
		it.Name != "PESSOA REPETIDA FICTICIA" || len(it.Records) != 2 || it.Records[0].CPFMasked != "***777777**" {
		t.Fatalf("proposal %+v", it)
	}
	if it.TargetEntity.Entity.ID != e1 || len(it.TargetEntity.OtherRecords) != 1 || it.TargetEntity.OtherRecords[0].ID != r3 {
		t.Fatalf("target %+v, want entity %d with record %d", it.TargetEntity, e1, r3)
	}
	if strings.Contains(rec.Body.String(), "group_key") || strings.Contains(rec.Body.String(), "|777777") {
		t.Fatalf("group key leaked: %s", rec.Body.String())
	}

	want[any](t, e.do("POST", fmt.Sprintf("/api/v1/identity/proposals/%d/decision", prop), tokAnalyst, `{"confirm":true}`), http.StatusNoContent)
	var a, b int64
	if err := e.pool.QueryRow(bg, `SELECT (SELECT entity_id FROM person_record WHERE id = $1), (SELECT entity_id FROM person_record WHERE id = $2)`,
		r1, r2).Scan(&a, &b); err != nil {
		t.Fatal(err)
	}
	if a != e1 || b != e1 {
		t.Fatalf("records in entities %d and %d, want both in %d", a, b, e1)
	}
	var detail, user string
	if err := e.pool.QueryRow(bg, `SELECT user_sub, detail::text FROM audit_log WHERE action = 'identity.decide'`).Scan(&user, &detail); err != nil {
		t.Fatal(err)
	}
	if user != "sub-analyst" || !strings.Contains(detail, fmt.Sprint(prop)) {
		t.Fatalf("audit %s %s", user, detail)
	}
	// Decided already: conflict. No pending proposals left.
	wantError(t, e.do("POST", fmt.Sprintf("/api/v1/identity/proposals/%d/decision", prop), tokAnalyst, `{"confirm":false}`),
		http.StatusConflict, "conflict")
	if page := want[proposalPage](t, e.do("GET", "/api/v1/identity/proposals", tokAnalyst, ""), http.StatusOK); len(page.Items) != 0 {
		t.Fatalf("pending after decision: %+v", page.Items)
	}
	if page := want[proposalPage](t, e.do("GET", "/api/v1/identity/proposals?status=confirmed", tokAnalyst, ""), http.StatusOK); len(page.Items) != 1 {
		t.Fatalf("confirmed: %+v", page.Items)
	}
}

func TestProposalsPaginate(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	b, rc := graphtest.FreshReceita(t, e.pool)
	// The resolver's reasons (internal/identity/resolver.go) and their codes.
	reasons := [3][2]string{
		{"150 distinct CPF fragments share this name (automatic merge up to 100)", "common_name"},
		{"the records would join an entity an analyst built", "impure_target"},
		{"an analyst split records with this name and CPF fragment apart before", "ambiguous_key"},
	}
	for i := range 55 {
		r1, _ := b.Person(rc, fmt.Sprintf("5%07d", i), fmt.Sprintf("PESSOA FICTICIA %02d", i), "***121212**")
		r2, _ := b.Person(rc, fmt.Sprintf("4%07d", i), fmt.Sprintf("PESSOA FICTICIA %02d", i), "***121212**")
		if _, err := e.pool.Exec(bg, `INSERT INTO identity_proposal (group_key, record_ids, reason, connections)
			VALUES ($1, $2, $4, $3)`, fmt.Sprintf("K%d", i), []int64{r1, r2}, i%3, reasons[i%3][0]); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[int64]bool{}
	cursor, pages := "", 0
	for {
		path := "/api/v1/identity/proposals"
		if cursor != "" {
			path += "?cursor=" + cursor
		}
		page := want[proposalPage](t, e.do("GET", path, tokAnalyst, ""), http.StatusOK)
		pages++
		last := 1 << 30
		for _, it := range page.Items {
			if seen[it.ID] || it.Connections > last {
				t.Fatalf("repeat or out of order: %+v", it)
			}
			seen[it.ID], last = true, it.Connections
			if r := reasons[it.Connections]; it.Reason != r[0] || it.ReasonCode != r[1] {
				t.Errorf("proposal %d: reason %q code %q, want %q", it.ID, it.Reason, it.ReasonCode, r[1])
			}
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if len(seen) != 55 || pages != 2 {
		t.Fatalf("%d proposals over %d pages", len(seen), pages)
	}
}

func TestMergeAndSplit(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	_, r1, e1, r2, _, _ := proposalFixture(e)
	got := want[map[string]int64](t, e.do("POST", "/api/v1/identity/merge", tokAnalyst,
		fmt.Sprintf(`{"record_ids":[%d,%d],"reason":"mesma pessoa"}`, r2, r1)), http.StatusOK)
	if got["entity_id"] != e1 {
		t.Fatalf("merge: %v, want entity %d", got, e1)
	}
	got = want[map[string]int64](t, e.do("POST", "/api/v1/identity/split", tokAnalyst,
		fmt.Sprintf(`{"record_ids":[%d],"reason":"pessoas diferentes"}`, r2)), http.StatusOK)
	if got["entity_id"] == 0 || got["entity_id"] == e1 {
		t.Fatalf("split: %v", got)
	}
	for _, action := range []string{"identity.merge", "identity.split"} {
		var detail json.RawMessage
		if err := e.pool.QueryRow(bg, `SELECT detail FROM audit_log WHERE action = $1`, action).Scan(&detail); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		if strings.Contains(string(detail), "pessoa") {
			t.Fatalf("%s audit has the free-text reason: %s", action, detail)
		}
	}
	wantError(t, e.do("POST", "/api/v1/identity/merge", tokAnalyst, `{"record_ids":[987654,987655],"reason":"x"}`),
		http.StatusNotFound, "not_found")
}

// entityOf returns the entity of a person record.
func (e *env) entityOf(rec int64) int64 {
	e.t.Helper()
	var id int64
	if err := e.pool.QueryRow(bg, `SELECT entity_id FROM person_record WHERE id = $1`, rec).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// auditDetail returns the detail of the only audit row of action.
func (e *env) auditDetail(action string) map[string]any {
	e.t.Helper()
	var raw []byte
	if err := e.pool.QueryRow(bg, `SELECT detail FROM audit_log WHERE action = $1 ORDER BY id DESC LIMIT 1`, action).Scan(&raw); err != nil {
		e.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		e.t.Fatal(err)
	}
	return m
}

func TestProposalRecordsSayWhichAreFree(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	_, r1, _, r2, _, _ := proposalFixture(e)
	// r2 is split away by an analyst: locked, it will not be moved.
	want[map[string]int64](t, e.do("POST", "/api/v1/identity/split", tokAnalyst,
		fmt.Sprintf(`{"record_ids":[%d],"reason":"outra pessoa"}`, r2)), http.StatusOK)
	type freePage struct {
		Items []struct {
			Records []struct {
				ID     int64 `json:"id"`
				Free   bool  `json:"free"`
				Locked bool  `json:"locked"`
			} `json:"records"`
		} `json:"items"`
	}
	page := want[freePage](t, e.do("GET", "/api/v1/identity/proposals", tokAnalyst, ""), http.StatusOK)
	if len(page.Items) != 1 || len(page.Items[0].Records) != 2 {
		t.Fatalf("page %+v", page)
	}
	for _, rec := range page.Items[0].Records {
		if wantFree := rec.ID == r1; rec.Free != wantFree || rec.Locked == wantFree {
			t.Errorf("record %+v, want free=%v", rec, wantFree)
		}
	}
}

func TestProposalDecisionExpectations(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	prop, r1, e1, r2, e2, _ := proposalFixture(e)
	path := fmt.Sprintf("/api/v1/identity/proposals/%d/decision", prop)

	// Malformed expectations: 400 before anything is audited.
	for _, body := range []string{
		`{"confirm":true,"expected_target_entity_id":"x"}`,
		`{"confirm":true,"expected_target_entity_id":0}`,
		`{"confirm":true,"expected_record_ids":[0]}`,
	} {
		wantError(t, e.do("POST", path, tokAnalyst, body), http.StatusBadRequest, "bad_request")
	}
	if n := e.auditCount("identity.decide", nil); n != 0 {
		t.Fatalf("%d audit rows for malformed bodies", n)
	}

	// The screen showed e2 as the target: the data changed, nothing is done.
	wantError(t, e.do("POST", path, tokAnalyst,
		fmt.Sprintf(`{"confirm":true,"expected_target_entity_id":%d,"expected_record_ids":[%d,%d]}`, e2, r1, r2)),
		http.StatusConflict, "conflict")
	if n := e.auditCount("identity.decide", nil); n != 1 {
		t.Fatalf("%d audit rows, want the attempt", n)
	}
	d := e.auditDetail("identity.decide")
	if d["expected_target_entity_id"] != float64(e2) || fmt.Sprint(d["expected_record_ids"]) != fmt.Sprint([]any{float64(r1), float64(r2)}) {
		t.Fatalf("audit detail %v", d)
	}
	if e.count(`SELECT count(*) FROM identity_decision`) != 0 || e.entityOf(r2) != e2 ||
		e.count(`SELECT count(*) FROM identity_proposal WHERE status = 'pending'`) != 1 {
		t.Fatal("a refused decision changed something")
	}
	// A record set that is not the free one: 409.
	wantError(t, e.do("POST", path, tokAnalyst, fmt.Sprintf(`{"confirm":false,"expected_record_ids":[%d]}`, r1)),
		http.StatusConflict, "conflict")

	// What the screen shows: done.
	want[any](t, e.do("POST", path, tokAnalyst,
		fmt.Sprintf(`{"confirm":true,"expected_target_entity_id":%d,"expected_record_ids":[%d,%d]}`, e1, r2, r1)),
		http.StatusNoContent)
	if e.entityOf(r2) != e1 {
		t.Fatal("not merged")
	}
}

func TestMergeAndSplitExpectations(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	_, r1, e1, r2, e2, r3 := proposalFixture(e) // e1 = {r1, r3}, e2 = {r2}

	for _, tc := range []struct{ path, body string }{
		{"merge", fmt.Sprintf(`{"record_ids":[%d,%d],"reason":"x","entity_ids":[]}`, r1, r2)},
		{"merge", fmt.Sprintf(`{"record_ids":[%d,%d],"reason":"x","entity_ids":[1,2,3,4,5,6,7,8,9,10,11]}`, r1, r2)},
		{"merge", fmt.Sprintf(`{"record_ids":[%d,%d],"reason":"x","entity_ids":[0]}`, r1, r2)},
		{"merge", fmt.Sprintf(`{"record_ids":[%d,%d],"reason":"x","entity_id":%d}`, r1, r2, e1)},
		{"split", fmt.Sprintf(`{"record_ids":[%d],"reason":"x","entity_ids":[%d]}`, r1, e1)},
		{"split", fmt.Sprintf(`{"record_ids":[%d],"reason":"x","entity_id":0}`, r1)},
	} {
		wantError(t, e.do("POST", "/api/v1/identity/"+tc.path, tokAnalyst, tc.body), http.StatusBadRequest, "bad_request")
	}

	// r3 is in e1 and missing from record_ids: 409, audited, nothing done.
	wantError(t, e.do("POST", "/api/v1/identity/merge", tokAnalyst,
		fmt.Sprintf(`{"record_ids":[%d,%d],"reason":"x","entity_ids":[%d,%d]}`, r1, r2, e1, e2)), http.StatusConflict, "conflict")
	if n := e.auditCount("identity.merge", nil); n != 1 {
		t.Fatalf("%d merge audit rows", n)
	}
	if d := e.auditDetail("identity.merge"); fmt.Sprint(d["entity_ids"]) != fmt.Sprint([]any{float64(e1), float64(e2)}) {
		t.Fatalf("audit detail %v", d)
	}
	if e.count(`SELECT count(*) FROM identity_decision`) != 0 || e.entityOf(r2) != e2 {
		t.Fatal("a refused merge changed something")
	}
	got := want[map[string]int64](t, e.do("POST", "/api/v1/identity/merge", tokAnalyst,
		fmt.Sprintf(`{"record_ids":[%d,%d,%d],"reason":"x","entity_ids":[%d,%d]}`, r1, r2, r3, e2, e1)), http.StatusOK)
	if got["entity_id"] != e1 || e.entityOf(r2) != e1 {
		t.Fatalf("merge %v", got)
	}

	// Split: the records are no longer in e2; and all of e1 cannot be split off.
	wantError(t, e.do("POST", "/api/v1/identity/split", tokAnalyst,
		fmt.Sprintf(`{"record_ids":[%d],"reason":"x","entity_id":%d}`, r2, e2)), http.StatusConflict, "conflict")
	wantError(t, e.do("POST", "/api/v1/identity/split", tokAnalyst,
		fmt.Sprintf(`{"record_ids":[%d,%d,%d],"reason":"x","entity_id":%d}`, r1, r2, r3, e1)), http.StatusConflict, "conflict")
	if d := e.auditDetail("identity.split"); d["entity_id"] != float64(e1) {
		t.Fatalf("audit detail %v", d)
	}
	got = want[map[string]int64](t, e.do("POST", "/api/v1/identity/split", tokAnalyst,
		fmt.Sprintf(`{"record_ids":[%d],"reason":"x","entity_id":%d}`, r2, e1)), http.StatusOK)
	if got["entity_id"] == e1 || e.entityOf(r2) != got["entity_id"] {
		t.Fatalf("split %v", got)
	}
}
