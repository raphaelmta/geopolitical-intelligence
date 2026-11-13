package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"
)

func timeAfter100ms() <-chan time.Time { return time.After(100 * time.Millisecond) }

// The success-criteria walk (docs/performance, "critérios de sucesso"):
// the same API calls an analyst's screens make, summarized as counts,
// kinds and ids. Names are printed only for parties, public bodies, the
// company searched by CNPJ and the two ends of the path (chosen by the
// caller: a public office holder and a large company); any other person or
// company is "pessoa #id" / "empresa #id". CPF fragments and personal
// values are never printed.

type edgeJSON struct {
	RelationshipID int64  `json:"relationship_id"`
	Type           string `json:"type"`
	Src, Dst       struct {
		ID         int64  `json:"id"`
		Kind       string `json:"kind"`
		Name       string `json:"name"`
		NaturalKey string `json:"natural_key"`
	}
	Kind               string   `json:"kind"`
	Confidence         *float64 `json:"confidence"`
	IdentityConfidence *float64 `json:"identity_confidence"`
	IdentityBasis      string   `json:"identity_basis"`
	SourceKey          string   `json:"source_key"`
	Closed             bool     `json:"closed"`
	Stale              bool     `json:"stale"`
	ValidFrom          *string  `json:"valid_from"`
	ValidToAfter       *string  `json:"valid_to_after"`
	ValidToBefore      *string  `json:"valid_to_before"`
}

func label(kind, name string, id int64, public map[int64]bool) string {
	// Only the entities the caller named (a large public company, a public
	// office holder) are shown by name: any other company may carry an
	// individual's name (sole proprietors).
	// Allow-list: parties, public bodies and the named entities show their
	// names; every other kind (person, company, foreign: foreign partners
	// include individuals) is anonymous.
	if public[id] || kind == "party" || kind == "public_body" {
		return fmt.Sprintf("%s %q #%d", kind, name, id)
	}
	if k, ok := map[string]string{"person": "pessoa", "company": "empresa", "foreign": "estrangeiro"}[kind]; ok {
		return fmt.Sprintf("%s #%d", k, id)
	}
	return fmt.Sprintf("entidade #%d", id)
}

func (c *client) getJSON(ctx context.Context, path string, out any) (int, error) {
	st, body, _, err := c.call(ctx, http.MethodGet, path, nil)
	if err != nil {
		return st, err
	}
	if st != http.StatusOK {
		return st, fmt.Errorf("GET %s: %s", path, errText(nil, st, body))
	}
	return st, json.Unmarshal(body, out)
}

