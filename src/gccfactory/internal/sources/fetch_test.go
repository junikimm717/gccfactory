package sources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// serve returns a server handing out body at /f, plus a hit counter.
func serve(t *testing.T, body []byte) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestFetchDownloadsAndIsIdempotent(t *testing.T) {
	body := []byte("the quick brown fox\n")
	srv, hits := serve(t, body)
	dist := t.TempDir()
	s := Source{Name: "probe", Version: "1", File: "probe-1.tar.gz",
		URLs: []string{srv.URL + "/f"}, SHA256: sum(body), TopDir: "probe-1"}

	p, err := Fetch(context.Background(), dist, s)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dist, "src", s.SHA256[:16]+"-probe-1.tar.gz"); p != want {
		t.Errorf("path = %q, want %q", p, want)
	}
	got, err := os.ReadFile(p)
	if err != nil || string(got) != string(body) {
		t.Fatalf("content = %q, err = %v", got, err)
	}

	// Second call must be a no-op: no new HTTP request.
	if _, err := Fetch(context.Background(), dist, s); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Errorf("server hit %d times, want 1 (Fetch is not idempotent)", n)
	}
}

func TestFetchChecksumMismatch(t *testing.T) {
	srv, _ := serve(t, []byte("actual bytes"))
	dist := t.TempDir()
	bogus := strings.Repeat("0", 64)
	s := Source{Name: "probe", Version: "1", File: "probe-1.tar.gz",
		URLs: []string{srv.URL + "/f"}, SHA256: bogus, TopDir: "probe-1"}

	_, err := Fetch(context.Background(), dist, s)
	if err == nil {
		t.Fatal("expected a checksum error")
	}
	msg := err.Error()
	for _, want := range []string{"sha256 mismatch", bogus, sum([]byte("actual bytes")), srv.URL} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q:\n%s", want, msg)
		}
	}
	// Nothing may be published under a wrong hash.
	if _, err := os.Stat(Path(dist, s)); !errors.Is(err, os.ErrNotExist) {
		t.Error("a mismatching download was published anyway")
	}
	ents, _ := os.ReadDir(filepath.Join(dist, "src"))
	for _, e := range ents {
		if strings.Contains(e.Name(), ".part-") {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
}

func TestFetchFallsBackToSecondURL(t *testing.T) {
	body := []byte("mirror payload")
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer dead.Close()
	good, hits := serve(t, body)
	dist := t.TempDir()
	s := Source{Name: "probe", Version: "1", File: "probe-1.tar.gz",
		URLs: []string{dead.URL + "/f", good.URL + "/f"}, SHA256: sum(body), TopDir: "probe-1"}

	p, err := Fetch(context.Background(), dist, s)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != string(body) {
		t.Errorf("content = %q", got)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Errorf("good mirror hit %d times, want 1", n)
	}
}

func TestFetchAllMirrorsFailNamesThem(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer dead.Close()
	s := Source{Name: "probe", Version: "1", File: "probe-1.tar.gz",
		URLs: []string{dead.URL + "/a", dead.URL + "/b"}, SHA256: sum([]byte("x")), TopDir: "probe-1"}

	_, err := Fetch(context.Background(), t.TempDir(), s)
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, want := range []string{dead.URL + "/a", dead.URL + "/b", "404"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%s", want, err)
		}
	}
}

// Two concurrent Fetches for the same source must produce exactly one file and
// never let a reader see a partial one.
func TestFetchConcurrent(t *testing.T) {
	body := []byte(strings.Repeat("payload chunk ", 4096))
	srv, hits := serve(t, body)
	dist := t.TempDir()
	s := Source{Name: "probe", Version: "1", File: "probe-1.tar.gz",
		URLs: []string{srv.URL + "/f"}, SHA256: sum(body), TopDir: "probe-1"}

	const n = 8
	var wg sync.WaitGroup
	paths := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			paths[i], errs[i] = Fetch(context.Background(), dist, s)
		}(i)
	}
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if paths[i] != paths[0] {
			t.Fatalf("goroutine %d got %q, goroutine 0 got %q", i, paths[i], paths[0])
		}
	}
	got, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if sum(got) != s.SHA256 {
		t.Fatalf("published file is corrupt: %d bytes", len(got))
	}
	if h := atomic.LoadInt32(hits); h != 1 {
		t.Errorf("downloaded %d times, want 1 (the lock is not serializing)", h)
	}
	ents, _ := os.ReadDir(filepath.Join(dist, "src"))
	var payloads int
	for _, e := range ents {
		if strings.Contains(e.Name(), ".part-") {
			t.Errorf("temp file %s left behind", e.Name())
		}
		if strings.HasSuffix(e.Name(), ".tar.gz") {
			payloads++
		}
	}
	if payloads != 1 {
		t.Errorf("%d payload files in dist/src, want 1", payloads)
	}
}

// A cached file whose content no longer matches the pin is re-downloaded, not
// trusted. (This is what a truncated download from a killed run looks like.)
func TestFetchRepairsCorruptCache(t *testing.T) {
	body := []byte("good content")
	srv, _ := serve(t, body)
	dist := t.TempDir()
	s := Source{Name: "probe", Version: "1", File: "probe-1.tar.gz",
		URLs: []string{srv.URL + "/f"}, SHA256: sum(body), TopDir: "probe-1"}

	if err := os.MkdirAll(CacheDir(dist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(dist, s), []byte("truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Fetch(context.Background(), dist, s)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != string(body) {
		t.Errorf("corrupt cache entry was not repaired: %q", got)
	}
}
