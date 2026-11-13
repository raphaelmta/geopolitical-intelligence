// Package sourcesim simulates the five public sources for the end-to-end
// test (`make e2e`): it serves the committed fixtures of testdata/ in the
// wire formats the connectors read from the real portals. It is test
// tooling only: imported by cmd/sourcesim and its own test, never by the
// geoint binary, and never part of deploy/compose.yml.
//
// Routes (all under one server):
//
//	/receita/            WebDAV-like share (Basic auth user required, like the
//	                     real share token): PROPFIND Depth 1 lists one month
//	                     folder, the current month (UTC); PROPFIND on the
//	                     folder lists Empresas0.zip, Estabelecimentos0.zip and
//	                     Socios0.zip (size, ETag, Last-Modified); GET serves
//	                     them with Range support. Each zip holds one CSV.
//	/tse/consulta_cand/consulta_cand_<year>.zip
//	/tse/prestacao_contas/prestacao_de_contas_eleitorais_candidatos_<year>.zip
//	                     every default election year; years without fixtures
//	                     are zips of header-only CSVs (no candidacies).
//	/camara/api/v2/...   the Câmara REST v2 routes the connector calls; the
//	                     legislatures without fixtures (55) have their real
//	                     dates and empty lists.
//	/senado/dadosabertos/senador/...  the Senado routes; legislatures without
//	                     fixtures (55) are empty lists.
//	/cvm/fre_cia_aberta_<year>.zip    the fixture years.
//	/healthz             200 "ok".
//
// Every zip is built once, at start, with fixed timestamps, so sizes and
// ETags are stable for the life of the process.
package sourcesim

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"geoint/internal/connector/camara"
	"geoint/internal/connector/senado"
	"geoint/internal/connector/tse"
)

// zipTime is the modification time written into every zip entry, so the
// same fixtures always give byte-identical zips.
var zipTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// file is one downloadable file.
type file struct {
	body []byte
	etag string
}

func newFile(b []byte) file {
	sum := sha256.Sum256(b)
	return file{body: b, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
}

// Server is the simulator's http.Handler.
type Server struct {
	now      func() time.Time
	started  time.Time
	receita  map[string]file // by zip name
	tse      map[string]file // by path under /tse/
	cvm      map[string]file // by zip name
	camara   map[string][]byte
	senado   map[string][]byte
	receitaN []string // zip names, sorted
}

// New loads the fixtures under testdata (the repository's testdata/
// directory) and returns the simulator. now gives the clock that names the
// Receita month folder (nil: time.Now).
func New(testdata string, now func() time.Time) (*Server, error) {
	if now == nil {
		now = time.Now
	}
	s := &Server{now: now, started: now().UTC(), receita: map[string]file{}, tse: map[string]file{}, cvm: map[string]file{},
		camara: map[string][]byte{}, senado: map[string][]byte{}}
	if err := s.loadReceita(filepath.Join(testdata, "receita")); err != nil {
		return nil, err
	}
	if err := s.loadTSE(filepath.Join(testdata, "tse")); err != nil {
		return nil, err
	}
	if err := s.loadJSON(filepath.Join(testdata, "camara"), s.camara); err != nil {
		return nil, err
	}
	if err := s.loadJSON(filepath.Join(testdata, "senado"), s.senado); err != nil {
		return nil, err
	}
	if err := s.loadCVM(filepath.Join(testdata, "cvm")); err != nil {
		return nil, err
	}
	s.addEmptyLegislatures()
	return s, nil
}

// zipOf builds a zip with the entries in name order and fixed timestamps.
func zipOf(entries map[string][]byte) ([]byte, error) {
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: n, Method: zip.Deflate, Modified: zipTime})
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(entries[n]); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// loadReceita: one zip per kind (part 0), each with the fixture CSV, like
// the published files.
func (s *Server) loadReceita(dir string) error {
	for _, kind := range []string{"Empresas", "Estabelecimentos", "Socios"} {
		b, err := os.ReadFile(filepath.Join(dir, kind+".csv"))
		if err != nil {
			return fmt.Errorf("sourcesim: receita: %w", err)
		}
		z, err := zipOf(map[string][]byte{kind + ".csv": b})
		if err != nil {
			return err
		}
		name := kind + "0.zip"
		s.receita[name] = newFile(z)
		s.receitaN = append(s.receitaN, name)
	}
	sort.Strings(s.receitaN)
	return nil
}

