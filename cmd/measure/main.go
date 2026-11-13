// Command measure times the platform THROUGH ITS AUTHENTICATED API, the way
// a client uses it (scripts/measure.sh, make measure): search (M1), entity
// page, neighbours (M2), paths (M3) and control group (M4) as background
// jobs. It is not part of the geoint binary.
//
// Each run creates a temporary Keycloak user with role analyst (admin REST
// API, bootstrap admin from the environment), logs in with authorization
// code + PKCE S256 against the application's public client, measures
// sequentially, and deletes the user at the end (also on Ctrl-C). Tokens
// and passwords are never printed or written.
//
// Inputs are entity ids chosen by scripts/measure.sh with deterministic SQL
// (files in -samples). Outputs (-out): <pass>.json with every timing and the
// ids measured, <pass>.csv, and <pass>.md, a summary. No names, CPFs or
// other values read from the data are written: sampled search terms are
// recorded as "sampled:<entity id>", CNPJ lookups as "cnpj:<entity id>".
//
//	measure run -samples DIR -out DIR -pass cold
//	measure probe -a ID -b ID         one paths job, timed (starvation check)
//	measure criteria -cnpj N -a ID -b ID   success-criteria walk (counts, kinds, ids)
//	measure provenance -id N          provenance of one relationship
//	measure cleanup                   deletes the users this tool created and recorded in
//	                                  MEASURE_STATE but could not delete (never by name)
//
// Environment: KC_URL (http://localhost:8081), API_URL
// (http://localhost:8080), KC_ADMIN, KC_ADMIN_PASSWORD, KC_REALM (geoint),
// KC_CLIENT (geoint-web), KC_REDIRECT (http://localhost:5173/callback),
// MEASURE_STATE (file of created users not deleted yet). Exit status 1 when
// anything failed.
package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("measure: ")
	if len(os.Args) < 2 {
		log.Fatal("usage: measure run|probe|cleanup [flags]")
	}
	k := &keycloak{
		base: strings.TrimSuffix(env("KC_URL", "http://localhost:8081"), "/"), realm: env("KC_REALM", "geoint"),
		client: env("KC_CLIENT", "geoint-web"), redirect: env("KC_REDIRECT", "http://localhost:5173/callback"),
		adminUser: os.Getenv("KC_ADMIN"), adminPassword: os.Getenv("KC_ADMIN_PASSWORD"),
		http: &http.Client{Timeout: 30 * time.Second},
	}
	if os.Args[1] != "render" && (k.adminUser == "" || k.adminPassword == "") {
		log.Fatal("KC_ADMIN and KC_ADMIN_PASSWORD must be set (from the settings file; never on the command line)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	apiURL := strings.TrimSuffix(env("API_URL", "http://localhost:8080"), "/")

	switch os.Args[1] {
	case "cleanup":
		// Deletes only the users this tool created and recorded in the
		// state file (MEASURE_STATE) and did not delete yet: a run killed
		// before its own delete. Never by name: another run's user, or the
		// owner's, is never touched.
		left, err := readState()
		if err != nil {
			log.Fatal(err)
		}
		failed := 0
		for _, u := range left {
			if err := k.deleteUser(context.WithoutCancel(ctx), u.ID); err != nil {
				log.Printf("delete %s: %v", u.Username, err)
				failed++
				continue
			}
			forget(u.ID)
			log.Printf("deleted leftover user %s", u.Username)
		}
		log.Printf("leftover temporary users of this tool: %d deleted, %d left", len(left)-failed, failed)
		if failed > 0 {
			os.Exit(1)
		}
	case "probe":
		fs := flag.NewFlagSet("probe", flag.ExitOnError)
		a := fs.Int64("a", 0, "entity id")
		b := fs.Int64("b", 0, "entity id")
		_ = fs.Parse(os.Args[2:])
		exit(withUser(ctx, k, apiURL, func(c *client) error {
			r := c.job(ctx, "paths", map[string]any{"a": *a, "b": *b, "max_depth": 6}, "probe")
			out, _ := json.Marshal(r)
			fmt.Println(string(out))
			return nil
		}))
	case "criteria":
		fs := flag.NewFlagSet("criteria", flag.ExitOnError)
		cnpj := fs.String("cnpj", "33000167", "CNPJ (8 digits) of a large public company")
		a := fs.Int64("a", 0, "path end: a public office holder")
		b := fs.Int64("b", 0, "path end: a company")
		_ = fs.Parse(os.Args[2:])
		exit(withUser(ctx, k, apiURL, func(c *client) error {
			criteria(ctx, c, *cnpj, *a, *b)
			return nil
		}))
	case "provenance":
		fs := flag.NewFlagSet("provenance", flag.ExitOnError)
		id := fs.Int64("id", 0, "relationship id")
		_ = fs.Parse(os.Args[2:])
		exit(withUser(ctx, k, apiURL, func(c *client) error {
			c.provenance(ctx, *id)
			return nil
		}))
	case "render":
		// Re-renders <pass>.md from <pass>.json (after a change of the summary).
		fs := flag.NewFlagSet("render", flag.ExitOnError)
		in := fs.String("in", "", "a <pass>.json written by run")
		_ = fs.Parse(os.Args[2:])
		b, err := os.ReadFile(*in)
		if err != nil {
			log.Fatal(err)
		}
		var r results
		if err := json.Unmarshal(b, &r); err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(strings.TrimSuffix(*in, ".json")+".md", []byte(r.markdown()), 0o644); err != nil {
			log.Fatal(err)
		}
		return
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		samples := fs.String("samples", "", "folder with the sample id files")
		outDir := fs.String("out", "", "output folder")
		pass := fs.String("pass", "run", "name of this pass (cold, warm, ...)")
		only := fs.String("only", "", "comma-separated subset: m1,entity,m2,m3,m4 (default all)")
		_ = fs.Parse(os.Args[2:])
		if *samples == "" || *outDir == "" {
			log.Fatal("run: -samples and -out are required")
		}
		s, err := readSamples(*samples)
		if err != nil {
			log.Fatal(err)
		}
		exit(withUser(ctx, k, apiURL, func(c *client) error {
			res := runAll(ctx, c, s, *pass, *only)
			return res.write(*outDir)
		}))
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}

// withUser creates the temporary user (recorded in the state file until it
// is deleted), logs in, runs fn and always deletes the user. It returns the
// first error.
func withUser(ctx context.Context, k *keycloak, apiURL string, fn func(*client) error) (err error) {
	u, cerr := k.createUser(ctx)
	if u.ID != "" {
		if serr := remember(u); serr != nil {
			log.Printf("WARNING: could not record the temporary user in the state file: %v", serr)
		}
		defer func() {
			if derr := k.deleteUser(context.WithoutCancel(ctx), u.ID); derr != nil {
				log.Printf("WARNING: could not delete the temporary user %s: %v (run: measure cleanup)", u.Username, derr)
				err = errors.Join(err, derr)
			} else {
				forget(u.ID)
				log.Printf("temporary user %s deleted", u.Username)
			}
		}()
	}
	if cerr != nil {
		return fmt.Errorf("create user: %w", cerr)
	}
	log.Printf("temporary user %s created (role analyst)", u.Username)
	s, err := k.login(ctx, u)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	log.Printf("logged in with authorization code + PKCE")
	c := &client{api: apiURL, s: s, http: &http.Client{Timeout: 150 * time.Second}}
	return fn(c)
}

// exit ends the program with status 1 after an error (deferred deletes have
// already run inside withUser).
func exit(err error) {
	if err != nil {
		log.Printf("error: %v", err)
		os.Exit(1)
	}
}

// The state file (MEASURE_STATE, default measure-users.state in the working
// folder) lists "id username" of the temporary users this tool created and
// has not deleted yet; `cleanup` deletes exactly those.
func statePath() string { return env("MEASURE_STATE", "measure-users.state") }

func readState() ([]tempUser, error) {
	b, err := os.ReadFile(statePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []tempUser
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			out = append(out, tempUser{ID: f[0], Username: f[1]})
		}
	}
	return out, nil
}

func writeState(users []tempUser) error {
	var b strings.Builder
	for _, u := range users {
		fmt.Fprintf(&b, "%s %s\n", u.ID, u.Username)
	}
	return os.WriteFile(statePath(), []byte(b.String()), 0o600)
}

func remember(u tempUser) error {
	users, err := readState()
	if err != nil {
		return err
	}
	return writeState(append(users, tempUser{ID: u.ID, Username: u.Username}))
}

func forget(id string) {
	users, err := readState()
	if err != nil {
		return
	}
	users = slices.DeleteFunc(users, func(u tempUser) bool { return u.ID == id })
	_ = writeState(users)
}

// client calls the API with the session's token.
type client struct {
	api  string
	s    *session
	http *http.Client
}

// call does one request and returns status, body and the wall time of the
// whole exchange (request sent to body read). The token refresh, when one
// is due, happens before the clock starts.
func (c *client) call(ctx context.Context, method, path string, body any) (int, []byte, time.Duration, error) {
	tok, err := c.s.Token(ctx)
	if err != nil {
		return 0, nil, 0, err
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api+path, rd)
	if err != nil {
		return 0, nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, time.Since(start), err
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, b, time.Since(start), err
}

// sample is one timed request.
type sample struct {
	Group  string  `json:"group"`
	Scope  string  `json:"scope,omitempty"`
	Label  string  `json:"label"`
	Status int     `json:"status"`
	Ms     float64 `json:"ms"`
	Items  int     `json:"items"`
	Error  string  `json:"error,omitempty"`
}

// jobResult is one background job, timed from the POST to the done state.
type jobResult struct {
	Group        string  `json:"group"`
	Label        string  `json:"label"`
	Mode         string  `json:"mode,omitempty"`
	Status       string  `json:"status"`
	ClientMs     float64 `json:"client_ms"`
	ServerMs     float64 `json:"server_ms"`
	Found        int     `json:"found"`
	ShortestLen  int     `json:"shortest_len,omitempty"`
	StoppedHubs  int     `json:"stopped_hubs"`
	Truncated    bool    `json:"truncated"`
	Members      int     `json:"members,omitempty"`
	Controllers  int     `json:"controllers,omitempty"`
	Stops        int     `json:"stops,omitempty"`
	Error        string  `json:"error,omitempty"`
	HTTPStatus   int     `json:"http_status,omitempty"`
	QueueDelayMs float64 `json:"queue_delay_ms,omitempty"`
}

// job posts a job and polls it (every 100 ms) until it is done or failed,
// or 150 s passed.
func (c *client) job(ctx context.Context, typ string, params map[string]any, label string) jobResult {
	r := jobResult{Label: label}
	start := time.Now()
	st, body, _, err := c.call(ctx, http.MethodPost, "/api/v1/jobs", map[string]any{"type": typ, "params": params})
	r.HTTPStatus = st
	if err != nil || st != http.StatusAccepted {
		r.Status, r.Error = "post_failed", errText(err, st, body)
		return r
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &created)
	deadline := start.Add(150 * time.Second)
	for {
		select {
		case <-ctx.Done():
			r.Status = "cancelled"
			return r
		case <-time.After(100 * time.Millisecond):
		}
		st, body, _, err := c.call(ctx, http.MethodGet, "/api/v1/jobs/"+created.ID, nil)
		if err != nil || st != http.StatusOK {
			if time.Now().After(deadline) {
				r.Status, r.Error = "poll_failed", errText(err, st, body)
				return r
			}
			continue
		}
		var j struct {
			Status     string          `json:"status"`
			Error      string          `json:"error"`
			Result     json.RawMessage `json:"result"`
			CreatedAt  time.Time       `json:"created_at"`
			FinishedAt *time.Time      `json:"finished_at"`
		}
		if err := json.Unmarshal(body, &j); err != nil {
			r.Status, r.Error = "bad_json", err.Error()
			return r
		}
		if j.Status == "done" || j.Status == "failed" {
			r.ClientMs = ms(time.Since(start))
			r.Status, r.Error = j.Status, j.Error
			if j.FinishedAt != nil {
				r.ServerMs = ms(j.FinishedAt.Sub(j.CreatedAt))
			}
			summarizeJob(&r, typ, j.Result)
			return r
		}
		if time.Now().After(deadline) {
			r.ClientMs = ms(time.Since(start))
			r.Status = "client_timeout(" + j.Status + ")"
			return r
		}
	}
}

func summarizeJob(r *jobResult, typ string, raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	switch typ {
	case "paths":
		var p struct {
			Paths       [][]json.RawMessage `json:"paths"`
			StoppedHubs []json.RawMessage   `json:"stopped_hubs"`
			Truncated   bool                `json:"truncated"`
		}
		if json.Unmarshal(raw, &p) == nil {
			r.Found, r.StoppedHubs, r.Truncated = len(p.Paths), len(p.StoppedHubs), p.Truncated
			for _, path := range p.Paths {
				if r.ShortestLen == 0 || len(path) < r.ShortestLen {
					r.ShortestLen = len(path)
				}
			}
		}
	case "control_group":
		var g struct {
			Controllers []json.RawMessage `json:"controllers"`
			Members     []json.RawMessage `json:"members"`
			Stops       []json.RawMessage `json:"stops"`
		}
		if json.Unmarshal(raw, &g) == nil {
			r.Members, r.Controllers, r.Stops = len(g.Members), len(g.Controllers), len(g.Stops)
		}
	}
}

func errText(err error, st int, body []byte) string {
	if err != nil {
		return err.Error()
	}
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return fmt.Sprintf("status %d %s", st, e.Error)
}

func ms(d time.Duration) float64 { return math.Round(float64(d.Microseconds())/10) / 100 }

// samples are the ids chosen by scripts/measure.sh.
type samples struct {
	Entities, TopDegree, SearchSampled, CNPJ []int64
	Pairs                                    []pair
	Control                                  []member
}

type pair struct {
	A, B  int64
	Group string
}

type member struct {
	ID    int64
	Group string
}

func readSamples(dir string) (samples, error) {
	var s samples
	ids := func(name string) ([]int64, error) {
		var out []int64
		err := eachLine(filepath.Join(dir, name), func(f []string) error {
			v, err := strconv.ParseInt(f[0], 10, 64)
			out = append(out, v)
			return err
		})
		return out, err
	}
	var err error
	if s.Entities, err = ids("entities.txt"); err != nil {
		return s, err
	}
	if s.TopDegree, err = ids("top_degree.txt"); err != nil {
		return s, err
	}
	if s.SearchSampled, err = ids("search_sampled.txt"); err != nil {
		return s, err
	}
	if s.CNPJ, err = ids("cnpj.txt"); err != nil {
		return s, err
	}
	err = eachLine(filepath.Join(dir, "pairs.txt"), func(f []string) error {
		if len(f) != 3 {
			return fmt.Errorf("pairs.txt: want 'a b group', got %q", strings.Join(f, " "))
		}
		a, err1 := strconv.ParseInt(f[0], 10, 64)
		b, err2 := strconv.ParseInt(f[1], 10, 64)
		s.Pairs = append(s.Pairs, pair{a, b, f[2]})
		return errors.Join(err1, err2)
	})
	if err != nil {
		return s, err
	}
	err = eachLine(filepath.Join(dir, "control.txt"), func(f []string) error {
		if len(f) != 2 {
			return fmt.Errorf("control.txt: want 'id group', got %q", strings.Join(f, " "))
		}
		id, err := strconv.ParseInt(f[0], 10, 64)
		s.Control = append(s.Control, member{id, f[1]})
		return err
	})
	return s, err
}

func eachLine(path string, fn func([]string) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if err := fn(fields); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return sc.Err()
}

// The spike's fixed term classes (docs/spikes/2026-11-receita-path-queries.md,
// "Tempos do desenho final"): public names and common words only.
var (
	fixedTerms = []string{"MARIA", "JOSE DA SILVA", "JOAO", "ANA PAULA", "SOUZA", "FRANCISCO DAS CHAGAS",
		"ANTONIO CARLOS", "OLIVEIRA SANTOS", "LUIZ INACIO", "PEREIRA", "PETROBRAS", "VALE S A", "ITAU UNIBANCO",
		"ODEBRECHT", "JBS", "CONSTRUTORA", "COMERCIO DE ALIMENTOS", "TRANSPORTES", "PARTICIPACOES", "HOLDING",
		"AGROPECUARIA", "IGREJA", "ASSOCIACAO DE MORADORES", "FUNDO DE INVESTIMENTO", "LTDA"}
	prefixTerms = []string{"OLIVEIRA SANT", "JOSE DA SIL", "PETROB", "ITAU UNIB", "CONSTRUT", "COMERCIO DE ALIM",
		"MARIA APARECIDA DA S", "FRANCISCO DAS CHA", "ODEBRE", "ASSOCIACAO DE MOR"}
	partialTerms = []string{"BRASILEIRO PETROB", "BANCO DO BRAS", "UNIBANCO HOLD", "COMPANHIA SIDERURGICA NAC",
		"TELEFONICA BRAS", "MAGAZINE LUI", "NATURA COSM", "GERDAU ACO", "CAIXA ECONOMICA FED", "RAIA DROG"}
	adverseTerms = []string{"SILVA", "SANTOS", "SIL", "OLIVEIRA SILVA SANTOS", "COMERCIO E SERVICOS",
		"ELEICAO 2016 VEREADOR", "JOSE MARIA DA SILVA SANTOS", "AB", "DE SOUZA", "SERVICOS"}
	scopes = []string{"", "company", "person"}
)

type results struct {
	Pass     string      `json:"pass"`
	Started  time.Time   `json:"started"`
	Finished time.Time   `json:"finished"`
	Search   []sample    `json:"search"`
	Entity   []sample    `json:"entity"`
	Neighbor []sample    `json:"neighbors"`
	Paths    []jobResult `json:"paths"`
	Control  []jobResult `json:"control_group"`
}

type entityView struct {
	Entity struct {
		ID         int64  `json:"id"`
		Kind       string `json:"kind"`
		Name       string `json:"name"`
		NaturalKey string `json:"natural_key"`
	} `json:"entity"`
}

func runAll(ctx context.Context, c *client, s samples, pass, only string) *results {
	want := func(part string) bool { return only == "" || slices.Contains(strings.Split(only, ","), part) }
	r := &results{Pass: pass, Started: time.Now().UTC()}
	defer func() { r.Finished = time.Now().UTC() }()

	if want("m1") {
		type term struct{ class, label, q string }
		var terms []term
		for _, t := range fixedTerms {
			terms = append(terms, term{"fixed", t, t})
		}
		// Sampled names (the first two words of sampled entities) and CNPJ
		// lookups: read through the API, untimed; recorded by id only.
		for _, id := range s.SearchSampled {
			if e, ok := c.entity(ctx, id); ok {
				w := strings.Fields(e.Entity.Name)
				if len(w) > 2 {
					w = w[:2]
				}
				if len(w) > 0 {
					terms = append(terms, term{"sampled", fmt.Sprintf("sampled:%d", id), strings.Join(w, " ")})
				}
			}
		}
		for _, t := range prefixTerms {
			terms = append(terms, term{"prefix", t, t})
		}
		for _, t := range partialTerms {
			terms = append(terms, term{"partial", t, t})
		}
		for _, id := range s.CNPJ {
			if e, ok := c.entity(ctx, id); ok && len(e.Entity.NaturalKey) == 8 {
				terms = append(terms, term{"cnpj", fmt.Sprintf("cnpj:%d", id), e.Entity.NaturalKey})
			}
		}
		for _, t := range adverseTerms {
			terms = append(terms, term{"adverse", t, t})
		}
		log.Printf("[%s] M1: %d terms x %d scopes", pass, len(terms), len(scopes))
		for _, sc := range scopes {
			for _, t := range terms {
				q := url.Values{"q": {t.q}}
				if sc != "" {
					q.Set("kind", sc)
				}
				st, body, d, err := c.call(ctx, http.MethodGet, "/api/v1/search?"+q.Encode(), nil)
				smp := sample{Group: t.class, Scope: scopeName(sc), Label: t.label, Status: st, Ms: ms(d)}
				var out struct {
					Items []json.RawMessage `json:"items"`
				}
				if err == nil && st == http.StatusOK && json.Unmarshal(body, &out) == nil {
					smp.Items = len(out.Items)
				} else {
					smp.Error = errText(err, st, body)
				}
				r.Search = append(r.Search, smp)
			}
		}
	}
	type ent struct {
		id    int64
		group string
	}
	var ents []ent
	for _, id := range s.Entities {
		ents = append(ents, ent{id, "random"})
	}
	for _, id := range s.TopDegree {
		ents = append(ents, ent{id, "top_degree"})
	}
	if want("entity") {
		log.Printf("[%s] entity page: %d entities", pass, len(ents))
		for _, e := range ents {
			st, body, d, err := c.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/entities/%d", e.id), nil)
			smp := sample{Group: e.group, Label: strconv.FormatInt(e.id, 10), Status: st, Ms: ms(d)}
			if err != nil || st != http.StatusOK {
				smp.Error = errText(err, st, body)
			}
			r.Entity = append(r.Entity, smp)
		}
	}
	if want("m2") {
		log.Printf("[%s] M2 neighbours: %d entities", pass, len(ents))
		for _, e := range ents {
			st, body, d, err := c.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/entities/%d/relationships?limit=200", e.id), nil)
			smp := sample{Group: e.group, Label: strconv.FormatInt(e.id, 10), Status: st, Ms: ms(d)}
			var out struct {
				Items []json.RawMessage `json:"items"`
			}
			if err == nil && st == http.StatusOK && json.Unmarshal(body, &out) == nil {
				smp.Items = len(out.Items)
			} else {
				smp.Error = errText(err, st, body)
			}
			r.Neighbor = append(r.Neighbor, smp)
		}
	}
	if want("m3") {
		log.Printf("[%s] M3 paths: %d pairs, depth 6", pass, len(s.Pairs))
		for _, p := range s.Pairs {
			j := c.job(ctx, "paths", map[string]any{"a": p.A, "b": p.B, "max_depth": 6}, fmt.Sprintf("%d-%d", p.A, p.B))
			j.Group = p.Group
			r.Paths = append(r.Paths, j)
		}
	}
	if want("m4") {
		log.Printf("[%s] M4 control group: %d companies x 2 modes", pass, len(s.Control))
		for _, mode := range []string{"documented", "corporate_links"} {
			for _, m := range s.Control {
				j := c.job(ctx, "control_group", map[string]any{"company_id": m.ID, "mode": mode}, strconv.FormatInt(m.ID, 10))
				j.Group, j.Mode = m.Group, mode
				r.Control = append(r.Control, j)
			}
		}
	}
	return r
}

func scopeName(s string) string {
	if s == "" {
		return "all"
	}
	return s
}

func (c *client) entity(ctx context.Context, id int64) (entityView, bool) {
	var e entityView
	st, body, _, err := c.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/entities/%d", id), nil)
	if err != nil || st != http.StatusOK || json.Unmarshal(body, &e) != nil {
		return e, false
	}
	return e, true
}

// stats are nearest-rank percentiles in milliseconds.
type stats struct {
	N             int
	P50, P95, Max float64
	Errors        int
}

func summarize(v []float64, errs int) stats {
	if len(v) == 0 {
		return stats{Errors: errs}
	}
	s := slices.Clone(v)
	sort.Float64s(s)
	rank := func(p float64) float64 { return s[int(math.Ceil(p*float64(len(s))))-1] }
	return stats{N: len(s), P50: rank(0.50), P95: rank(0.95), Max: s[len(s)-1], Errors: errs}
}

func group[T any](items []T, key func(T) string, val func(T) (float64, bool)) (keys []string, by map[string]stats) {
	vals := map[string][]float64{}
	errs := map[string]int{}
	seen := map[string]bool{}
	for _, it := range items {
		k := key(it)
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
		if v, ok := val(it); ok {
			vals[k] = append(vals[k], v)
		} else {
			errs[k]++
		}
	}
	by = map[string]stats{}
	for _, k := range keys {
		by[k] = summarize(vals[k], errs[k])
	}
	return keys, by
}

func secs(msv float64) string { return strconv.FormatFloat(msv/1000, 'f', 3, 64) }

func (r *results) write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(r, "", " ")
	if err := os.WriteFile(filepath.Join(dir, r.Pass+".json"), b, 0o644); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(dir, r.Pass+".csv"))
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"measure", "group", "scope_or_mode", "label", "status", "ms", "items_or_found", "error"})
	for _, s := range r.Search {
		_ = w.Write([]string{"search", s.Group, s.Scope, s.Label, strconv.Itoa(s.Status), fmt.Sprint(s.Ms), strconv.Itoa(s.Items), s.Error})
	}
	for _, s := range r.Entity {
		_ = w.Write([]string{"entity", s.Group, "", s.Label, strconv.Itoa(s.Status), fmt.Sprint(s.Ms), "", s.Error})
	}
	for _, s := range r.Neighbor {
		_ = w.Write([]string{"neighbors", s.Group, "", s.Label, strconv.Itoa(s.Status), fmt.Sprint(s.Ms), strconv.Itoa(s.Items), s.Error})
	}
	for _, j := range r.Paths {
		_ = w.Write([]string{"paths", j.Group, "", j.Label, j.Status, fmt.Sprint(j.ClientMs), strconv.Itoa(j.Found), j.Error})
	}
	for _, j := range r.Control {
		_ = w.Write([]string{"control_group", j.Group, j.Mode, j.Label, j.Status, fmt.Sprint(j.ClientMs), strconv.Itoa(j.Members), j.Error})
	}
	w.Flush()
	f.Close()
	return os.WriteFile(filepath.Join(dir, r.Pass+".md"), []byte(r.markdown()), 0o644)
}

