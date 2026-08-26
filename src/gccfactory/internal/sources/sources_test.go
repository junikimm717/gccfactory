package sources

import (
	"strings"
	"testing"
)

// TestEmbeddedDB is the regression guard on sources.json itself: a bad hand
// edit (truncated sha, dropped mirror, duplicated name) must fail here rather
// than three hours into a gcc build.
func TestEmbeddedDB(t *testing.T) {
	all := All()
	if len(all) == 0 {
		t.Fatal("sources.json is empty")
	}
	if err := Validate(all); err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		if !strings.HasPrefix(s.File, s.Name) && s.File != "config.sub" {
			t.Errorf("%s: file %q does not look like it belongs to this source", s.Name, s.File)
		}
		if !s.Raw && s.TopDir != s.Name+"-"+s.Version {
			t.Errorf("%s: topdir %q != %s-%s", s.Name, s.TopDir, s.Name, s.Version)
		}
		if s.StripComponents() != 1 {
			t.Errorf("%s: strip is %d, every archive we ship has one top-level dir", s.Name, s.StripComponents())
		}
	}
}

// The recipes hard-code these names; losing one silently would break the build.
func TestRequiredSourcesPresent(t *testing.T) {
	want := []string{"binutils", "gcc", "musl", "gmp", "mpfr", "mpc", "isl", "linux-headers", "make", "config.sub"}
	for _, n := range want {
		s, err := Get(n)
		if err != nil {
			t.Errorf("%v", err)
			continue
		}
		if s.Name != n {
			t.Errorf("Get(%q) returned %q", n, s.Name)
		}
	}
}

func TestGetUnknown(t *testing.T) {
	if _, err := Get("nope"); err == nil {
		t.Fatal("expected an error for an unknown source")
	} else if !strings.Contains(err.Error(), "known:") {
		t.Errorf("error should list the known names, got: %v", err)
	}
}

func TestValidateRejectsBadEntries(t *testing.T) {
	good := Source{Name: "x", Version: "1", File: "x-1.tar.gz",
		URLs: []string{"https://e/x"}, SHA256: strings.Repeat("a", 64), TopDir: "x-1"}
	if err := Validate([]Source{good}); err != nil {
		t.Fatalf("good entry rejected: %v", err)
	}
	cases := map[string]func(*Source){
		"short sha":  func(s *Source) { s.SHA256 = "abc" },
		"upper sha":  func(s *Source) { s.SHA256 = strings.Repeat("A", 64) },
		"no urls":    func(s *Source) { s.URLs = nil },
		"empty url":  func(s *Source) { s.URLs = []string{""} },
		"no name":    func(s *Source) { s.Name = "" },
		"no file":    func(s *Source) { s.File = "" },
		"no topdir":  func(s *Source) { s.TopDir = "" },
		"raw topdir": func(s *Source) { s.Raw = true },
	}
	for name, mutate := range cases {
		bad := good
		mutate(&bad)
		if err := Validate([]Source{bad}); err == nil {
			t.Errorf("%s: Validate accepted an invalid entry", name)
		}
	}
	if err := Validate([]Source{good, good}); err == nil {
		t.Error("Validate accepted duplicate names")
	}
}

func TestPatchesForPatchedSources(t *testing.T) {
	for _, tc := range []struct {
		name string
		want int
	}{
		{"gcc", 11}, {"binutils", 5}, {"musl", 4},
	} {
		s := MustGet(tc.name)
		ps, err := Patches(s)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(ps) != tc.want {
			names := make([]string, len(ps))
			for i, p := range ps {
				names[i] = p.Name
			}
			t.Errorf("%s (%s): got %d patches %v, want %d", tc.name, s.Slug(), len(ps), names, tc.want)
		}
		for i, p := range ps {
			if i > 0 && ps[i-1].Name >= p.Name {
				t.Errorf("%s: patches not sorted: %q before %q", tc.name, ps[i-1].Name, p.Name)
			}
			if len(p.Data) == 0 {
				t.Errorf("%s: patch %s is empty", tc.name, p.Name)
			}
			if !isDiff(p.Name) {
				t.Errorf("%s: %s is not a .diff/.patch", tc.name, p.Name)
			}
			if !looksLikeUnifiedDiff(p.Data) {
				t.Errorf("%s: %s has no unified-diff hunk", tc.name, p.Name)
			}
		}
	}
}

// Sources with no patch set must return nil, not an error.
func TestPatchesForUnpatchedSource(t *testing.T) {
	ps, err := Patches(MustGet("gmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 0 {
		t.Errorf("gmp unexpectedly has %d patches", len(ps))
	}
}

// Every embedded patch dir must correspond to a pinned source at that exact
// version, or we would ship patches that are never applied.
func TestPatchSetsMatchPinnedVersions(t *testing.T) {
	slugs := map[string]bool{}
	for _, s := range All() {
		slugs[s.Slug()] = true
	}
	for _, d := range PatchSets() {
		if !slugs[d] {
			t.Errorf("patch dir %q has no matching pinned source (stale after a version bump?)", d)
		}
	}
}

func looksLikeUnifiedDiff(b []byte) bool {
	var sawMinus, sawPlus, sawHunk bool
	for _, ln := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(ln, "--- "):
			sawMinus = true
		case strings.HasPrefix(ln, "+++ "):
			sawPlus = true
		case strings.HasPrefix(ln, "@@ "):
			sawHunk = true
		}
	}
	return sawMinus && sawPlus && sawHunk
}
