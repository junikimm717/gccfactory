package cli

import (
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// /dev/null would not do here: it is a character device, not a pipe.
func withPipedStdin(t *testing.T) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; r.Close(); w.Close() })
}

func TestParseTriplesExpansions(t *testing.T) {
	all, err := parseTriples("host", "all")
	if err != nil || len(all) != len(triple.Known) {
		t.Fatalf("all: got %d triples, err %v; want %d", len(all), err, len(triple.Known))
	}
	// "proven" is role-dependent: we prove two hosts but every target.
	provenHosts, err := parseTriples("host", "proven")
	if err != nil || len(provenHosts) != len(triple.ProvenHosts) {
		t.Fatalf("--host proven: got %v, err %v; want %v", names(provenHosts), err, triple.ProvenHosts)
	}
	provenTargets, err := parseTriples("target", "proven")
	if err != nil || len(provenTargets) != len(triple.ProvenTargets) {
		t.Fatalf("--target proven: got %v, err %v; want %v", names(provenTargets), err, triple.ProvenTargets)
	}
	if len(provenTargets) <= len(provenHosts) {
		t.Fatalf("--target proven (%v) must be a strict superset of --host proven (%v)",
			names(provenTargets), names(provenHosts))
	}
	for _, want := range []string{"riscv32-linux-musl", "riscv64-linux-musl"} {
		if !contains(names(provenTargets), want) {
			t.Errorf("--target proven must include %s, got %v", want, names(provenTargets))
		}
	}
	list, err := parseTriples("target", "x86_64-linux-musl,aarch64-linux-musl,x86_64-linux-musl")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("duplicates should collapse, got %v", names(list))
	}
}

func TestBadTripleErrorNamesTheAlternatives(t *testing.T) {
	_, err := parseTriples("host", "x86_64-linux-gnu")
	if err == nil {
		t.Fatal("want an error for a non-musl triple")
	}
	msg := err.Error()
	for _, want := range []string{"--host", "x86_64-linux-gnu", "all, proven", "x86_64-linux-musl"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message is missing %q:\n%s", want, msg)
		}
	}
}

func TestBuildRejectsBadTripleBeforeTouchingDist(t *testing.T) {
	g := &Global{Dist: t.TempDir(), ColorWhen: "never"}
	err := runBuild(g, []string{"--host", "nope", "--target", "all"})
	if err == nil || !strings.Contains(err.Error(), "unknown triple") {
		t.Fatalf("got %v, want an unknown-triple error", err)
	}
	if ents, _ := os.ReadDir(g.Dist); len(ents) != 0 {
		t.Fatalf("a bad flag must not create anything under dist, found %v", ents)
	}
}

// The one behaviour that must never regress: with no selection and no
// terminal, we must fail loudly instead of waiting on a picker forever.
func TestBuildWithoutSelectionErrorsInsteadOfHanging(t *testing.T) {
	withPipedStdin(t)
	g := &Global{Dist: t.TempDir(), ColorWhen: "never"}

	errc := make(chan error, 1)
	go func() { errc <- runBuild(g, nil) }()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("want an error explaining how to select a matrix")
		}
		msg := err.Error()
		for _, want := range []string{"--host", "--target", "proven", "all", "not a terminal"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error message is missing %q:\n%s", want, msg)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runBuild blocked with no flags and no tty")
	}
}

func TestMissingArgumentsAreUsageErrors(t *testing.T) {
	dist := t.TempDir()
	for _, tc := range []struct {
		name string
		run  func(*Global, []string) error
	}{
		{"logs", runLogs},
		{"shell", runShell},
	} {
		err := tc.run(&Global{Dist: dist, ColorWhen: "never"}, nil)
		if !errors.Is(err, errUsage) {
			t.Errorf("%s with no slug: got %v, want a usage error", tc.name, err)
		}
	}
}

func TestUnknownFlagIsAUsageError(t *testing.T) {
	err := runStatus(&Global{Dist: t.TempDir(), ColorWhen: "never"}, []string{"--nonsense"})
	if !errors.Is(err, errUsage) {
		t.Fatalf("got %v, want a usage error", err)
	}
}

