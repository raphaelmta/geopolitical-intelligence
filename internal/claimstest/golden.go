package claimstest

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Golden files hold the canonical dump of a scenario as recorded from the
// code path that was current when they were written. A change to the
// loader, the resolver or the search rebuild that must leave the final state
// unchanged (tasks 2 to 9 of the load optimisation) has to pass WITHOUT
// regenerating them. Regenerate only for a change whose difference is
// intended and reviewed in the diff of the golden file:
//
//	make test PKG='./internal/connector/receita ./internal/connector/tse -update' RUN=TestEquivalence TEST_PROJECT=<name>
//
// (go test passes -update, written after the packages, to the test binaries),
// or UPDATE_GOLDEN=1 with a plain go test.
var update = flag.Bool("update", false, "rewrite the golden files of the equivalence tests (internal/claimstest)")

// Updating reports whether golden files are being rewritten.
func Updating() bool { return *update || os.Getenv("UPDATE_GOLDEN") == "1" }

// Mask rewrites one line of a section before it is compared; it is for the
// few fields a scenario cannot fix by construction (see
// MaskRelationshipFirstCollection). nil masks nothing.
type Mask func(section, line string) string

// MaskRelationshipFirstCollection replaces the first collection of every
// relationship by "*". For a load interrupted between two relationship
// chunks and repeated, which relationships the failed run wrote depends on
// how claims are cut into chunks (task 2 changes it), so the first collection
// of a relationship is not a property of the final state there; everything
// else is.
func MaskRelationshipFirstCollection(section, line string) string {
	if section != SecRelationship {
		return line
	}
	f := strings.Split(line, FieldSep)
	if len(f) > relationshipFirstCol {
		f[relationshipFirstCol] = "*"
	}
	return strings.Join(f, FieldSep)
}

// apply returns sections with mask applied to every line (lines re-sorted).
func apply(sections map[string]string, mask Mask) map[string]string {
	if mask == nil {
		return sections
	}
	out := make(map[string]string, len(sections))
	for name, text := range sections {
		lines := splitLines(text)
		for i, l := range lines {
			lines[i] = mask(name, l)
		}
		sortStrings(lines)
		out[name] = joinLines(lines)
	}
	return out
}

// CheckGolden compares sections (masked by mask) with the golden file at
// path, or rewrites the file when Updating. On a difference it reports every
// differing section with its first missing and extra lines.
func CheckGolden(t *testing.T, path string, sections map[string]string, mask Mask, note string) {
	t.Helper()
	got := apply(sections, mask)
	if Updating() {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(renderGolden(got, note)), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("golden %s rewritten (sha256 %s)", path, Hash(got))
		return
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("golden %s does not exist; record it from the current code with -update (internal/claimstest/golden.go)", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	want, err := parseGolden(string(b))
	if err != nil {
		t.Fatalf("golden %s: %v", path, err)
	}
	if Hash(want) == Hash(got) {
		return
	}
	t.Errorf("canonical dump differs from golden %s (want sha256 %s, got %s)%s", path, Hash(want), Hash(got), Diff(want, got))
}

// Diff describes how two dumps differ: for each section that differs, its
// line counts and up to maxDiffLines lines missing from b and extra in b
// (as multisets). Empty when they are equal.
func Diff(a, b map[string]string) string {
	var out strings.Builder
	names := map[string]bool{}
	for n := range a {
		names[n] = true
	}
	for n := range b {
		names[n] = true
	}
	all := make([]string, 0, len(names))
	for n := range names {
		all = append(all, n)
	}
	sortStrings(all)
	for _, n := range all {
		if a[n] == b[n] {
			continue
		}
		al, bl := splitLines(a[n]), splitLines(b[n])
		missing, extra := multisetDiff(al, bl)
		fmt.Fprintf(&out, "\n  section %s differs: %d lines want, %d got; %d missing, %d extra", n, len(al), len(bl), len(missing), len(extra))
		for i, l := range missing {
			if i == maxDiffLines {
				fmt.Fprintf(&out, "\n    - … %d more", len(missing)-i)
				break
			}
			fmt.Fprintf(&out, "\n    - %s", shorten(l))
		}
		for i, l := range extra {
			if i == maxDiffLines {
				fmt.Fprintf(&out, "\n    + … %d more", len(extra)-i)
				break
			}
			fmt.Fprintf(&out, "\n    + %s", shorten(l))
		}
	}
	return out.String()
}

const (
	maxDiffLines = 8
	maxLineLen   = 400
)

func shorten(l string) string {
	l = strings.ReplaceAll(l, FieldSep, " ¦ ")
	if len(l) > maxLineLen {
		return l[:maxLineLen] + "…"
	}
	return l
}

// multisetDiff returns the lines of a not in b and of b not in a, counting
// repeats; both inputs are sorted.
func multisetDiff(a, b []string) (missing, extra []string) {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			i++
			j++
		case a[i] < b[j]:
			missing = append(missing, a[i])
			i++
		default:
			extra = append(extra, b[j])
			j++
		}
	}
	missing = append(missing, a[i:]...)
	extra = append(extra, b[j:]...)
	return missing, extra
}

