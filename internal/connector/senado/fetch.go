package senado

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"geoint/internal/connector/legis"
	"geoint/internal/rawstore"
)

// rawPrefix is the raw store prefix of a collection: the reference date.
func rawPrefix(ref time.Time) string {
	return "raw/" + Key + "/" + ref.UTC().Format(time.DateOnly) + "/"
}

func (c *senado) client() (*legis.Client, error) {
	cl, err := legis.NewClient(Key, c.baseURL)
	if err != nil {
		return nil, err
	}
	cl.MinInterval, cl.Backoff, cl.MaxBackoff = c.pace, c.backoff, c.maxBackoff
	cl.Attempts, cl.MaxBody, cl.MaxRequests = c.attempts, c.maxBody, c.maxRequests
	return cl, nil
}

// Fetch stores, under raw/senado/<ref date>/, the senators of every
// legislature (senadores_<n>.json, one response each: the API has no
// pagination) and the party affiliations (senador_<código>_filiacoes.json)
// of every listed senator who was ever in office (has an Exercicio).
// Responses are checked, then stored as they came (JSON, asked with
// Accept); one already stored under the date is reused, not re-requested.
// A 4xx for one senator's affiliations stores an "unavailable" marker
// instead (Load skips that senator); a 4xx for a list fails the fetch.
func (c *senado) Fetch(ctx context.Context, ref time.Time, raw rawstore.Store) ([]rawstore.File, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cl, err := c.client()
	if err != nil {
		return nil, err
	}
	f := &legis.Fetcher{Client: cl, Raw: raw, Prefix: rawPrefix(ref)}
	codes := map[int64]bool{}
	for _, n := range c.legislatures {
		name := fmt.Sprintf("senadores_%d.json", n)
		var list legis.Many[json.RawMessage]
		_, err := f.Get(ctx, name, "senador/lista/legislatura/"+strconv.Itoa(n), nil, func(b []byte) error {
			var err error
			list, err = senators(name, b)
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, item := range list.Items {
			var s senatorItem
			if json.Unmarshal(item, &s) != nil || !s.inOffice() {
				continue
			}
			if code, ok := s.code(); ok {
				n, _ := strconv.ParseInt(code, 10, 64)
				codes[n] = true
			}
		}
	}
	sorted := make([]int64, 0, len(codes))
	for c := range codes {
		sorted = append(sorted, c)
	}
	slices.Sort(sorted)
	for _, code := range sorted {
		s := strconv.FormatInt(code, 10)
		name := "senador_" + s + "_filiacoes.json"
		_, _, err := f.GetItem(ctx, name, "senador/"+s+"/filiacoes", nil, func(b []byte) error {
			_, _, err := affiliations(name, b)
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	c.fetched.Add(f.URLs())
	return f.Files(), nil
}