// logs and friends print straight to os.Stdout rather than an injectable
// writer, so the only way to assert on their output is to redirect it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = old
	w.Close()
	out, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func writeFakeJobLog(t *testing.T, dist, slug, body string) {
	t.Helper()
	mustWrite(t, filepath.Join(dist, "logs", "jobs", slug, "latest", "001-configure.log"), body)
}

// This is the exact shape from the bug report: `logs <slug> --failed`. Before
// the fix, flag.FlagSet.Parse stopped scanning at the positional slug, so
// --failed was silently dropped and the default (table + tail) output came
// back instead of the failed-step-only tail.
func TestLogsFlagAfterPositionalIsHonored(t *testing.T) {
	dist := t.TempDir()
	writeFakeJobLog(t, dist, "cross_test", "the failing output\n")
	g := &Global{Dist: dist, ColorWhen: "never"}

	out := captureStdout(t, func() {
		if err := runLogs(g, []string{"cross_test", "--failed"}); err != nil {
			t.Fatalf("runLogs with --failed after the slug: %v", err)
		}
	})
	if strings.Contains(out, "full step:") {
		t.Errorf("--failed after the slug was ignored (fell through to the default table view):\n%s", out)
	}
}

// Regression guard: the pre-fix form (flag before the positional) must keep
// working exactly as it did before.
func TestLogsFlagBeforePositionalStillWorks(t *testing.T) {
	dist := t.TempDir()
	writeFakeJobLog(t, dist, "cross_test", "the failing output\n")
	g := &Global{Dist: dist, ColorWhen: "never"}

	out := captureStdout(t, func() {
		if err := runLogs(g, []string{"--failed", "cross_test"}); err != nil {
			t.Fatalf("runLogs with --failed before the slug: %v", err)
		}
	})
	if strings.Contains(out, "full step:") {
		t.Errorf("--failed before the slug should suppress the default table view:\n%s", out)
	}
}

// The dangerous variant from the bug report: a GLOBAL flag (--dist) placed
// after the positional slug. Before the fix this was silently ignored -
// `gccf logs <slug> --dist /bogus` would read the REAL dist and exit 0. Now
// it must actually redirect to the bogus dist and fail to find the job there.
func TestLogsGlobalFlagAfterPositionalIsHonored(t *testing.T) {
	realDist := t.TempDir()
	writeFakeJobLog(t, realDist, "cross_test", "the failing output\n")
	bogusDist := t.TempDir() // deliberately has no logs for cross_test

	g := &Global{Dist: realDist, ColorWhen: "never"}
	err := runLogs(g, []string{"cross_test", "--dist", bogusDist})
	if err == nil {
		t.Fatal("--dist after the slug was ignored: runLogs succeeded against the real dist instead of the bogus one")
	}
	if g.Dist != bogusDist {
		t.Fatalf("g.Dist = %q, want it rebound to the post-positional --dist value %q", g.Dist, bogusDist)
	}
	if !strings.Contains(err.Error(), bogusDist) {
		t.Errorf("error should name the bogus dist it actually looked under: %v", err)
	}
}