// edges returns up to pages pages of an entity's relationships (state all).
func (c *client) edges(ctx context.Context, id int64, pages int) ([]edgeJSON, bool) {
	var all []edgeJSON
	cursor := ""
	for i := 0; i < pages; i++ {
		var page struct {
			Items      []edgeJSON `json:"items"`
			NextCursor string     `json:"next_cursor"`
		}
		q := url.Values{"state": {"all"}, "limit": {"200"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		if _, err := c.getJSON(ctx, fmt.Sprintf("/api/v1/entities/%d/relationships?%s", id, q.Encode()), &page); err != nil {
			fmt.Printf("  erro: %v\n", err)
			return all, false
		}
		all = append(all, page.Items...)
		if page.NextCursor == "" {
			return all, false
		}
		cursor = page.NextCursor
	}
	return all, true
}

func countBy(edges []edgeJSON, key func(edgeJSON) string) string {
	m := map[string]int{}
	for _, e := range edges {
		m[key(e)]++
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for _, k := range keys {
		out += fmt.Sprintf("%s=%d ", k, m[k])
	}
	return out
}

func criteria(ctx context.Context, c *client, cnpj string, a, b int64) {
	public := map[int64]bool{a: true, b: true}
	fmt.Println("## 1. CNPJ → sócios e administradores, outras empresas de cada um, cadeia")
	var found struct {
		Items []struct {
			ID   int64  `json:"id"`
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"items"`
	}
	if _, err := c.getJSON(ctx, "/api/v1/search?q="+url.QueryEscape(cnpj), &found); err != nil || len(found.Items) == 0 {
		fmt.Printf("  busca pelo CNPJ %s: %v, %d resultados\n", cnpj, err, len(found.Items))
	} else {
		co := found.Items[0]
		public[co.ID] = true
		fmt.Printf("  busca %s → %s\n", cnpj, label(co.Kind, co.Name, co.ID, public))
		var ent struct {
			Versions []json.RawMessage `json:"versions"`
		}
		_, _ = c.getJSON(ctx, fmt.Sprintf("/api/v1/entities/%d", co.ID), &ent)
		fmt.Printf("  página da entidade: %d versão(ões) cadastrais\n", len(ent.Versions))
		es, more := c.edges(ctx, co.ID, 10)
		fmt.Printf("  vínculos (todas as situações, até 2000): %d%s\n", len(es), map[bool]string{true: " (há mais)", false: ""}[more])
		fmt.Printf("  por tipo: %s\n", countBy(es, func(e edgeJSON) string { return e.Type }))
		fmt.Printf("  por tipo da outra ponta: %s\n", countBy(es, func(e edgeJSON) string {
			if e.Src.ID == co.ID {
				return e.Dst.Kind
			}
			return e.Src.Kind
		}))
		// Other companies of up to 5 partners/administrators, and one more level.
		seen := 0
		for _, e := range es {
			if e.Dst.ID != co.ID || (e.Type != "partner_of" && e.Type != "administrator_of") || seen >= 5 {
				continue
			}
			seen++
			other, _ := c.edges(ctx, e.Src.ID, 2)
			companies := map[int64]bool{}
			var next int64
			for _, o := range other {
				if o.Src.ID == e.Src.ID && o.Dst.ID != co.ID && o.Dst.Kind == "company" {
					companies[o.Dst.ID] = true
					if next == 0 {
						next = o.Dst.ID
					}
				}
			}
			line := fmt.Sprintf("  %s (%s) → %d outra(s) empresa(s)", label(e.Src.Kind, e.Src.Name, e.Src.ID, public), e.Type, len(companies))
			if next != 0 {
				lvl2, _ := c.edges(ctx, next, 1)
				line += fmt.Sprintf("; nível seguinte: empresa #%d tem %d vínculo(s)", next, len(lvl2))
			}
			fmt.Println(line)
		}
		if len(es) > 0 {
			fmt.Println("## 3. Proveniência de um vínculo dessa empresa")
			c.provenance(ctx, es[0].RelationshipID)
		}
		fmt.Println("## 6/7. Fato e inferência, identidade, fechados (nos vínculos acima)")
		fmt.Printf("  kind: %s\n", countBy(es, func(e edgeJSON) string { return e.Kind }))
		fmt.Printf("  identity_basis: %s\n", countBy(es, func(e edgeJSON) string { return e.IdentityBasis }))
		fmt.Printf("  closed: %s stale: %s\n", countBy(es, func(e edgeJSON) string { return fmt.Sprint(e.Closed) }),
			countBy(es, func(e edgeJSON) string { return fmt.Sprint(e.Stale) }))
		certain := 0
		for _, e := range es {
			if e.Kind == "inference" && (e.Confidence == nil || *e.Confidence >= 1) {
				certain++
			}
			if e.IdentityBasis == "auto" && e.IdentityConfidence != nil && *e.IdentityConfidence >= 1 {
				certain++
			}
		}
		fmt.Printf("  inferências ou junções automáticas mostradas com confiança 1: %d\n", certain)
	}

	if a != 0 && b != 0 {
		fmt.Printf("## 2. Caminhos entre #%d e #%d (profundidade 6)\n", a, b)
		st, body, _, err := c.call(ctx, http.MethodPost, "/api/v1/jobs", map[string]any{"type": "paths", "params": map[string]any{"a": a, "b": b, "max_depth": 6}})
		if err != nil || st != http.StatusAccepted {
			fmt.Printf("  erro: %s\n", errText(err, st, body))
		} else {
			var created struct{ ID string }
			_ = json.Unmarshal(body, &created)
			c.printPaths(ctx, created.ID, public)
		}
	}

	fmt.Println("## 5. Tela Bases (GET /sources)")
	var srcs []struct {
		Key               string  `json:"key"`
		Status            string  `json:"status"`
		LastReferenceDate *string `json:"last_reference_date"`
		Observed          *string `json:"last_observed_date"`
		Editions          bool    `json:"editions"`
		LastCollectedAt   *string `json:"last_collected_at"`
	}
	if _, err := c.getJSON(ctx, "/api/v1/sources", &srcs); err != nil {
		fmt.Printf("  erro: %v\n", err)
	}
	for _, s := range srcs {
		fmt.Printf("  %s: %s, referência %s, observada %s, coletada %s, edições %v\n", s.Key, s.Status, deref(s.LastReferenceDate), deref(s.Observed), deref(s.LastCollectedAt), s.Editions)
	}
	fmt.Println("## 8. Fila de identidades (GET /identity/proposals?status=pending)")
	var props struct {
		Items      []json.RawMessage `json:"items"`
		NextCursor string            `json:"next_cursor"`
	}
	st, body, d, err := c.call(ctx, http.MethodGet, "/api/v1/identity/proposals?status=pending", nil)
	if err == nil && st == http.StatusOK && json.Unmarshal(body, &props) == nil {
		fmt.Printf("  primeira página: %d propostas em %.3f s, próxima página: %v\n", len(props.Items), ms(d)/1000, props.NextCursor != "")
	} else {
		fmt.Printf("  erro: %s\n", errText(err, st, body))
	}
}

func deref(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}

func (c *client) provenance(ctx context.Context, id int64) {
	var p struct {
		Kind       string `json:"kind"`
		RecordRef  string `json:"record_ref"`
		Source     struct{ Key, Name string }
		Collection struct {
			ID            int64
			ReferenceDate string  `json:"reference_date"`
			ObservedDate  string  `json:"observed_date"`
			CollectedAt   *string `json:"collected_at"`
		}
		LastSeen *struct {
			ID            int64
			ReferenceDate string `json:"reference_date"`
		} `json:"last_seen_collection"`
		RawKey    string  `json:"raw_key"`
		RawSHA256 string  `json:"raw_sha256"`
		RawSize   int64   `json:"raw_size"`
		SourceURL *string `json:"source_url"`
		Note      string  `json:"source_note"`
	}
	if _, err := c.getJSON(ctx, fmt.Sprintf("/api/v1/relationships/%d/provenance", id), &p); err != nil {
		fmt.Printf("  erro: %v\n", err)
		return
	}
	file := p.RecordRef
	for i := len(file) - 1; i >= 0; i-- {
		if file[i] == ':' {
			file = file[:i]
			break
		}
	}
	fmt.Printf("  vínculo #%d: %s; base %s; arquivo do registro %s (linha omitida); coleta #%d referência %s observada %s coletada %s\n",
		id, p.Kind, p.Source.Key, file, p.Collection.ID, p.Collection.ReferenceDate, p.Collection.ObservedDate, deref(p.Collection.CollectedAt))
	if p.LastSeen != nil {
		fmt.Printf("  visto pela última vez: coleta #%d (%s)\n", p.LastSeen.ID, p.LastSeen.ReferenceDate)
	}
	fmt.Printf("  arquivo bruto: %s, %d bytes, sha256 %s\n  fonte: %s\n  nota: %v\n", p.RawKey, p.RawSize, p.RawSHA256, deref(p.SourceURL), p.Note != "")
}

func (c *client) printPaths(ctx context.Context, jobID string, public map[int64]bool) {
	for i := 0; i < 1500; i++ {
		var j struct {
			Status string `json:"status"`
			Error  string `json:"error"`
			Result *struct {
				Paths [][]struct {
					Edge    edgeJSON `json:"edge"`
					Forward bool     `json:"forward"`
				} `json:"paths"`
				StoppedHubs []json.RawMessage `json:"stopped_hubs"`
				Truncated   bool              `json:"truncated"`
			} `json:"result"`
		}
		if _, err := c.getJSON(ctx, "/api/v1/jobs/"+jobID, &j); err != nil {
			fmt.Printf("  erro: %v\n", err)
			return
		}
		if j.Status != "done" && j.Status != "failed" {
			select {
			case <-ctx.Done():
				return
			case <-timeAfter100ms():
			}
			continue
		}
		if j.Result == nil {
			fmt.Printf("  %s %s\n", j.Status, j.Error)
			return
		}
		fmt.Printf("  %s: %d caminho(s), hubs parados %d, truncado %v\n", j.Status, len(j.Result.Paths), len(j.Result.StoppedHubs), j.Result.Truncated)
		for n, path := range j.Result.Paths {
			if n >= 3 {
				break
			}
			fmt.Printf("  caminho %d (%d elos):\n", n+1, len(path))
			for _, h := range path {
				e := h.Edge
				fmt.Printf("    %s —%s→ %s [%s, identidade %s, fonte %s, fechado %v, vínculo #%d]\n",
					label(e.Src.Kind, e.Src.Name, e.Src.ID, public), e.Type, label(e.Dst.Kind, e.Dst.Name, e.Dst.ID, public),
					e.Kind, e.IdentityBasis, e.SourceKey, e.Closed, e.RelationshipID)
			}
		}
		return
	}
}
