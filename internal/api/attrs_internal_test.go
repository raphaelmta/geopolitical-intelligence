package api

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"geoint/internal/claims"
	"geoint/internal/graph"
)

// TestEdgeAttrs converts realistic attrs of every relationship type the
// connectors write and checks the exact JSON the API sends: identifiers and
// codes unchanged (an 11-digit receipt id is not a CPF), free text masked,
// values of the wrong shape and nulls dropped.
func TestEdgeAttrs(t *testing.T) {
	cases := []struct {
		name  string
		typ   claims.RelType
		attrs string // as stored (jsonb)
		want  string // attrs as sent
	}{
		{"receita", claims.PartnerOf, `{"role":"22"}`, `{"role":"22"}`},
		{"candidacy", claims.CandidateFor,
			`{"year":2024,"office":"VEREADOR 12345678901","office_code":"13","uf":"SP","city":"71072",
			  "city_name":"SÃO PAULO 12345678901","result":"ELEITO","ballot_name":"JOANA 12345678901",
			  "role":"2024:13:SP:71072"}`,
			`{"ballot_name":"JOANA ***456789**","city":"71072","city_name":"SÃO PAULO ***456789**",
			  "office":"VEREADOR ***456789**","office_code":"13","result":"ELEITO","role":"2024:13:SP:71072",
			  "uf":"SP","year":2024}`},
		{"state candidacy, no city", claims.CandidateFor,
			`{"year":2022,"office":"DEPUTADO FEDERAL","office_code":"6","uf":"MG","city":"","city_name":"","role":"2022:6:MG:"}`,
			`{"city_name":"","office":"DEPUTADO FEDERAL","office_code":"6","role":"2022:6:MG:","uf":"MG","year":2022}`},
		{"donation, 11-digit receipt", claims.DonatedTo,
			`{"year":2022,"amount":"123456789012345678.90","date":"2022-09-01","origin":"Recursos de pessoas físicas",
			  "origin_code":"10010100","receipt_id":"12345678901","items":3,"role":"12345678901"}`,
			`{"amount":"123456789012345678.90","date":"2022-09-01","items":3,"origin":"Recursos de pessoas físicas",
			  "origin_code":"10010100","receipt_id":"12345678901","role":"12345678901","year":2022}`},
		{"donation, 12-digit receipt", claims.DonatedTo,
			`{"amount":"10.5","receipt_id":"123456789012","role":"123456789012"}`,
			`{"amount":"10.5","receipt_id":"123456789012","role":"123456789012"}`},
		{"donation, 20-digit receipt", claims.DonatedTo,
			`{"amount":"0.01","receipt_id":"12345678901234567890","role":"12345678901234567890"}`,
			`{"amount":"0.01","receipt_id":"12345678901234567890","role":"12345678901234567890"}`},
		{"mandate", claims.HoldsMandate,
			`{"legislature":57,"uf":"SP","party":"UNIÃO","status":"suplente_exercicio","role":"57:UNIÃO:SP:suplente_exercicio"}`,
			`{"legislature":57,"party":"UNIÃO","role":"57:UNIÃO:SP:suplente_exercicio","status":"suplente_exercicio","uf":"SP"}`},
		{"mandate without party", claims.HoldsMandate,
			`{"legislature":56,"uf":"RJ","party":"","status":"exercicio","role":"56::RJ:exercicio"}`,
			`{"legislature":56,"party":"","role":"56::RJ:exercicio","status":"exercicio","uf":"RJ"}`},
		{"shareholding", claims.ShareholderOf,
			`{"role":"shareholder","reference_date":"2025-04-30","form_version":3,"percent":"10.50",
			  "percent_on":"21.000001","percent_pn":"0","described_by":"90000001"}`,
			`{"described_by":"90000001","form_version":3,"percent":"10.50","percent_on":"21.000001",
			  "percent_pn":"0","reference_date":"2025-04-30","role":"shareholder"}`},
		{"control", claims.Controls, `{"role":"controller","reference_date":"2025-04-30","form_version":1}`,
			`{"form_version":1,"reference_date":"2025-04-30","role":"controller"}`},
		// Unknown keys, nulls, objects, arrays and wrong shapes are dropped.
		{"dropped", claims.DonatedTo,
			`{"role":"22","name":"JOANA FICTICIA","nested":{"role":"x"},"items":[1,2],"percent":null,
			  "date":"ontem","amount":"1e9","status":{"x":1},"uf":"São Paulo","year":"2024.5",
			  "receipt_id":"RECIBO 12345678901","form_version":12345678901234567890,"legislature":true,
			  "reference_date":"2025-02-30","office":7,"described_by":""}`,
			`{"form_version":12345678901234567890,"role":"22"}`},
		{"integer-looking string number", claims.CandidateFor, `{"year":"2024"}`, `{"year":2024}`},
		// Non-canonical integers are written canonically or dropped, never
		// sent as an invalid JSON number (a Marshal error: 500).
		{"non-canonical integers", claims.DonatedTo, `{"year":"007","items":"-0","form_version":"1.0","legislature":"-012"}`,
			`{"items":0,"legislature":-12,"year":7}`},
	}
	for _, c := range cases {
		// Neighbors gets attrs through the driver (float64 numbers); job
		// results through decodeNumbers (json.Number). Both must send the same.
		var asFloat, asNumber map[string]any
		if err := json.Unmarshal([]byte(c.attrs), &asFloat); err != nil {
			t.Fatal(err)
		}
		if err := decodeNumbers(json.RawMessage(c.attrs), &asNumber); err != nil {
			t.Fatal(err)
		}
		want := compact(t, c.want)
		for path, attrs := range map[string]map[string]any{"driver": asFloat, "job result": asNumber} {
			got, err := json.Marshal(toEdge(graph.Edge{Type: c.typ, Attrs: attrs}).Attrs)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != want {
				// float64 cannot hold 12345678901234567890 exactly: only the
				// job-result path is held to it (no connector stores such a number).
				if path == "driver" && strings.Contains(c.attrs, "12345678901234567890,") {
					continue
				}
				t.Errorf("%s (%s):\n got %s\nwant %s", c.name, path, got, want)
			}
		}
	}
}

