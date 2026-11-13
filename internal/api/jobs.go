package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"geoint/internal/graph"
	"geoint/internal/jobs"
)

// enqueueJob: POST /api/v1/jobs {"type","params"} → 202 {"id"}.
func (s *server) enqueueJob(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type   string          `json:"type"`
		Params json.RawMessage `json:"params"`
	}
	if err := decodeBody(r, &body, false); err != nil {
		fail(w, r, err)
		return
	}
	if err := jobs.ValidateParams(body.Type, body.Params); err != nil {
		fail(w, r, err)
		return
	}
	detail := map[string]any{"type": body.Type}
	switch body.Type {
	case jobs.TypePaths:
		var p jobs.PathsParams
		if json.Unmarshal(body.Params, &p) == nil {
			detail["entity_ids"] = []int64{p.A, p.B}
		}
	case jobs.TypeControlGroup:
		var p jobs.ControlGroupParams
		if json.Unmarshal(body.Params, &p) == nil {
			detail["entity_ids"] = []int64{p.CompanyID}
		}
	}
	if !s.auditAttempt(w, r, "job.enqueue", nil, detail) {
		return
	}
	id, err := s.Jobs.Enqueue(r.Context(), body.Type, body.Params, user(r).Sub)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": id.String()})
}

type jobJSON struct {
	ID         uuid.UUID       `json:"id"`
	Type       string          `json:"type"`
	Params     json.RawMessage `json:"params"`
	Status     string          `json:"status"`
	Progress   float64         `json:"progress"`
	Result     any             `json:"result"`
	Error      string          `json:"error"`
	CreatedAt  time.Time       `json:"created_at"`
	FinishedAt *time.Time      `json:"finished_at"`
}

// publicJobErrors are the job failure messages shown as they are; any other
// handler error (which may carry internal detail) is shown as "failed".
var publicJobErrors = map[string]bool{
	"": true, "could not enqueue": true, "interrupted": true, "expired": true, "unknown job type": true,
	"timeout": true, "internal error": true, "result too large": true, "entity not found": true,
}

// getJob: GET /api/v1/jobs/{id}. A job of another user is 404 (unless admin),
// indistinguishable from a missing one.
func (s *server) getJob(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		fail(w, r, errBadRequest)
		return
	}
	u := user(r)
	j, err := s.Jobs.Get(r.Context(), id, u.Sub, u.Has(roleAdmin))
	if err != nil {
		fail(w, r, err)
		return
	}
	out := jobJSON{ID: j.ID, Type: j.Type, Params: j.Params, Status: j.Status, Progress: j.Progress,
		Error: j.Error, CreatedAt: j.CreatedAt.UTC(), FinishedAt: utc(j.FinishedAt)}
	if !publicJobErrors[out.Error] {
		out.Error = "failed"
	}
	if len(j.Result) > 0 {
		if out.Result, err = jobResult(j.Type, j.Result); err != nil {
			fail(w, r, err)
			return
		}
	}
	if !s.audited(w, r, "job.read", nil, map[string]any{"job_id": j.ID.String(), "type": j.Type, "status": j.Status}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// jobResult re-shapes a stored graph result the way the API returns graph
// data: names masked, date-only fields as YYYY-MM-DD.
func jobResult(typ string, raw json.RawMessage) (any, error) {
	switch typ {
	case jobs.TypePaths:
		var res graph.PathsResult
		if err := decodeNumbers(raw, &res); err != nil {
			return nil, fmt.Errorf("decode paths result: %w", err)
		}
		type hopJSON struct {
			Edge    edgeJSON `json:"edge"`
			Forward bool     `json:"forward"`
		}
		type hubJSON struct {
			Entity entityJSON `json:"entity"`
			Degree int        `json:"degree"`
		}
		paths := make([][]hopJSON, len(res.Paths))
		for i, p := range res.Paths {
			paths[i] = make([]hopJSON, len(p))
			for k, h := range p {
				paths[i][k] = hopJSON{Edge: toEdge(h.Edge), Forward: h.Forward}
			}
		}
		hubs := make([]hubJSON, len(res.StoppedHubs))
		for i, h := range res.StoppedHubs {
			hubs[i] = hubJSON{Entity: toEntity(h.Entity), Degree: h.Degree}
		}
		return map[string]any{"paths": paths, "stopped_hubs": hubs, "truncated": res.Truncated}, nil
	case jobs.TypeControlGroup:
		var res graph.ControlResult
		if err := decodeNumbers(raw, &res); err != nil {
			return nil, fmt.Errorf("decode control group result: %w", err)
		}
		type memberJSON struct {
			Entity entityJSON `json:"entity"`
			Depth  int        `json:"depth"`
			Via    []int64    `json:"via"`
		}
		type stopJSON struct {
			Entity entityJSON `json:"entity"`
			Reason string     `json:"reason"`
		}
		members := make([]memberJSON, len(res.Members))
		for i, m := range res.Members {
			via := m.Via
			if via == nil {
				via = []int64{}
			}
			members[i] = memberJSON{Entity: toEntity(m.Entity), Depth: m.Depth, Via: via}
		}
		stops := make([]stopJSON, len(res.Stops))
		for i, st := range res.Stops {
			stops[i] = stopJSON{Entity: toEntity(st.Entity), Reason: st.Reason}
		}
		return map[string]any{"root": toEntity(res.Root), "controllers": toEntities(res.Controllers),
			"members": members, "edges": toEdges(res.Edges), "stops": stops}, nil
	}
	return raw, nil
}

// decodeNumbers decodes a stored result keeping the numbers of untyped
// values (edge attrs) as json.Number, so they are shown as written.
func decodeNumbers(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(v)
}
