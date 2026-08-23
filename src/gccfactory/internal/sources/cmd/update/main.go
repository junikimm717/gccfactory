// Command update re-downloads every pinned source, recomputes its sha256 and
// rewrites internal/sources/sources.json.
//
// It never invents a checksum: every value it writes comes from bytes it just
// pulled over the network. Run it through src/gccfactory/update-sources.sh.
//
//	update -json <path> [-only <name>] [-check]
//
//	-check   verify only; print drift and exit 1 without writing anything.
//	-only    restrict to a single source by name.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/sources"
)

func main() {
	jsonPath := flag.String("json", "", "path to sources.json (required)")
	only := flag.String("only", "", "only refresh this source name")
	check := flag.Bool("check", false, "verify only; exit non-zero on drift")
	flag.Parse()

	if *jsonPath == "" {
		fatal(fmt.Errorf("-json is required"))
	}
	list, err := readJSON(*jsonPath)
	if err != nil {
		fatal(err)
	}
	if err := sources.Validate(list); err != nil {
		fatal(err)
	}

	tmp, err := os.MkdirTemp("", "gccfactory-update-")
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(tmp)

	ctx := context.Background()
	drift := 0
	touched := 0
	for i := range list {
		s := &list[i]
		if *only != "" && s.Name != *only {
			continue
		}
		touched++
		sum, url, size, err := fetchAndHash(ctx, tmp, *s)
		if err != nil {
			fatal(fmt.Errorf("%s: %w", s.Name, err))
		}
		top, err := inspectTopDir(filepath.Join(tmp, s.Name), *s)
		if err != nil {
			fatal(fmt.Errorf("%s: %w", s.Name, err))
		}
		status := "unchanged"
		if sum != s.SHA256 {
			status = "SHA CHANGED (was " + short(s.SHA256) + ")"
			drift++
		}
		if top != s.TopDir {
			status += fmt.Sprintf("  TOPDIR CHANGED (was %q now %q)", s.TopDir, top)
			drift++
		}
		fmt.Printf("%-14s %s  %9d B  via %s  [%s]\n", s.Name, short(sum), size, hostOf(url), status)
		s.SHA256 = sum
		s.TopDir = top
	}
	if touched == 0 {
		fatal(fmt.Errorf("no source matched -only %q", *only))
	}

	if *check {
		if drift > 0 {
			fmt.Fprintf(os.Stderr, "\n%d field(s) drifted from sources.json\n", drift)
			os.Exit(1)
		}
		fmt.Println("\nall pinned sources match sources.json")
		return
	}
	if err := sources.Validate(list); err != nil {
		fatal(err)
	}
	if err := writeJSON(*jsonPath, list); err != nil {
		fatal(err)
	}
	fmt.Printf("\nwrote %s (%d entries)\n", *jsonPath, len(list))
}

func readJSON(path string) ([]sources.Source, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list []sources.Source
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return list, nil
}

// writeJSON renders the DB with a stable key order (struct field order), a
// stable entry order (sorted by name), 2-space indent and a trailing newline,
// so a re-run produces a zero diff when nothing upstream changed.
func writeJSON(path string, list []sources.Source) error {
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

var client = &http.Client{Timeout: 30 * time.Minute}

// fetchAndHash leaves the downloaded bytes at <dir>/<name> for inspectTopDir.
func fetchAndHash(ctx context.Context, dir string, s sources.Source) (sum, url string, size int64, err error) {
	var problems []string
	for _, u := range s.URLs {
		sum, size, err := download(ctx, u, filepath.Join(dir, s.Name))
		if err == nil {
			return sum, u, size, nil
		}
		problems = append(problems, fmt.Sprintf("  %s: %v", u, err))
	}
	return "", "", 0, fmt.Errorf("all %d mirrors failed:\n%s", len(s.URLs), strings.Join(problems, "\n"))
}

func download(ctx context.Context, url, dst string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", "gccfactory-update/1")
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("HTTP %s", resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// Errors out if the archive has zero or more than one top-level directory:
// strip-components=1 would silently mangle it otherwise.
func inspectTopDir(path string, s sources.Source) (string, error) {
	if s.Raw {
		return "", nil
	}
	out, err := exec.Command("tar", "-tf", path).Output()
	if err != nil {
		return "", fmt.Errorf("tar -tf %s: %w", path, err)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimPrefix(strings.TrimSpace(line), "./")
		if line == "" {
			continue
		}
		seen[strings.SplitN(line, "/", 2)[0]] = true
	}
	tops := make([]string, 0, len(seen))
	for k := range seen {
		tops = append(tops, k)
	}
	sort.Strings(tops)
	if len(tops) != 1 {
		return "", fmt.Errorf("expected exactly one top-level dir, got %v", tops)
	}
	return tops[0], nil
}

func short(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}

func hostOf(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	return strings.SplitN(u, "/", 2)[0]
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "update-sources: "+err.Error())
	os.Exit(1)
}