var tseFixture = regexp.MustCompile(`^(consulta_cand|receitas_candidatos)_([0-9]{4})_([A-Z]{2})\.csv$`)

// loadTSE builds the two zips of every default election year: the UF CSVs
// of the fixture (and the _BRASIL aggregate the TSE adds) or, for a year
// without fixtures, one header-only CSV per kind.
func (s *Server) loadTSE(dir string) error {
	matches, err := filepath.Glob(filepath.Join(dir, "*.csv"))
	if err != nil {
		return err
	}
	// kind -> year -> entry name -> body
	byYear := map[string]map[int]map[string][]byte{}
	header := map[string][]byte{}
	for _, m := range matches {
		g := tseFixture.FindStringSubmatch(filepath.Base(m))
		if g == nil {
			continue
		}
		b, err := os.ReadFile(m)
		if err != nil {
			return fmt.Errorf("sourcesim: tse: %w", err)
		}
		year, _ := strconv.Atoi(g[2])
		if byYear[g[1]] == nil {
			byYear[g[1]] = map[int]map[string][]byte{}
		}
		if byYear[g[1]][year] == nil {
			byYear[g[1]][year] = map[string][]byte{}
		}
		byYear[g[1]][year][filepath.Base(m)] = b
		if h, _, ok := bytes.Cut(b, []byte("\r\n")); ok {
			header[g[1]] = append(h, '\r', '\n')
		}
	}
	for _, kind := range []string{"consulta_cand", "receitas_candidatos"} {
		if header[kind] == nil {
			return fmt.Errorf("sourcesim: tse: no %s fixture", kind)
		}
	}
	for _, y := range tse.DefaultYears {
		for _, kind := range []string{"consulta_cand", "receitas_candidatos"} {
			entries := byYear[kind][y]
			if entries == nil {
				entries = map[string][]byte{fmt.Sprintf("%s_%d_AC.csv", kind, y): header[kind]}
			}
			all := map[string][]byte{}
			for n, b := range entries {
				all[n] = b
			}
			all[fmt.Sprintf("%s_%d_BRASIL.csv", kind, y)] = brasil(entries)
			var name string
			if kind == "consulta_cand" {
				all["leiame.pdf"] = []byte("%PDF-1.4 placeholder\n")
				name = fmt.Sprintf("consulta_cand/consulta_cand_%d.zip", y)
			} else {
				all["leiame_receitas-candidatos.pdf"] = []byte("%PDF-1.4 placeholder\n")
				name = fmt.Sprintf("prestacao_contas/prestacao_de_contas_eleitorais_candidatos_%d.zip", y)
			}
			z, err := zipOf(all)
			if err != nil {
				return err
			}
			s.tse[name] = newFile(z)
		}
	}
	return nil
}

// brasil is the _BRASIL aggregate the TSE publishes: the header once and
// every data line of the UF files, in name order.
func brasil(entries map[string][]byte) []byte {
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	for i, n := range names {
		b := entries[n]
		if i > 0 {
			_, b, _ = bytes.Cut(b, []byte("\r\n"))
		}
		buf.Write(b)
	}
	return buf.Bytes()
}

var cvmFixture = regexp.MustCompile(`^fre_cia_aberta_posicao_acionaria_([0-9]{4})\.csv$`)

// loadCVM: one zip per fixture year with the shareholding entry and the
// document index (header only), as the portal publishes it.
func (s *Server) loadCVM(dir string) error {
	matches, err := filepath.Glob(filepath.Join(dir, "*.csv"))
	if err != nil {
		return err
	}
	for _, m := range matches {
		g := cvmFixture.FindStringSubmatch(filepath.Base(m))
		if g == nil {
			continue
		}
		b, err := os.ReadFile(m)
		if err != nil {
			return fmt.Errorf("sourcesim: cvm: %w", err)
		}
		z, err := zipOf(map[string][]byte{
			filepath.Base(m):                  b,
			"fre_cia_aberta_" + g[1] + ".csv": []byte("CNPJ_CIA;DT_REFER;VERSAO;DENOM_CIA;CD_CVM;CATEG_DOC;ID_DOC;DT_RECEB;LINK_DOC\r\n"),
		})
		if err != nil {
			return err
		}
		s.cvm["fre_cia_aberta_"+g[1]+".zip"] = newFile(z)
	}
	if len(s.cvm) == 0 {
		return fmt.Errorf("sourcesim: cvm: no fixture in %s", dir)
	}
	return nil
}

