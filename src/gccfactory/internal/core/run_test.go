package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIdempotentRebuild(t *testing.T) {
	e := testEnv(t)
	dep := newJob("idem-dep")
	top := newJob("idem-top")
	top.deps = []Job{dep}

	mustRun(t, e, top)
	if dep.count() != 1 || top.count() != 1 {
		t.Fatalf("first run should build both once, got dep=%d top=%d", dep.count(), top.count())
	}
	k1, _ := Key(top)

	mustRun(t, e, top)
	if dep.count() != 1 || top.count() != 1 {
		t.Fatalf("second run must not rebuild, got dep=%d top=%d", dep.count(), top.count())
	}
	k2, _ := Key(top)
	if k1 != k2 {
		t.Fatalf("key changed between runs: %s -> %s", k1, k2)
	}

	m, err := ReadManifest(top.ArtifactDir(e))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if m.Key != k1 || m.Slug != "idem-top" {
		t.Fatalf("manifest does not describe the job: %+v", m)
	}
	if dk, err := Key(dep); err != nil || m.Deps["idem-dep"] != dk {
		t.Fatalf("manifest deps wrong: %+v", m.Deps)
	}
	assertValid(t, e, top)

	// A changed input invalidates the artifact and forces exactly one rebuild.
	clearKeyCache()
	top.inputs = map[string]string{"v": "2"}
	mustRun(t, e, top)
	if top.count() != 2 || dep.count() != 1 {
		t.Fatalf("input change should rebuild only top, got dep=%d top=%d", dep.count(), top.count())
	}
}

// A dependency rebuild must cascade to everything downstream.
func TestDepChangeCascades(t *testing.T) {
	e := testEnv(t)
	dep := newJob("casc-dep")
	top := newJob("casc-top")
	top.deps = []Job{dep}
	mustRun(t, e, top)

	clearKeyCache()
	dep.inputs = map[string]string{"v": "2"}
	mustRun(t, e, top)
	if dep.count() != 2 || top.count() != 2 {
		t.Fatalf("dep change must rebuild both, got dep=%d top=%d", dep.count(), top.count())
	}
}

// Deps must be published before a dependent's Build starts.
func TestDepsReadyBeforeBuild(t *testing.T) {
	e := testEnv(t)
	e.MaxWorkers = 8
	dep := newJob("order-dep")
	top := newJob("order-top")
	top.deps = []Job{dep}
	top.build = func(ctx context.Context, e *Env, r *Runner, work, stage string) error {
		ok, err := IsValid(e, dep)
		if err != nil || !ok {
			t.Errorf("dependency not valid when dependent started (err=%v)", err)
		}
		return writePayload(stage, "top")
	}
	mustRun(t, e, top)
}

func TestPlanReportsState(t *testing.T) {
	e := testEnv(t)
	dep := newJob("plan-dep")
	top := newJob("plan-top")
	top.deps = []Job{dep}

	p, err := Plan(e, []Job{top})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(p) != 2 || p[0].Job.Slug() != "plan-dep" {
		t.Fatalf("plan should be dependency-first, got %v", p)
	}
	for _, n := range p {
		if n.Valid || n.Building {
			t.Fatalf("nothing is built yet: %+v", n)
		}
	}
	mustRun(t, e, top)
	p, _ = Plan(e, []Job{top})
	for _, n := range p {
		if !n.Valid {
			t.Fatalf("%s should be valid after a build", n.Job.Slug())
		}
	}
}

// A failing job must surface the underlying *CmdError, name the job, leave the
// artifact absent, and leave behind a replayable script.
func TestFailureSurfacesCmdError(t *testing.T) {
	e := testEnv(t)
	j := newJob("failing")
	j.build = func(ctx context.Context, e *Env, r *Runner, work, stage string) error {
		r.Step("boom")
		return r.Run(ctx, Cmd{
			Dir:    work,
			Args:   []string{"/bin/sh", "-c", "echo about to fail; echo detail >&2; exit 3"},
			EnvAdd: map[string]string{"MARKER": "value with spaces"},
			Name:   "explode",
		})
	}

	err := Run(context.Background(), e, []Job{j})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "failing") {
		t.Fatalf("error must name the job, got: %v", err)
	}
	var ce *CmdError
	if !errors.As(err, &ce) {
		t.Fatalf("error must unwrap to *CmdError, got %T: %v", err, err)
	}
	if ce.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", ce.ExitCode)
	}
	if !strings.Contains(ce.Tail, "about to fail") || !strings.Contains(ce.Tail, "detail") {
		t.Fatalf("tail should hold merged stdout+stderr, got %q", ce.Tail)
	}
	if !strings.HasSuffix(ce.Error(), "full log: "+ce.LogPath) {
		t.Fatalf("CmdError should end with the log path, got:\n%s", ce.Error())
	}

	if _, err := os.Stat(j.ArtifactDir(e)); !os.IsNotExist(err) {
		t.Fatalf("failed job must not publish an artifact (err=%v)", err)
	}

	logged, err := os.ReadFile(ce.LogPath)
	if err != nil {
		t.Fatalf("read step log: %v", err)
	}
	for _, want := range []string{"# cwd: ", "# env: MARKER=value with spaces", "# cmd: /bin/sh -c ", "# started: ", "# exit: 3"} {
		if !strings.Contains(string(logged), want) {
			t.Fatalf("step log missing %q:\n%s", want, logged)
		}
	}

	attempt := filepath.Dir(ce.LogPath)
	script, err := os.ReadFile(filepath.Join(attempt, "commands.sh"))
	if err != nil {
		t.Fatalf("read commands.sh: %v", err)
	}
	if !strings.Contains(string(script), "cd ") || !strings.Contains(string(script), "'MARKER=value with spaces'") {
		t.Fatalf("commands.sh must be replayable:\n%s", script)
	}
	if !strings.Contains(string(script), "step: boom") {
		t.Fatalf("commands.sh should record steps:\n%s", script)
	}

	link, err := os.Readlink(filepath.Join(e.JobLogDir("failing"), "latest"))
	if err != nil {
		t.Fatalf("latest symlink: %v", err)
	}
	if link != filepath.Base(attempt) {
		t.Fatalf("latest points at %q, want %q", link, filepath.Base(attempt))
	}
}

