package ensure

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestReportStringAndOK(t *testing.T) {
	r := NewReport("cross toolchain aarch64-linux-musl")
	r.Pass("tools", "24/24 tools present")
	r.Skip("probe:dlopen/-O0/static", "dlopen needs a dynamic binary")
	r.Add(Check{
		Name:    "probe:pthread/-O2/dynamic",
		Err:     errors.New("stdout mismatch"),
		Detail:  "line 1: want \"sum=100000\\n\"\n           got \"\"",
		LogPath: "/dist/logs/jobs/x/007-probe.log",
	})

	if r.OK() {
		t.Fatal("OK() must be false when a check failed")
	}
	pass, fail, skip := r.Counts()
	if pass != 1 || fail != 1 || skip != 1 {
		t.Fatalf("counts = %d/%d/%d", pass, fail, skip)
	}

	got := r.String()
	want := strings.Join([]string{
		"cross toolchain aarch64-linux-musl: 1 passed, 1 failed, 1 skipped",
		"  ✓ tools                      24/24 tools present",
		"  - probe:dlopen/-O0/static    dlopen needs a dynamic binary",
		"  ✗ probe:pthread/-O2/dynamic",
		"      line 1: want \"sum=100000\\n\"",
		"                 got \"\"",
		"      stdout mismatch",
		"      log: /dist/logs/jobs/x/007-probe.log",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("String():\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	err := r.Err()
	if err == nil {
		t.Fatal("Err() must be non-nil")
	}
	for _, frag := range []string{"1 of 2 checks failed", "probe:pthread/-O2/dynamic", "007-probe.log"} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("Err() missing %q:\n%s", frag, err)
		}
	}

	ok := NewReport("empty")
	if !ok.OK() || ok.Err() != nil {
		t.Fatal("an empty report is a passing report")
	}
	ok.Pass("a", "")
	if !ok.OK() || ok.Err() != nil {
		t.Fatal("all-pass report must be OK")
	}
}

func TestReportLogPathFromError(t *testing.T) {
	r := NewReport("s")
	r.Fail("compile", errors.New("exit status 1\nfull log: /dist/logs/jobs/j/003-gcc.log"), "boom")
	if got := r.Checks[0].LogPath; got != "/dist/logs/jobs/j/003-gcc.log" {
		t.Fatalf("LogPath = %q", got)
	}
}

func TestReportJSON(t *testing.T) {
	r := NewReport("s")
	r.Pass("a", "fine")
	r.Fail("b", errors.New("nope"), "detail")
	b, err := r.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Subject string `json:"subject"`
		OK      bool   `json:"ok"`
		Failed  int    `json:"failed"`
		Checks  []struct {
			Name  string `json:"name"`
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.OK || out.Failed != 1 || len(out.Checks) != 2 || out.Checks[1].Error != "nope" {
		t.Fatalf("bad json: %s", b)
	}
}

func TestAbsorb(t *testing.T) {
	sub := NewReport("sub")
	sub.Pass("x", "")
	r := NewReport("top")
	r.Absorb("pre-", sub)
	if len(r.Checks) != 1 || r.Checks[0].Name != "pre-x" {
		t.Fatalf("%+v", r.Checks)
	}
	r.Absorb("", nil)
}