func (s *Server) loadJSON(dir string, into map[string][]byte) error {
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			return fmt.Errorf("sourcesim: %w", err)
		}
		into[filepath.Base(m)] = b
	}
	if len(into) == 0 {
		return fmt.Errorf("sourcesim: no fixture in %s", dir)
	}
	return nil
}

// legislatureDates are the official dates of the legislatures the
// connectors collect by default (Câmara API, legislaturas/{n}).
var legislatureDates = map[int][2]string{
	55: {"2015-02-01", "2019-01-31"},
	56: {"2019-02-01", "2023-01-31"},
	57: {"2023-02-01", "2027-01-31"},
}

// addEmptyLegislatures fills the default legislatures that have no fixture
// with their real dates and empty member lists, so a default-configured
// worker collects the fixtures without a 404.
func (s *Server) addEmptyLegislatures() {
	for _, n := range camara.DefaultLegislatures {
		if _, ok := s.camara[fmt.Sprintf("legislaturas_%d.json", n)]; ok {
			continue
		}
		d := legislatureDates[n]
		s.camara[fmt.Sprintf("legislaturas_%d.json", n)] = mustJSON(map[string]any{
			"dados": map[string]any{"id": n, "dataInicio": d[0], "dataFim": d[1]}, "links": []any{}})
		for _, list := range []string{"deputados", "partidos"} {
			s.camara[fmt.Sprintf("%s_%d_p1.json", list, n)] = mustJSON(map[string]any{"dados": []any{}, "links": []any{}})
		}
	}
	for _, n := range senado.DefaultLegislatures {
		name := fmt.Sprintf("senadores_%d.json", n)
		if _, ok := s.senado[name]; ok {
			continue
		}
		s.senado[name] = mustJSON(map[string]any{"ListaParlamentarLegislatura": map[string]any{
			"Parlamentares": map[string]any{"Parlamentar": []any{}}}})
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// ServeHTTP routes a request to the source it simulates.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/healthz":
		_, _ = w.Write([]byte("ok\n"))
	case strings.HasPrefix(p, "/receita/"):
		s.serveReceita(w, r)
	case strings.HasPrefix(p, "/tse/"):
		serveFile(w, r, s.tse, strings.TrimPrefix(p, "/tse/"), s.started)
	case strings.HasPrefix(p, "/cvm/"):
		serveFile(w, r, s.cvm, strings.TrimPrefix(p, "/cvm/"), s.started)
	case strings.HasPrefix(p, "/camara/api/v2/"):
		s.serveCamara(w, r, strings.TrimPrefix(p, "/camara/api/v2"))
	case strings.HasPrefix(p, "/senado/dadosabertos/"):
		s.serveSenado(w, r, strings.TrimPrefix(p, "/senado/dadosabertos"))
	default:
		http.NotFound(w, r)
	}
}

// serveFile answers GET and HEAD for one of files (Content-Length, ETag,
// Range and If-Range through http.ServeContent).
func serveFile(w http.ResponseWriter, r *http.Request, files map[string]file, name string, mod time.Time) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	f, ok := files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("ETag", f.etag)
	w.Header().Set("Content-Type", "application/zip")
	http.ServeContent(w, r, "", mod, bytes.NewReader(f.body))
}

// month is the Receita folder served now: the current month, UTC.
func (s *Server) month() string { return s.now().UTC().Format("2006-01") }