func (r *results) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Passada `%s` (%s → %s UTC)\n\n", r.Pass, r.Started.Format("2006-01-02 15:04:05"), r.Finished.Format("15:04:05"))
	okSample := func(s sample) (float64, bool) { return s.Ms, s.Status == http.StatusOK }
	table := func(title, target string, limit float64, keys []string, by map[string]stats, all stats) {
		fmt.Fprintf(&b, "**%s** (meta: %s)\n\n| Grupo | n | p50 (s) | p95 (s) | máx. (s) | erros | meta |\n|---|---|---|---|---|---|---|\n", title, target)
		for _, k := range keys {
			s := by[k]
			fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %d | |\n", k, s.N, secs(s.P50), secs(s.P95), secs(s.Max), s.Errors)
		}
		verdict := "passou"
		if all.P95 > limit || all.Errors > 0 {
			verdict = "**FALHOU**"
		}
		fmt.Fprintf(&b, "| **todos** | %d | %s | **%s** | %s | %d | %s |\n\n", all.N, secs(all.P50), secs(all.P95), secs(all.Max), all.Errors, verdict)
	}
	if len(r.Search) > 0 {
		// A term the API refuses by design (400 query_too_short: only common
		// words, or fewer than 3 useful characters, README "Busca") is an
		// answer, timed like any other, not an error.
		okSearch := func(s sample) (float64, bool) {
			return s.Ms, s.Status == http.StatusOK || (s.Status == http.StatusBadRequest && strings.Contains(s.Error, "query_too_short"))
		}
		keys, by := group(r.Search, func(s sample) string { return s.Group + " / " + s.Scope }, okSearch)
		var core []float64
		errs, refused := 0, 0
		for _, s := range r.Search {
			if s.Status == http.StatusBadRequest {
				refused++
			}
			if s.Group == "adverse" {
				continue // extra class, reported but outside the target set (as in the spike)
			}
			if _, ok := okSearch(s); ok {
				core = append(core, s.Ms)
			} else {
				errs++
			}
		}
		table("M1 busca (todos = classes do ensaio, sem os adversos)", "p95 ≤ 1 s", 1000, keys, by, summarize(core, errs))
		fmt.Fprintf(&b, "Recusadas por desenho (400 `query_too_short`, contadas como resposta): %d.\n\n", refused)
	}
	if len(r.Entity) > 0 {
		keys, by := group(r.Entity, func(s sample) string { return s.Group }, okSample)
		_, all := group(r.Entity, func(sample) string { return "all" }, okSample)
		table("Página de entidade", "p95 ≤ 1 s", 1000, keys, by, all["all"])
	}
	if len(r.Neighbor) > 0 {
		keys, by := group(r.Neighbor, func(s sample) string { return s.Group }, okSample)
		_, all := group(r.Neighbor, func(sample) string { return "all" }, okSample)
		table("M2 vizinhos (primeira página, 200)", "p95 ≤ 2 s", 2000, keys, by, all["all"])
	}
	jobs := func(title string, list []jobResult, key func(jobResult) string) {
		if len(list) == 0 {
			return
		}
		okJob := func(j jobResult) (float64, bool) { return j.ClientMs, j.Status == "done" }
		keys, by := group(list, key, okJob)
		fmt.Fprintf(&b, "**%s** (meta: cada um ≤ 120 s, em segundo plano)\n\n| Grupo | n | p50 (s) | p95 (s) | máx. (s) | falhas | achados | hubs parados | truncados |\n|---|---|---|---|---|---|---|---|---|\n", title)
		for _, k := range keys {
			found, hubs, trunc := 0, 0, 0
			for _, j := range list {
				if key(j) != k {
					continue
				}
				if j.Found > 0 || j.Members > 0 {
					found++
				}
				hubs += j.StoppedHubs
				if j.Truncated {
					trunc++
				}
			}
			s := by[k]
			fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %d | %d | %d | %d |\n", k, s.N, secs(s.P50), secs(s.P95), secs(s.Max), s.Errors, found, hubs, trunc)
		}
		worst, fails := 0.0, 0
		for _, j := range list {
			worst = max(worst, j.ClientMs)
			if j.Status != "done" {
				fails++
			}
		}
		verdict := "passou"
		if worst > 120000 || fails > 0 {
			verdict = "**FALHOU**"
		}
		fmt.Fprintf(&b, "\nPior caso: %s s; falhas: %d → %s.\n\n", secs(worst), fails, verdict)
	}
	jobs("M3 caminhos (profundidade 6)", r.Paths, func(j jobResult) string { return j.Group })
	jobs("M4 grupo de controle", r.Control, func(j jobResult) string { return j.Group + " / " + j.Mode })
	return b.String()
}