// parse() is the shared mechanism behind every subcommand's flag handling;
// exercise it directly to pin down exactly what gets bound, independent of
// any one command's plumbing.
func TestParseBindsFlagValueAfterPositionalNotJustTheFlag(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dist := fs.String("dist", "", "")

	if err := parse(fs, []string{"cross_test", "--dist", "/some/path"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if fs.NArg() != 1 || fs.Arg(0) != "cross_test" {
		t.Fatalf("positional got mangled: NArg=%d Arg(0)=%q, want the slug preserved", fs.NArg(), fs.Arg(0))
	}
	if *dist != "/some/path" {
		t.Fatalf("--dist = %q, want the value bound exactly, not the flag name or a bare bool", *dist)
	}
}

func TestBuildRejectsBarePositionalArgs(t *testing.T) {
	err := runBuild(&Global{Dist: t.TempDir(), ColorWhen: "never"}, []string{"x86_64-linux-musl"})
	if !errors.Is(err, errUsage) {
		t.Fatalf("got %v, want a usage error suggesting --target", err)
	}
	if !strings.Contains(err.Error(), "--target") {
		t.Errorf("the error should suggest the flag: %v", err)
	}
}

func TestDispatch(t *testing.T) {
	withPipedStdin(t)
	if code := Main([]string{"definitely-not-a-command"}); code != 2 {
		t.Errorf("unknown command: exit %d, want 2", code)
	}
	if code := Main(nil); code != 0 {
		t.Errorf("no args should print help and exit 0, got %d", code)
	}
	if code := Main([]string{"help", "build"}); code != 0 {
		t.Errorf("help build: exit %d, want 0", code)
	}
	if code := Main([]string{"help", "nope"}); code != 2 {
		t.Errorf("help for an unknown command: exit %d, want 2", code)
	}
}

// Help output is the documentation, so hold it to a standard.
func TestEveryCommandDocumentsItself(t *testing.T) {
	for _, c := range commands() {
		if c.Short == "" || c.Long == "" {
			t.Errorf("%s: needs both a Short and a Long description", c.Name)
		}
		if !strings.HasPrefix(c.Synopsis, "gccfactory "+c.Name) {
			t.Errorf("%s: synopsis %q should start with the command", c.Name, c.Synopsis)
		}
	}
}

func TestSlugFromLogPath(t *testing.T) {
	cases := map[string]string{
		"/w/dist/logs/jobs/cross_x86_64-linux-musl/2026-01-01T00:00:00Z/007-gcc-configure.log": "cross_x86_64-linux-musl",
		"/w/dist/logs/jobs/canadian_a__b/003-make.log":                                         "canadian_a__b",
		"/nothing/here.log": "",
	}
	for in, want := range cases {
		if got := slugFromLogPath(in); got != want {
			t.Errorf("slugFromLogPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// core may write logs flat or under a per-attempt directory; both must work.
func TestLatestAttemptHandlesBothLayouts(t *testing.T) {
	flat := t.TempDir()
	mustWrite(t, filepath.Join(flat, "001-configure.log"), "x")
	if got, err := latestAttempt(flat); err != nil || got != flat {
		t.Errorf("flat layout: got %q, %v", got, err)
	}

	nested := t.TempDir()
	for _, a := range []string{"2026-01-01T00-00-00Z", "2026-02-01T00-00-00Z"} {
		mustWrite(t, filepath.Join(nested, a, "001-configure.log"), "x")
	}
	got, err := latestAttempt(nested)
	if err != nil || filepath.Base(got) != "2026-02-01T00-00-00Z" {
		t.Errorf("nested layout: got %q, %v; want the newest attempt", got, err)
	}

	if err := os.Symlink(filepath.Join(nested, "2026-01-01T00-00-00Z"), filepath.Join(nested, "latest")); err != nil {
		t.Skip(err)
	}
	if got, err := latestAttempt(nested); err != nil || filepath.Base(got) != "latest" {
		t.Errorf("the latest symlink should win: got %q, %v", got, err)
	}
}

func TestParseRecordedEnvReadsTheLogHeader(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "003-gcc-configure.log")
	mustWrite(t, p, strings.Join([]string{
		"# cwd: /w/dist/work/cross_x86_64.123.abc/gcc",
		"# env: CC=/w/dist/toolchains/cross/x86_64-linux-musl/bin/x86_64-linux-musl-gcc",
		"# env: CFLAGS=-O2 -g",
		"# cmd: ../configure --prefix=/opt",
		"checking for gcc... yes",
	}, "\n"))

	cwd, env := parseRecordedEnv(p)
	if cwd != "/w/dist/work/cross_x86_64.123.abc/gcc" {
		t.Errorf("cwd = %q", cwd)
	}
	if env["CFLAGS"] != "-O2 -g" {
		t.Errorf("CFLAGS = %q, want %q", env["CFLAGS"], "-O2 -g")
	}
	if len(env) != 2 {
		t.Errorf("env should stop at the # cmd: line, got %v", env)
	}
}

func TestOwnerPID(t *testing.T) {
	if pid, ok := ownerPID("/w/dist/work/cross_x86_64-linux-musl.4231.f0e1"); !ok || pid != 4231 {
		t.Errorf("got %d, %v", pid, ok)
	}
	if _, ok := ownerPID("/w/dist/work/notscratch"); ok {
		t.Error("a non-scratch name should not yield a pid")
	}
}

func TestQemuPathTemplateUsesQemuName(t *testing.T) {
	ppc := triple.MustParse("powerpc64le-linux-musl")
	if got := qemuPath("/opt/q/qemu-%s", ppc); got != "/opt/q/qemu-ppc64le" {
		t.Errorf("template form: %q", got)
	}
}

func TestQemuPathDirFormIsAlwaysStatic(t *testing.T) {
	dir := t.TempDir()
	nonStatic := filepath.Join(dir, "qemu-ppc64le")
	if err := os.WriteFile(nonStatic, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := qemuPath(dir, triple.MustParse("powerpc64le-linux-musl"))
	if want := filepath.Join(dir, "qemu-ppc64le-static"); got != want {
		t.Errorf("dir form: got %q, want %q (the non-static binary must be ignored)", got, want)
	}
}

func TestMissingStaticQemuNamesEveryArch(t *testing.T) {
	dir := t.TempDir()
	ts := make([]triple.Triple, 0, len(triple.Known))
	for _, raw := range triple.Known {
		ts = append(ts, triple.MustParse(raw))
	}
	missing := missingStaticQemu(dir, ts)
	if len(missing) != 10 {
		t.Fatalf("got %d missing, want 10 unique qemu names: %v", len(missing), missing)
	}
	joined := strings.Join(missing, "\n")
	for _, n := range []string{"qemu-ppc64-static", "qemu-ppc64le-static", "qemu-x86_64-static", "qemu-arm-static"} {
		if !strings.Contains(joined, n) {
			t.Errorf("missing list must contain %s:\n%s", n, joined)
		}
	}
	if err := staticQemuErr(missing); err == nil || !strings.Contains(err.Error(), "qemu-user-static") {
		t.Fatalf("error must name the package:\n%v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "qemu-ppc64-static"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	missing = missingStaticQemu(dir, []triple.Triple{triple.MustParse("powerpc64-linux-musl")})
	if len(missing) != 0 {
		t.Fatalf("present -static binary must satisfy the check, got %v", missing)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestShimQemuStaticNamesMatchTriples(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(root, "src/gccf"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`qemu_static_names=\(([^)]+)\)`).FindSubmatch(body)
	if m == nil {
		t.Fatal("src/gccf must declare qemu_static_names=(...)")
	}
	var want []string
	seen := map[string]bool{}
	for _, raw := range triple.Known {
		n := triple.MustParse(raw).QemuName()
		if !seen[n] {
			seen[n] = true
			want = append(want, n)
		}
	}
	got := strings.Fields(string(m[1]))
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("src/gccf qemu_static_names drifted from triple.QemuName()\n  shim: %v\n  go:   %v", got, want)
	}
}

func TestShimStopsWithoutQemuStatic(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	dist := t.TempDir()
	cmd := exec.Command(filepath.Join(root, "src/gccf"), "--qemu-dir", empty,
		"build", "--host", "x86_64-linux-musl", "--target", "x86_64-linux-musl")
	cmd.Env = append(os.Environ(), "GCCF_DIST="+dist, "GCCF_QEMU_DIR="+empty)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a hard stop, got success:\n%s", out)
	}
	s := string(out)
	for _, frag := range []string{"qemu-ppc64-static", "qemu-user-static", "binfmt_misc is not a substitute"} {
		if !strings.Contains(s, frag) {
			t.Errorf("missing %q in:\n%s", frag, s)
		}
	}
}

func TestShimDryRunWorksWithoutQemuStatic(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	dist := t.TempDir()
	cmd := exec.Command(filepath.Join(root, "src/gccf"), "--qemu-dir", empty,
		"build", "--dry-run", "--host", "x86_64-linux-musl", "--target", "x86_64-linux-musl")
	cmd.Env = append(os.Environ(), "GCCF_DIST="+dist, "GCCF_QEMU_DIR="+empty)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run must not require qemu-user-static: %v\n%s", err, out)
	}
}

func TestShimHelpWorksWithoutQemuStatic(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	dist := t.TempDir()
	cmd := exec.Command(filepath.Join(root, "src/gccf"), "--qemu-dir", empty, "help")
	cmd.Env = append(os.Environ(), "GCCF_DIST="+dist, "GCCF_QEMU_DIR="+empty)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("help must still run without qemu-user-static: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "gccfactory") {
		t.Fatalf("help output:\n%s", out)
	}
}