func (s *Server) serveReceita(w http.ResponseWriter, r *http.Request) {
	// The real share is reached with its token as the Basic auth user.
	if user, _, ok := r.BasicAuth(); !ok || user == "" {
		w.Header().Set("WWW-Authenticate", `Basic realm="share"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	month := s.month()
	root, dir := "/receita/", "/receita/"+month+"/"
	switch {
	case r.Method == "PROPFIND" && (r.URL.Path == root || r.URL.Path == dir):
		if r.Header.Get("Depth") != "1" {
			http.Error(w, "Depth 1 only", http.StatusBadRequest)
			return
		}
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="utf-8"?><d:multistatus xmlns:d="DAV:">`)
		folder := func(href string) {
			fmt.Fprintf(&b, `<d:response><d:href>%s</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, href)
		}
		folder(r.URL.Path)
		if r.URL.Path == root {
			folder(dir)
		} else {
			lastMod := s.started.Format(http.TimeFormat)
			for _, n := range s.receitaN {
				f := s.receita[n]
				fmt.Fprintf(&b, `<d:response><d:href>%s%s</d:href><d:propstat><d:prop><d:getcontentlength>%d</d:getcontentlength><d:getetag>%s</d:getetag><d:getlastmodified>%s</d:getlastmodified><d:resourcetype/></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`,
					dir, n, len(f.body), f.etag, lastMod)
			}
		}
		b.WriteString(`</d:multistatus>`)
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(b.String()))
	case r.Method == "PROPFIND":
		http.NotFound(w, r)
	case receitaFile.MatchString(r.URL.Path):
		// Any month folder serves the same files, so a file listed just
		// before a month rollover can still be downloaded after it.
		serveFile(w, r, s.receita, receitaFile.FindStringSubmatch(r.URL.Path)[1], s.started)
	default:
		http.NotFound(w, r)
	}
}

var (
	receitaFile       = regexp.MustCompile(`^/receita/[0-9]{4}-(?:0[1-9]|1[0-2])/([^/]+)$`)
	camaraLegislatura = regexp.MustCompile(`^/legislaturas/([0-9]+)$`)
	camaraDeputado    = regexp.MustCompile(`^/deputados/([0-9]+)$`)
	camaraHistorico   = regexp.MustCompile(`^/deputados/([0-9]+)/historico$`)
	senadoLista       = regexp.MustCompile(`^/senador/lista/legislatura/([0-9]+)$`)
	senadoFiliacoes   = regexp.MustCompile(`^/senador/([0-9]+)/filiacoes$`)
)

// serveCamara maps the API routes to the recorded responses, as the
// connector's own test server does; lists are paged by "pagina".
func (s *Server) serveCamara(w http.ResponseWriter, r *http.Request, p string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var name string
	switch {
	case camaraLegislatura.MatchString(p):
		name = "legislaturas_" + camaraLegislatura.FindStringSubmatch(p)[1] + ".json"
	case camaraDeputado.MatchString(p):
		name = "deputado_" + camaraDeputado.FindStringSubmatch(p)[1] + ".json"
	case camaraHistorico.MatchString(p):
		name = "deputado_" + camaraHistorico.FindStringSubmatch(p)[1] + "_historico.json"
	case p == "/deputados" || p == "/partidos":
		q := r.URL.Query()
		list, order := "deputados", "id"
		if p == "/partidos" {
			list, order = "partidos", "sigla"
		}
		if q.Get("itens") != "100" || q.Get("ordenarPor") != order || q.Get("ordem") != "ASC" {
			http.Error(w, "unexpected query", http.StatusBadRequest)
			return
		}
		name = list + "_" + q.Get("idLegislatura") + "_p" + q.Get("pagina") + ".json"
	}
	serveJSON(w, r, s.camara, name)
}

func (s *Server) serveSenado(w http.ResponseWriter, r *http.Request, p string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var name string
	switch {
	case senadoLista.MatchString(p):
		name = "senadores_" + senadoLista.FindStringSubmatch(p)[1] + ".json"
	case senadoFiliacoes.MatchString(p):
		name = "senador_" + senadoFiliacoes.FindStringSubmatch(p)[1] + "_filiacoes.json"
	}
	serveJSON(w, r, s.senado, name)
}

func serveJSON(w http.ResponseWriter, r *http.Request, files map[string][]byte, name string) {
	if !strings.Contains(r.Header.Get("Accept"), "application/json") {
		http.Error(w, "JSON only (Accept: application/json)", http.StatusNotAcceptable)
		return
	}
	b, ok := files[name]
	if name == "" || !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(b)
}