// TestNumberAttrIsAlwaysValidJSON feeds attrValue the shapes a number can
// arrive in, including a json.Number that is not canonical JSON.
func TestNumberAttrIsAlwaysValidJSON(t *testing.T) {
	for in, want := range map[any]string{
		json.Number("007"): "7", json.Number("-0"): "0", "007": "7", "-0": "0", "-007": "-7", "0": "0",
		float64(-0.0): "0", json.Number("1.0"): "", "1.0": "", "+1": "", " 1": "",
	} {
		v, ok := attrValue(attrNumber, in)
		if want == "" {
			if ok {
				t.Errorf("%#v: kept as %v, want dropped", in, v)
			}
			continue
		}
		b, err := json.Marshal(map[string]any{"n": v})
		if !ok || err != nil || string(b) != `{"n":`+want+`}` {
			t.Errorf("%#v: %s, %v, ok %v; want %s", in, b, err, ok, want)
		}
	}
}

func compact(t *testing.T, s string) string {
	t.Helper()
	var v map[string]any
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestJobResultKeepsAttrs: a stored paths result keeps its edges' attrs.
func TestJobResultKeepsAttrs(t *testing.T) {
	raw := json.RawMessage(`{"paths":[[{"edge":{"relationship_id":1,"type":"donated_to","attrs":
		{"amount":"1000.10","items":2,"name":"X"},"kind":"fact","confidence":null},"forward":true}]],
		"stopped_hubs":[],"truncated":false}`)
	out, err := jobResult("paths", raw)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), `"attrs":{"amount":"1000.10","items":2}`) {
		t.Fatalf("job result attrs: %s", b)
	}
}

