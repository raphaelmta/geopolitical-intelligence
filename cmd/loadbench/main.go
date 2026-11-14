// Command loadbench is tooling for the load benchmark (make bench-load,
// scripts/bench-load.sh, docs/performance/benchmark.md). It is not part of
// the geoint binary or of deploy/compose.yml.
//
//	loadbench sample -src DIR -out DIR -uf GO[,DF] [-parts 4] [-churn 1]
//	    Cuts a self-consistent sample out of a downloaded Receita edition
//	    (the 30 data zips in -src): every company whose head office is in
//	    one of the states, with its establishments and partner rows, copied
//	    byte for byte. It writes two simulated editions under -out:
//	    2026-10/ (the first load) and 2026-11/ (the monthly re-load), which
//	    differ by -churn percent of companies and of partner rows in each
//	    direction (rows only in the second edition are new; rows only in the
//	    first are closed by the re-load). Prints counts only, never rows.
//
//	loadbench share -dir DIR [-addr :8080]
//	    Serves DIR (month folders of zips, as written by sample) the way the
//	    Receita share answers: PROPFIND Depth 1 on the root and on a month
//	    folder, GET with Range on the files.
//
// The sample files hold personal data as published by the Receita: keep them
// under data/ (git-ignored) and never copy rows anywhere else.
package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: loadbench sample|share [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "sample":
		err = sample(os.Args[2:])
	case "share":
		err = share(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadbench:", err)
		os.Exit(1)
	}
}

// The two simulated editions and their publication dates (the files'
// modification times, which the connector reads as Last-Modified).
var editions = []struct {
	month     string
	published time.Time
}{
	{"2026-10", time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)},
	{"2026-11", time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)},
}

const maxBase = 100_000_000 // cnpj_basico has 8 digits