// Rebuilding must not clobber the previous attempt's logs, however fast the
// attempts follow each other.
func TestAttemptLogsArePreserved(t *testing.T) {
	e := testEnv(t)
	j := newJob("attempts")
	j.build = func(ctx context.Context, e *Env, r *Runner, work, stage string) error {
		if err := r.Run(ctx, Cmd{Dir: work, Args: []string{"/bin/sh", "-c", "echo attempt"}, Name: "probe"}); err != nil {
			return err
		}
		return writePayload(stage, "x")
	}
	mustRun(t, e, j)
	clearKeyCache()
	j.inputs = map[string]string{"v": "2"}
	mustRun(t, e, j)

	ents, err := os.ReadDir(e.JobLogDir("attempts"))
	if err != nil {
		t.Fatal(err)
	}
	var attempts []string
	for _, ent := range ents {
		if ent.Name() != "latest" {
			attempts = append(attempts, ent.Name())
		}
	}
	if len(attempts) != 2 {
		t.Fatalf("expected 2 attempt dirs, got %v", attempts)
	}
	for _, a := range attempts {
		if _, err := os.Stat(filepath.Join(e.JobLogDir("attempts"), a, "001-probe.log")); err != nil {
			t.Fatalf("attempt %s lost its logs: %v", a, err)
		}
	}
}

// One failure must stop the run without corrupting the jobs that did succeed.
func TestFailureDoesNotCorruptSiblings(t *testing.T) {
	e := testEnv(t)
	e.MaxWorkers = 2
	good := newJob("sib-good")
	bad := newJob("sib-bad")
	bad.build = func(ctx context.Context, e *Env, r *Runner, work, stage string) error {
		return errors.New("nope")
	}
	top := newJob("sib-top")
	top.deps = []Job{good, bad}

	if err := Run(context.Background(), e, []Job{top}); err == nil {
		t.Fatal("expected failure")
	}
	if top.count() != 0 {
		t.Fatal("dependent of a failed job must not run")
	}
	if _, err := os.Stat(bad.ArtifactDir(e)); !os.IsNotExist(err) {
		t.Fatal("failed job left an artifact behind")
	}

	// The surviving artifact must still be usable, and a rerun must finish it.
	bad.build = nil
	mustRun(t, e, top)
	if good.count() != 1 {
		t.Fatalf("good job rebuilt unnecessarily: %d", good.count())
	}
	assertValid(t, e, top)
}

// Every published directory must be complete: staging dirs are never visible
// as artifacts, and the manifest is the last thing written.
func TestPublishIsAtomic(t *testing.T) {
	e := testEnv(t)
	j := newJob("atomic")
	j.build = func(ctx context.Context, e *Env, r *Runner, work, stage string) error {
		if _, err := os.Stat(filepath.Join(stage, ManifestName)); !os.IsNotExist(err) {
			t.Error("staging dir must start without a manifest")
		}
		if !strings.Contains(stage, DirStaging) {
			t.Errorf("build should write to .staging, got %s", stage)
		}
		return writePayload(stage, "atomic")
	}
	mustRun(t, e, j)

	if got := readPayload(t, j.ArtifactDir(e)); got != "atomic" {
		t.Fatalf("payload = %q", got)
	}
	ents, err := os.ReadDir(e.Path(DirStaging))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("staging should be empty after a successful publish, got %v", ents)
	}
	if ents, _ := os.ReadDir(e.Path(DirWork)); len(ents) != 0 {
		t.Fatalf("work dirs should be removed on success, got %v", ents)
	}
}

// Replacing an artifact must never merge old and new contents.
func TestRepublishReplacesWholeDirectory(t *testing.T) {
	e := testEnv(t)
	j := newJob("replace")
	j.build = func(ctx context.Context, e *Env, r *Runner, work, stage string) error {
		return os.WriteFile(filepath.Join(stage, "old-only"), []byte("v1"), 0o644)
	}
	mustRun(t, e, j)

	clearKeyCache()
	j.inputs = map[string]string{"v": "2"}
	j.build = func(ctx context.Context, e *Env, r *Runner, work, stage string) error {
		return os.WriteFile(filepath.Join(stage, "new-only"), []byte("v2"), 0o644)
	}
	mustRun(t, e, j)

	if _, err := os.Stat(filepath.Join(j.ArtifactDir(e), "old-only")); !os.IsNotExist(err) {
		t.Fatal("stale file survived a republish")
	}
	if _, err := os.Stat(filepath.Join(j.ArtifactDir(e), "new-only")); err != nil {
		t.Fatalf("new file missing after republish: %v", err)
	}
}

// A corrupt or truncated manifest must read as "not built", not as an error
// the user has to clean up by hand.
func TestCorruptManifestRebuilds(t *testing.T) {
	e := testEnv(t)
	j := newJob("corrupt")
	mustRun(t, e, j)

	if err := os.WriteFile(filepath.Join(j.ArtifactDir(e), ManifestName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, _ := IsValid(e, j); ok {
		t.Fatal("a corrupt manifest must not count as valid")
	}
	mustRun(t, e, j)
	if j.count() != 2 {
		t.Fatalf("expected a rebuild, build ran %d times", j.count())
	}
	assertValid(t, e, j)
}
