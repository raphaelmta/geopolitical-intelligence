package api_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"geoint/internal/graph"
	"geoint/internal/jobs"
)

// startJobWorker runs a worker (the other process in production) on the env's
// pool and queue until the test ends.
func (e *env) startJobWorker() {
	e.t.Helper()
	w := jobs.New(e.pool, e.q)
	w.ProgressInterval = 10 * time.Millisecond
	jobs.RegisterGraphHandlers(w, graph.New(e.pool))
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- w.RunWorker(ctx) }()
	e.t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			e.t.Error("worker did not stop")
		}
	})
}

type jobView struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Status string `json:"status"`
	Error  string `json:"error"`
	Result *struct {
		Paths [][]struct {
			Edge    edge `json:"edge"`
			Forward bool `json:"forward"`
		} `json:"paths"`
		Truncated bool `json:"truncated"`
	} `json:"result"`
}

func (e *env) enqueue(token, body string) string {
	e.t.Helper()
	got := want[map[string]string](e.t, e.do("POST", "/api/v1/jobs", token, body), http.StatusAccepted)
	if got["id"] == "" {
		e.t.Fatalf("no job id: %v", got)
	}
	return got["id"]
}

func TestJobRoundTripPaths(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	e.startJobWorker()
	id := e.enqueue(tokAnalyst, fmt.Sprintf(`{"type":"paths","params":{"a":%d,"b":%d}}`, e.ids.P1, e.ids.A))
	if n := e.auditCount("job.enqueue", nil); n != 1 {
		t.Fatalf("%d job.enqueue rows", n)
	}
	var j jobView
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		j = want[jobView](t, e.do("GET", "/api/v1/jobs/"+id, tokAnalyst, ""), http.StatusOK)
		if j.Status == "done" || j.Status == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if j.Status != "done" || j.Result == nil || len(j.Result.Paths) != 1 || len(j.Result.Paths[0]) != 2 {
		t.Fatalf("job %+v", j)
	}
	for _, hop := range j.Result.Paths[0] {
		if hop.Edge.RelationshipID == e.ids.RelHA && (hop.Edge.ValidFrom == nil || *hop.Edge.ValidFrom != "2020-01-01" ||
			hop.Edge.Attrs["role"] != "22") {
			t.Fatalf("H→A in the job result: %+v, want valid_from as a date", hop.Edge)
		}
		if hop.Edge.Kind != "fact" || hop.Edge.SourceKey != "receita" {
			t.Fatalf("hop %+v", hop.Edge)
		}
	}
	if n := e.auditCount("job.read", nil); n == 0 {
		t.Fatal("job reads not audited")
	}
}

func TestJobOfAnotherUserIs404(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	id := e.enqueue(tokAnalyst, `{"type":"control_group","params":{"company_id":1,"mode":"documented"}}`)
	wantError(t, e.do("GET", "/api/v1/jobs/"+id, tokAnalyst2, ""), http.StatusNotFound, "not_found")
	wantError(t, e.do("GET", "/api/v1/jobs/00000000-0000-7000-8000-000000000000", tokAnalyst, ""), http.StatusNotFound, "not_found")
	if j := want[jobView](t, e.do("GET", "/api/v1/jobs/"+id, tokAnalyst, ""), http.StatusOK); j.ID != id || j.Type != "control_group" {
		t.Fatalf("owner read %+v", j)
	}
	if j := want[jobView](t, e.do("GET", "/api/v1/jobs/"+id, tokAdmin, ""), http.StatusOK); j.ID != id {
		t.Fatalf("admin read %+v", j)
	}
	if n := e.auditCount("job.read", nil); n != 2 {
		t.Fatalf("%d job.read rows, want 2 (refused reads return nothing)", n)
	}
}