// TestEdgeAttrsCoverConnectors fails when a connector writes an attr key
// that is neither shown (edgeAttrs) nor explicitly hidden (deniedAttrs), and
// when edgeAttrs lists a key no connector writes. Keys are read from the
// connectors' source: map[string]any literals, assignments to x.attrs["k"]
// / x.Attrs["k"], and the keys of jsonb_build_object(...) calls in SQL strings.
//
// Limits: it sees only literal keys in those three places. A key built at
// run time (a variable used as map key, a key computed in SQL, attrs decoded
// from a source file) is invisible to it; such a key is still dropped by the
// API (unknown keys never pass), so the failure mode is "not shown", never
// "shown unchecked".
func TestEdgeAttrsCoverConnectors(t *testing.T) {
	found := connectorAttrKeys(t, filepath.Join("..", "connector"))
	t.Logf("attr keys written by connectors: %v", keys(found))
	for _, k := range []string{"role", "amount", "ballot_name", "legislature", "percent_on", "described_by"} {
		if !found[k] {
			t.Fatalf("scan did not find %q; the scan is broken (found %v)", k, keys(found))
		}
	}
	for k := range found {
		if _, ok := edgeAttrs[k]; !ok && !deniedAttrs[k] {
			t.Errorf("a connector writes attr %q: add it to edgeAttrs (shown) or deniedAttrs (hidden)", k)
		}
	}
	for k := range edgeAttrs {
		if !found[k] {
			t.Errorf("edgeAttrs lists %q, which no connector writes", k)
		}
		if kind := edgeAttrs[k]; kind < attrText || kind > attrUF {
			t.Errorf("%q has no valid kind", k)
		}
		if deniedAttrs[k] {
			t.Errorf("%q is both allowed and denied", k)
		}
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

var jsonbKey = regexp.MustCompile(`'([a-z_][a-z0-9_]*)'\s*,`)

func connectorAttrKeys(t *testing.T, root string) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BasicLit:
				if x.Kind == token.STRING {
					if s, err := strconv.Unquote(x.Value); err == nil {
						for _, args := range jsonbArgs(s) {
							for _, m := range jsonbKey.FindAllStringSubmatch(args, -1) {
								found[m[1]] = true
							}
						}
					}
				}
			case *ast.CompositeLit:
				if isAnyMap(x.Type) {
					for _, el := range x.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if k := stringLit(kv.Key); k != "" {
								found[k] = true
							}
						}
					}
				}
			case *ast.IndexExpr:
				var name string
				switch v := x.X.(type) {
				case *ast.SelectorExpr:
					name = v.Sel.Name
				case *ast.Ident:
					name = v.Name
				}
				if strings.EqualFold(name, "attrs") {
					if k := stringLit(x.Index); k != "" {
						found[k] = true
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// jsonbArgs returns the argument text of every jsonb_build_object(...) in s.
func jsonbArgs(s string) []string {
	var out []string
	for {
		i := strings.Index(s, "jsonb_build_object(")
		if i < 0 {
			return out
		}
		s = s[i+len("jsonb_build_object("):]
		depth, end := 1, len(s)
		for j, r := range s {
			if r == '(' {
				depth++
			} else if r == ')' {
				if depth--; depth == 0 {
					end = j
					break
				}
			}
		}
		// A trailing "'key', value" has no comma after the value; the key
		// regexp needs one after the key only, which holds for every pair.
		out = append(out, s[:end])
		s = s[end:]
	}
}

func isAnyMap(e ast.Expr) bool {
	m, ok := e.(*ast.MapType)
	if !ok {
		return false
	}
	if k, ok := m.Key.(*ast.Ident); !ok || k.Name != "string" {
		return false
	}
	switch v := m.Value.(type) {
	case *ast.Ident:
		return v.Name == "any"
	case *ast.InterfaceType:
		return v.Methods == nil || len(v.Methods.List) == 0
	}
	return false
}

func stringLit(e ast.Expr) string {
	b, ok := e.(*ast.BasicLit)
	if !ok || b.Kind != token.STRING {
		return ""
	}
	s, err := strconv.Unquote(b.Value)
	if err != nil {
		return ""
	}
	return s
}