// records yields the CSV records of r byte for byte: a record ends at a line
// feed outside quotes (an even number of double quotes so far).
func records(r io.Reader, fn func(rec []byte) error) error {
	br := bufio.NewReaderSize(r, 1<<20)
	var pending []byte
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			rec := line
			if pending != nil {
				pending = append(pending, line...)
				rec = pending
			}
			if bytes.Count(rec, []byte{'"'})%2 == 0 || err != nil {
				if ferr := fn(rec); ferr != nil {
					return ferr
				}
				pending = nil
			} else if pending == nil {
				pending = append([]byte(nil), line...)
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// base returns the cnpj_basico of a record ("12345678";...), or -1.
func base(rec []byte) int {
	if len(rec) < 10 || rec[0] != '"' || rec[9] != '"' {
		return -1
	}
	n := 0
	for _, c := range rec[1:9] {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func bucket(b []byte) uint32 {
	h := fnv.New32a()
	_, _ = h.Write(b)
	// The low bits of FNV-1a over similar keys are weak; mix before the modulo.
	x := h.Sum32()
	x ^= x >> 15
	x *= 0x2c1b3c6d
	x ^= x >> 12
	return x % 100
}

// eachPart streams every part of a kind (Empresas0.zip ...) in order.
func eachPart(src, kind string, fn func(inner string, rec []byte) error) (int, error) {
	parts := 0
	for i := 0; ; i++ {
		name := filepath.Join(src, fmt.Sprintf("%s%d.zip", kind, i))
		zr, err := zip.OpenReader(name)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return parts, err
		}
		if len(zr.File) != 1 {
			zr.Close()
			return parts, fmt.Errorf("%s: %d files in the zip, want 1", name, len(zr.File))
		}
		rc, err := zr.File[0].Open()
		if err != nil {
			zr.Close()
			return parts, err
		}
		inner := filepath.Base(zr.File[0].Name)
		err = records(rc, func(rec []byte) error { return fn(inner, rec) })
		rc.Close()
		zr.Close()
		if err != nil {
			return parts, fmt.Errorf("%s: %w", name, err)
		}
		parts++
	}
	if parts == 0 {
		return 0, fmt.Errorf("no %s0.zip in %s", kind, src)
	}
	return parts, nil
}

// partWriter spreads records over n zips of one kind in one edition folder.
type partWriter struct {
	files []*os.File
	zips  []*zip.Writer
	outs  []io.Writer
	rows  int64
}

func newPartWriter(dir, kind string, n int) (*partWriter, error) {
	w := &partWriter{}
	for i := range n {
		f, err := os.Create(filepath.Join(dir, fmt.Sprintf("%s%d.zip", kind, i)))
		if err != nil {
			return nil, err
		}
		zw := zip.NewWriter(f)
		// The inner name is the file part of every claim's record_ref.
		out, err := zw.Create(fmt.Sprintf("BENCH.SAMPLE.%s%d.CSV", strings.ToUpper(kind), i))
		if err != nil {
			return nil, err
		}
		w.files, w.zips, w.outs = append(w.files, f), append(w.zips, zw), append(w.outs, out)
	}
	return w, nil
}

func (w *partWriter) write(b int, rec []byte) error {
	w.rows++
	// By company, so a company's rows stay in one part.
	_, err := w.outs[b%len(w.outs)].Write(rec)
	return err
}

func (w *partWriter) close(mod time.Time) error {
	for i, zw := range w.zips {
		if err := zw.Close(); err != nil {
			return err
		}
		name := w.files[i].Name()
		if err := w.files[i].Close(); err != nil {
			return err
		}
		if err := os.Chtimes(name, mod, mod); err != nil {
			return err
		}
	}
	return nil
}

func sample(args []string) error {
	fs := flag.NewFlagSet("sample", flag.ContinueOnError)
	src := fs.String("src", "", "folder with the downloaded edition (Empresas0.zip ...)")
	out := fs.String("out", "", "folder to write the two sample editions into")
	ufs := fs.String("uf", "", "comma-separated states (head office) to keep, e.g. GO,DF")
	parts := fs.Int("parts", 4, "zips per kind in each edition")
	churn := fs.Int("churn", 1, "percent of companies and partner rows that differ between the editions, each way")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *src == "" || *out == "" || *ufs == "" || *parts < 1 || *churn < 0 || *churn > 20 {
		return errors.New("sample: -src, -out and -uf are required; -parts >= 1; -churn 0..20")
	}
	keep := map[string]bool{}
	for _, u := range strings.Split(*ufs, ",") {
		keep[strings.ToUpper(strings.TrimSpace(u))] = true
	}
	start := time.Now()

	// Pass 1: the companies whose head office is in the chosen states.
	selected := make([]byte, maxBase)
	var estabRows, heads, odd int64
	sep := []byte(`";"`)
	if _, err := eachPart(*src, "Estabelecimentos", func(_ string, rec []byte) error {
		estabRows++
		b := base(rec)
		// "base";"ordem";"dv";"1";... : the head-office flag is at a fixed offset.
		if b < 0 || len(rec) < 26 || rec[24] != '1' || rec[23] != '"' || rec[25] != '"' {
			return nil
		}
		f := bytes.Split(rec, sep)
		if len(f) != 30 {
			odd++
			return nil
		}
		if keep[string(f[19])] {
			selected[b] = 1
			heads++
		}
		return nil
	}); err != nil {
		return err
	}
	fmt.Printf("estabelecimentos: %d rows read, %d head offices selected, %d head-office rows with an unexpected field count skipped (%s)\n",
		estabRows, heads, odd, time.Since(start).Round(time.Second))

	// Per company: 0 = only in the second edition (new), 1 = only in the
	// first (disappears), anything else = in both.
	inEdition := func(ed int, h uint32) bool {
		c := uint32(*churn)
		if h < c {
			return ed == 1
		}
		if h < 2*c {
			return ed == 0
		}
		return true
	}
	for _, e := range editions {
		if err := os.MkdirAll(filepath.Join(*out, e.month), 0o755); err != nil {
			return err
		}
	}
	for _, kind := range []string{"Empresas", "Estabelecimentos", "Socios"} {
		var ws [2]*partWriter
		for i, e := range editions {
			w, err := newPartWriter(filepath.Join(*out, e.month), kind, *parts)
			if err != nil {
				return err
			}
			ws[i] = w
		}
		var read int64
		if _, err := eachPart(*src, kind, func(_ string, rec []byte) error {
			read++
			b := base(rec)
			if b < 0 || selected[b] == 0 {
				return nil
			}
			company := bucket(rec[1:9])
			for i := range editions {
				if !inEdition(i, company) {
					continue
				}
				// Partner rows of a company present in both editions also come and go.
				if kind == "Socios" && company >= uint32(2**churn) && !inEdition(i, bucket(rec)) {
					continue
				}
				if err := ws[i].write(b, rec); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		for i, e := range editions {
			if err := ws[i].close(e.published); err != nil {
				return err
			}
			fmt.Printf("%s: %d rows read, %d written to edition %s (%s)\n", kind, read, ws[i].rows, e.month,
				time.Since(start).Round(time.Second))
		}
	}
	return nil
}

var (
	monthPath = regexp.MustCompile(`^/([0-9]{4}-(?:0[1-9]|1[0-2]))/$`)
	filePath  = regexp.MustCompile(`^/([0-9]{4}-(?:0[1-9]|1[0-2]))/([A-Za-z]+[0-9]+\.zip)$`)
)

func share(args []string) error {
	fs := flag.NewFlagSet("share", flag.ContinueOnError)
	dir := fs.String("dir", "", "folder holding the month folders")
	addr := fs.String("addr", ":8080", "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("share: -dir is required")
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/healthz":
			_, _ = io.WriteString(w, "ok\n")
		case r.Method == "PROPFIND" && r.URL.Path == "/":
			entries, err := os.ReadDir(*dir)
			if err != nil {
				http.Error(w, "list", http.StatusInternalServerError)
				return
			}
			var hrefs []string
			for _, e := range entries {
				if e.IsDir() && monthPath.MatchString("/"+e.Name()+"/") {
					hrefs = append(hrefs, "/"+e.Name()+"/")
				}
			}
			multistatus(w, "/", hrefs, nil)
		case r.Method == "PROPFIND" && monthPath.MatchString(r.URL.Path):
			month := monthPath.FindStringSubmatch(r.URL.Path)[1]
			entries, err := os.ReadDir(filepath.Join(*dir, month))
			if err != nil {
				http.NotFound(w, r)
				return
			}
			var files []os.FileInfo
			for _, e := range entries {
				if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() && strings.HasSuffix(fi.Name(), ".zip") {
					files = append(files, fi)
				}
			}
			sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })
			multistatus(w, r.URL.Path, nil, files)
		case (r.Method == http.MethodGet || r.Method == http.MethodHead) && filePath.MatchString(r.URL.Path):
			m := filePath.FindStringSubmatch(r.URL.Path)
			f, err := os.Open(filepath.Join(*dir, m[1], m[2]))
			if err != nil {
				http.NotFound(w, r)
				return
			}
			defer f.Close()
			fi, err := f.Stat()
			if err != nil {
				http.Error(w, "stat", http.StatusInternalServerError)
				return
			}
			w.Header().Set("ETag", etag(fi))
			w.Header().Set("Content-Type", "application/zip")
			http.ServeContent(w, r, "", fi.ModTime(), f)
		default:
			http.NotFound(w, r)
		}
	})
	srv := &http.Server{Addr: *addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	fmt.Fprintln(os.Stderr, "loadbench: serving", *dir, "on", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func etag(fi os.FileInfo) string {
	return fmt.Sprintf(`"%x-%x"`, fi.ModTime().Unix(), fi.Size())
}

func multistatus(w http.ResponseWriter, self string, folders []string, files []os.FileInfo) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><d:multistatus xmlns:d="DAV:">`)
	folder := func(href string) {
		fmt.Fprintf(&b, `<d:response><d:href>%s</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, href)
	}
	folder(self)
	for _, f := range folders {
		folder(f)
	}
	for _, fi := range files {
		fmt.Fprintf(&b, `<d:response><d:href>%s%s</d:href><d:propstat><d:prop><d:getcontentlength>%d</d:getcontentlength><d:getetag>%s</d:getetag><d:getlastmodified>%s</d:getlastmodified><d:resourcetype/></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`,
			self, fi.Name(), fi.Size(), etag(fi), fi.ModTime().UTC().Format(http.TimeFormat))
	}
	b.WriteString(`</d:multistatus>`)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = io.WriteString(w, b.String())
}