// Golden file format: comment lines starting with "#", then every section as
// in Render. Section headers carry the line count and hash for review only;
// the comparison reads the lines.
const goldenHeader = `# Canonical claims dump of an equivalence scenario (internal/claimstest).
# Recorded from the code path current when it was written; a change meant to
# keep the final state must pass without rewriting it. Rewrite (-update or
# UPDATE_GOLDEN=1) only for an intended difference, reviewed in this diff.
`

func renderGolden(sections map[string]string, note string) string {
	var b strings.Builder
	b.WriteString(goldenHeader)
	for _, l := range splitLines(note) {
		fmt.Fprintf(&b, "# %s\n", l)
	}
	fmt.Fprintf(&b, "# sha256 %s\n", Hash(sections))
	for _, name := range sectionNames(sections) {
		lines := splitLines(sections[name])
		fmt.Fprintf(&b, "# %-30s %4d lines  sha256 %s\n", name, len(lines), sha(sections[name]))
	}
	b.WriteString(Render(sections))
	return b.String()
}

func parseGolden(s string) (map[string]string, error) {
	out := map[string]string{}
	var cur string
	var lines []string
	flush := func() {
		if cur != "" {
			out[cur] = joinLines(lines)
		}
	}
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 0, 1<<16), 1<<24)
	for sc.Scan() {
		l := sc.Text()
		switch {
		case cur == "" && strings.HasPrefix(l, "#"):
			continue
		case strings.HasPrefix(l, "== ") && strings.HasSuffix(l, " =="):
			flush()
			cur, lines = strings.TrimSuffix(strings.TrimPrefix(l, "== "), " =="), nil
		case cur == "":
			return nil, fmt.Errorf("line outside a section: %q", l)
		default:
			lines = append(lines, l)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	flush()
	return out, nil
}

// Equivalence runs a scenario twice and checks its final state:
//   - run(t, false) on a fresh schema, run(t, true) on another fresh schema
//     whose id sequences were moved by ShiftIDs: the two canonical dumps must
//     be identical (the dump is deterministic and id-independent);
//   - after each run, the live search tables must equal a from-scratch
//     rebuild (CheckSearchMatchesRebuild);
//   - the dump of the first run must match the golden file (CheckGolden,
//     with mask; note goes into the file's header).
//
// run returns the pool of the schema it loaded.
func Equivalence(t *testing.T, golden string, mask Mask, note string, run func(t *testing.T, shiftIDs bool) *pgxpool.Pool) {
	t.Helper()
	ctx := t.Context()
	dumps := make([]map[string]string, 2)
	for i, shift := range []bool{false, true} {
		pool := run(t, shift)
		_, d, err := CanonicalDump(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		dumps[i] = d
		CheckSearchMatchesRebuild(t, ctx, pool)
	}
	if d := Diff(dumps[0], dumps[1]); d != "" {
		t.Errorf("canonical dump is not deterministic: a second run with shifted ids differs (- first, + second):%s", d)
	}
	CheckGolden(t, golden, dumps[0], mask, note)
}
