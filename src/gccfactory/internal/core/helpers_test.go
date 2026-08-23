package core

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/logging"
)

// testJob is a job whose Build is a closure. No compiler is ever involved.
type testJob struct {
	name   string
	slug   string
	inputs map[string]string
	deps   []Job
	calls  *int32
	build  func(ctx context.Context, e *Env, r *Runner, work, stage string) error
}

func newJob(slug string) *testJob {
	return &testJob{name: "test", slug: slug, inputs: map[string]string{"v": "1"}, calls: new(int32)}
}

func (j *testJob) Name() string                 { return j.name }
func (j *testJob) Slug() string                 { return j.slug }
func (j *testJob) Deps() []Job                  { return j.deps }
func (j *testJob) KeyInputs() map[string]string { return j.inputs }
func (j *testJob) ArtifactDir(e *Env) string    { return e.Path("art", j.slug) }

func (j *testJob) Build(ctx context.Context, e *Env, r *Runner, work, stage string) error {
	atomic.AddInt32(j.calls, 1)
	if j.build != nil {
		return j.build(ctx, e, r, work, stage)
	}
	return writePayload(stage, "payload of "+j.slug)
}

func (j *testJob) count() int { return int(atomic.LoadInt32(j.calls)) }

func writePayload(stage, content string) error {
	return os.WriteFile(filepath.Join(stage, "payload"), []byte(content), 0o644)
}

func readPayload(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "payload"))
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	return string(b)
}

func testEnv(t *testing.T) *Env {
	t.Helper()
	return envAt(t, t.TempDir())
}

func envAt(t *testing.T, dist string) *Env {
	t.Helper()
	clearKeyCache()
	e := newEnv(dist)
	if err := e.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	t.Cleanup(func() { e.Log.Close() })
	return e
}

// newEnv is shared with the subprocess children, which have no *testing.T.
func newEnv(dist string) *Env {
	lvl := logging.LevelDebug
	var out io.Writer = io.Discard
	if os.Getenv("GCCF_TEST_VERBOSE") != "" {
		out = os.Stderr
	}
	log, err := logging.New(logging.Options{
		RunsRoot: filepath.Join(dist, DirLogs, "runs"),
		Stderr:   out,
		Level:    lvl,
		Color:    new(bool),
	})
	if err != nil {
		log = logging.Discard()
	}
	return &Env{Dist: dist, RepoRoot: dist, Jobs: 1, MaxWorkers: 4, Log: log}
}

func clearKeyCache() {
	keyCache.Range(func(k, _ any) bool { keyCache.Delete(k); return true })
}

// freshKey computes a key ignoring the process-wide memo, so a test may reuse
// a slug with different inputs.
func freshKey(t *testing.T, j Job) string {
	t.Helper()
	k, err := computeKey(j, &keyWalk{memo: map[string]string{}, onStack: map[string]bool{}})
	if err != nil {
		t.Fatalf("computeKey: %v", err)
	}
	return k
}

func mustRun(t *testing.T, e *Env, jobs ...Job) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := Run(ctx, e, jobs); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func assertValid(t *testing.T, e *Env, j Job) {
	t.Helper()
	ok, err := IsValid(e, j)
	if err != nil {
		t.Fatalf("IsValid: %v", err)
	}
	if !ok {
		t.Fatalf("artifact for %s is not valid", j.Slug())
	}
}

// Cross-process tests re-exec this very test binary. TestMain dispatches on
// GCCF_TEST_CHILD before the test framework ever starts.

const (
	envChild  = "GCCF_TEST_CHILD"
	envDist   = "GCCF_TEST_DIST"
	envSlug   = "GCCF_TEST_SLUG"
	envMarker = "GCCF_TEST_MARKER"
	envViol   = "GCCF_TEST_VIOLATIONS"
)

func TestMain(m *testing.M) {
	switch os.Getenv(envChild) {
	case "":
		os.Exit(m.Run())
	case "race":
		os.Exit(childRace())
	case "crash":
		os.Exit(childCrash())
	default:
		fmt.Fprintln(os.Stderr, "unknown child mode")
		os.Exit(2)
	}
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// childRace builds one slow job and, in parallel, watches the artifact
// directory for any moment where it is visible but inconsistent.
func childRace() int {
	dist, slug := os.Getenv(envDist), os.Getenv(envSlug)
	marker, viol := os.Getenv(envMarker), os.Getenv(envViol)
	e := newEnv(dist)
	defer e.Log.Close()

	j := newJob(slug)
	j.build = func(ctx context.Context, e *Env, r *Runner, work, stage string) error {
		time.Sleep(300 * time.Millisecond)
		if err := appendLine(marker, fmt.Sprintf("built by pid %d", os.Getpid())); err != nil {
			return err
		}
		return writePayload(stage, "payload of "+slug)
	}
	want, err := Key(j)
	if err != nil {
		appendLine(viol, "key error: "+err.Error())
		return 1
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		art := j.ArtifactDir(e)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if m, err := ReadManifest(art); err == nil {
				if m.Key != want {
					appendLine(viol, fmt.Sprintf("pid %d saw key %s want %s", os.Getpid(), m.Key, want))
				}
				if _, err := os.Stat(filepath.Join(art, "payload")); err != nil {
					appendLine(viol, fmt.Sprintf("pid %d saw manifest without payload: %v", os.Getpid(), err))
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	runErr := Run(ctx, e, []Job{j})
	close(stop)
	<-done
	if runErr != nil {
		appendLine(viol, fmt.Sprintf("pid %d run error: %v", os.Getpid(), runErr))
		return 1
	}
	return 0
}

// childCrash acquires the build lease, gets halfway through Build, and dies
// hard without releasing anything.
func childCrash() int {
	dist, slug := os.Getenv(envDist), os.Getenv(envSlug)
	e := newEnv(dist)
	j := newJob(slug)
	j.build = func(ctx context.Context, e *Env, r *Runner, work, stage string) error {
		writePayload(stage, "poisoned by crashed build")
		os.WriteFile(filepath.Join(work, "half-built"), []byte("x"), 0o644)
		p, _ := os.FindProcess(os.Getpid())
		p.Signal(os.Kill)
		time.Sleep(10 * time.Second)
		return nil
	}
	Run(context.Background(), e, []Job{j})
	return 0
}

func spawnChild(t *testing.T, mode string, extra ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), envChild+"="+mode)
	cmd.Env = append(cmd.Env, extra...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd
}
