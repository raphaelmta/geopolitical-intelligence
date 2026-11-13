package legis

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestPartyKeyMatchesTheTSEKey(t *testing.T) {
	for in, want := range map[string]string{
		" pt ":      "PT",
		"UNIÃO":     "UNIÃO",
		"PCdoB":     "PCDOB",
		"PC do B":   "PC DO B", // an older TSE spelling: kept apart, as in tse
		"S.PART.":   "",        // Câmara: without party
		"S/Partido": "",        // Senado: without party
		"":          "",
	} {
		if got := PartyKey(in); got != want {
			t.Errorf("PartyKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseDate(t *testing.T) {
	for in, want := range map[string]string{
		"2023-02-01":          "2023-02-01",
		"2023-01-31T23:59":    "2023-01-31",
		"2023-01-31 10:00:00": "2023-01-31",
		"2024-13-45":          "",
		"31/01/2023":          "",
		"2023-02-01X":         "",
		"":                    "",
	} {
		got, ok := ParseDate(in)
		if (want == "") == ok || (ok && got.Format(time.DateOnly) != want) {
			t.Errorf("ParseDate(%q) = %v, %v; want %q", in, got, ok, want)
		}
	}
}

func TestManyAcceptsObjectOrArray(t *testing.T) {
	var v struct {
		A, B, C Many[struct{ X int }]
	}
	if err := json.Unmarshal([]byte(`{"A": [{"X": 1}, {"X": 2}], "B": {"X": 3}, "C": null}`), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.A.Items) != 2 || v.A.Single || v.A.Pointer(1) != "/1" {
		t.Errorf("array: %+v", v.A)
	}
	if len(v.B.Items) != 1 || !v.B.Single || v.B.Items[0].X != 3 || v.B.Pointer(0) != "" {
		t.Errorf("object: %+v", v.B)
	}
	if len(v.C.Items) != 0 {
		t.Errorf("null: %+v", v.C)
	}
}

func TestRetryAfter(t *testing.T) {
	if got := retryAfter("7"); got != 7*time.Second {
		t.Errorf("seconds: %v", got)
	}
	if got := retryAfter("soon"); got != 0 {
		t.Errorf("garbage: %v", got)
	}
	if got := retryAfter(time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)); got < 50*time.Minute {
		t.Errorf("date: %v", got)
	}
}

func TestRefKeepsTheLastColonAsSeparator(t *testing.T) {
	if got := Ref("a.json", "/x:y/1"); got != "a.json:/x_y/1" {
		t.Errorf("Ref = %q", got)
	}
}

func TestRedirectKeepsHostAndScheme(t *testing.T) {
	req := func(u string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	first := []*http.Request{req("https://api.example.invalid/a")}
	for u, ok := range map[string]bool{
		"https://api.example.invalid/b":   true,
		"http://api.example.invalid/b":    false, // https -> http
		"https://other.example.invalid/b": false,
	} {
		if err := checkRedirect(req(u), first); (err == nil) != ok {
			t.Errorf("%s: %v", u, err)
		}
	}
}
