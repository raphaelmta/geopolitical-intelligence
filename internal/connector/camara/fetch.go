package camara

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"time"

	"geoint/internal/connector/legis"
	"geoint/internal/rawstore"
)

// pageSize is the "itens" of the deputies list (the documented maximum).
const pageSize = 100

// maxPages bounds the pages of one list (the deputies list of a
// legislature had 11 in 2026: 1,071 rows in the 56th).
const maxPages = 200

// rawPrefix is the raw store prefix of a collection: the reference date.
func rawPrefix(ref time.Time) string {
	return "raw/" + Key + "/" + ref.UTC().Format(time.DateOnly) + "/"
}

func (c *camara) client() (*legis.Client, error) {
	cl, err := legis.NewClient(Key, c.baseURL)
	if err != nil {
		return nil, err
	}
	cl.MinInterval, cl.Backoff, cl.MaxBackoff = c.pace, c.backoff, c.maxBackoff
	cl.Attempts, cl.MaxBody, cl.MaxRequests = c.attempts, c.maxBody, c.maxRequests
	return cl, nil
}

// Fetch stores, under raw/camara/<ref date>/, for every legislature: the
// legislature (legislaturas_<n>.json), every page of its parties list
// (partidos_<n>_p<k>.json: sigla -> name) and of its deputies list
// (deputados_<n>_p<k>.json), following the "next" links; then for every
// deputy listed in any of them the record (deputado_<id>.json) and the
// status history (deputado_<id>_historico.json). Responses are checked,
// then stored as they came; one already stored under the date is reused,
// not re-requested. A 4xx for one deputy's record or history stores an
// "unavailable" marker instead (Load skips that deputy); a 4xx for a list
// fails the fetch.
func (c *camara) Fetch(ctx context.Context, ref time.Time, raw rawstore.Store) ([]rawstore.File, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cl, err := c.client()
	if err != nil {
		return nil, err
	}
	f := &legis.Fetcher{Client: cl, Raw: raw, Prefix: rawPrefix(ref)}
	ids := map[int64]bool{}
	for _, n := range c.legislatures {
		name := fmt.Sprintf("legislaturas_%d.json", n)
		_, err := f.Get(ctx, name, "legislaturas/"+strconv.Itoa(n), nil, func(b []byte) error {
			_, _, err := legislature(name, b, n)
			return err
		})
		if err != nil {
			return nil, err
		}
		if err := pages(ctx, f, fmt.Sprintf("partidos_%d", n), "partidos", url.Values{"idLegislatura": {strconv.Itoa(n)},
			"itens": {strconv.Itoa(pageSize)}, "ordem": {"ASC"}, "ordenarPor": {"sigla"}}, nil); err != nil {
			return nil, err
		}
		if err := pages(ctx, f, fmt.Sprintf("deputados_%d", n), "deputados", url.Values{"idLegislatura": {strconv.Itoa(n)},
			"itens": {strconv.Itoa(pageSize)}, "ordem": {"ASC"}, "ordenarPor": {"id"}}, func(list []json.RawMessage) {
			for _, it := range list {
				if id, ok := deputyID(it); ok {
					ids[id] = true
				}
			}
		}); err != nil {
			return nil, err
		}
	}
	sorted := make([]int64, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	slices.Sort(sorted)
	for _, id := range sorted {
		s := strconv.FormatInt(id, 10)
		name := "deputado_" + s + ".json"
		_, ok, err := f.GetItem(ctx, name, "deputados/"+s, nil, func(b []byte) error {
			_, err := deputy(name, b, id)
			return err
		})
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // the record is unavailable: Load skips the deputy, no need for the history
		}
		hname := "deputado_" + s + "_historico.json"
		if _, _, err := f.GetItem(ctx, hname, "deputados/"+s+"/historico", nil, func(b []byte) error {
			_, _, _, err := history(hname, b)
			return err
		}); err != nil {
			return nil, err
		}
	}
	c.fetched.Add(f.URLs())
	return f.Files(), nil
}

// pages fetches every page of a list (<base>_p<k>.json), following "next".
func pages(ctx context.Context, f *legis.Fetcher, base, path string, q url.Values, each func([]json.RawMessage)) error {
	for page := 1; ; page++ {
		if page > maxPages {
			return fmt.Errorf("camara: %s has more than %d pages", base, maxPages)
		}
		name := fmt.Sprintf("%s_p%d.json", base, page)
		q.Set("pagina", strconv.Itoa(page))
		var (
			list []json.RawMessage
			next bool
		)
		if _, err := f.Get(ctx, name, path, q, func(b []byte) error {
			var err error
			list, next, err = items(name, b)
			return err
		}); err != nil {
			return err
		}
		if each != nil {
			each(list)
		}
		if !next {
			return nil
		}
	}
}
