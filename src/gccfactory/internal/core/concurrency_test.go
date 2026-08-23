package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Many goroutines wanting the same job must produce exactly one build.
func TestInProcessRace(t *testing.T) {
	e := testEnv(t)
	e.MaxWorkers = 8
	j := newJob("inproc")
	started := make(chan struct{})
	j.build = func(ctx context.Context, e *Env, r *Runner, work, stage string) error {
		time.Sleep(50 * time.Millisecond)
		return writePayload(stage, "inproc")
	}

	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-started
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			errs[i] = Run(ctx, e, []Job{j})
		}(i)
	}
	close(started)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if j.count() != 1 {
		t.Fatalf("Build ran %d times, want exactly 1", j.count())
	}
	assertValid(t, e, j)
	if got := readPayload(t, j.ArtifactDir(e)); got != "inproc" {
		t.Fatalf("payload = %q", got)
	}
}

// A shared dependency contended by many dependents must be built once.
func TestSharedDepBuiltOnce(t *testing.T) {
	e := testEnv(t)
	e.MaxWorkers = 8
	dep := newJob("shared-dep")
	dep.build = func(ctx context.Context, e *Env, r *Runner, work, stage string) error {
		time.Sleep(30 * time.Millisecond)
		return writePayload(stage, "shared")
	}
	var tops []Job
	for i := 0; i < 8; i++ {
		top := newJob(fmt.Sprintf("shared-top-%d", i))
		top.deps = []Job{dep}
		tops = append(tops, top)
	}
	mustRun(t, e, tops...)
	if dep.count() != 1 {
		t.Fatalf("shared dependency built %d times, want 1", dep.count())
	}
}

// Separate processes racing over one dist/ must also produce exactly one
// build, and no process may ever observe an inconsistent artifact.
func TestCrossProcessRace(t *testing.T) {
	dist := t.TempDir()
	e := envAt(t, dist)
	marker := filepath.Join(dist, "marker.txt")
	viol := filepath.Join(dist, "violations.txt")

	const n = 6
	var cmds []*exec.Cmd
	for i := 0; i < n; i++ {
		c := spawnChild(t, "race",
			envDist+"="+dist,
			envSlug+"=xproc",
			envMarker+"="+marker,
			envViol+"="+viol,
		)
		if err := c.Start(); err != nil {
			t.Fatalf("start child %d: %v", i, err)
		}
		cmds = append(cmds, c)
	}
	for i, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Errorf("child %d failed: %v", i, err)
		}
	}

	if b, err := os.ReadFile(viol); err == nil && len(b) > 0 {
		t.Fatalf("children observed inconsistent artifacts:\n%s", b)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("no child built the job: %v", err)
	}
	lines := nonEmptyLines(string(b))
	if len(lines) != 1 {
		t.Fatalf("job body ran %d times, want exactly 1:\n%s", len(lines), b)
	}

	j := newJob("xproc")
	assertValid(t, e, j)
	if got := readPayload(t, j.ArtifactDir(e)); got != "payload of xproc" {
		t.Fatalf("payload = %q", got)
	}
	if ents, _ := os.ReadDir(e.Path(DirStaging)); len(ents) != 0 {
		t.Fatalf("staging dirs leaked: %v", ents)
	}
}

// A process killed mid-build releases its lock to the kernel; the next run
// must rebuild cleanly and must not adopt the dead process's staging dir.
func TestCrashRecovery(t *testing.T) {
	dist := t.TempDir()
	e := envAt(t, dist)

	c := spawnChild(t, "crash", envDist+"="+dist, envSlug+"=crashy")
	if err := c.Run(); err == nil {
		t.Fatal("child was supposed to die by SIGKILL")
	}

	j := newJob("crashy")
	if ok, _ := IsValid(e, j); ok {
		t.Fatal("a crashed build must not leave a valid artifact")
	}
	if _, err := os.Stat(j.ArtifactDir(e)); !os.IsNotExist(err) {
		t.Fatalf("crashed build published something: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		done <- Run(ctx, e, []Job{j})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("recovery run failed: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("recovery run hung: the dead process's lock was never released")
	}

	assertValid(t, e, j)
	if got := readPayload(t, j.ArtifactDir(e)); got != "payload of crashy" {
		t.Fatalf("the crashed build's staging dir was published: payload = %q", got)
	}
	if j.count() != 1 {
		t.Fatalf("recovery should build exactly once, got %d", j.count())
	}

	// The dead process's scratch dirs are attributable and collectable.
	stale, _ := os.ReadDir(e.Path(DirStaging))
	if len(stale) == 0 {
		t.Fatal("expected the crashed process's staging dir to still be there")
	}
	for _, ent := range stale {
		pid, ok := pidFromScratchName(ent.Name())
		if !ok || pid == os.Getpid() {
			t.Fatalf("staging dir %q is not attributable to the dead process", ent.Name())
		}
	}
	e.GCStale(0)
	if ents, _ := os.ReadDir(e.Path(DirStaging)); len(ents) != 0 {
		t.Fatalf("GCStale did not collect the dead process's dirs: %v", ents)
	}
}

// A reader holding the shared lock must never see a missing or half-written
// artifact while a writer swaps it under the exclusive lock.
func TestReaderNeverSeesPartialArtifact(t *testing.T) {
	e := testEnv(t)
	dep := newJob("reader-dep")
	// The artifact's payload always equals its manifest key, so any mismatch a
	// reader sees is proof it observed a half-swapped directory.
	dep.build = func(ctx context.Context, e *Env, r *Runner, work, stage string) error {
		k, err := Key(dep)
		if err != nil {
			return err
		}
		return writePayload(stage, k)
	}
	mustRun(t, e, dep)
	art := dep.ArtifactDir(e)

	ctx, cancel := context.WithCancel(context.Background())
	var reads int64
	var wg sync.WaitGroup

	wg.Add(1)
	go func() { // rebuilder: republish the artifact over and over
		defer wg.Done()
		for i := 0; ctx.Err() == nil && i < 40; i++ {
			key := fmt.Sprintf("key-%d", i)
			stage := e.Path(DirStaging, scratchName(dep.Slug()))
			if err := os.MkdirAll(stage, 0o755); err != nil {
				t.Error(err)
				return
			}
			if err := writePayload(stage, key); err != nil {
				t.Error(err)
				return
			}
			if err := writeManifest(stage, &Manifest{Key: key, Slug: dep.Slug(), Name: "test"}); err != nil {
				t.Error(err)
				return
			}
			mu := slugMutex(dep.Slug())
			mu.Lock()
			ex, err := acquire(ctx, e, dep.Slug(), true)
			if err != nil {
				mu.Unlock()
				return
			}
			err = publish(e, stage, art)
			ex.release()
			mu.Unlock()
			if err != nil {
				t.Errorf("publish: %v", err)
				return
			}
		}
	}()

	wg.Add(1)
	go func() { // dependent: read the artifact under the shared lock
		defer wg.Done()
		for ctx.Err() == nil {
			sh, err := acquire(ctx, e, dep.Slug(), false)
			if err != nil {
				return
			}
			m, err := ReadManifest(art)
			if err != nil {
				sh.release()
				t.Errorf("reader saw no manifest: %v", err)
				return
			}
			b, err := os.ReadFile(filepath.Join(art, "payload"))
			sh.release()
			if err != nil {
				t.Errorf("reader saw a manifest but no payload: %v", err)
				return
			}
			if string(b) != m.Key {
				t.Errorf("reader saw a torn artifact: payload %q, manifest key %q", b, m.Key)
				return
			}
			atomic.AddInt64(&reads, 1)
		}
	}()

	time.Sleep(400 * time.Millisecond)
	cancel()
	wg.Wait()
	if atomic.LoadInt64(&reads) < 10 {
		t.Fatalf("reader only managed %d reads; the test did not exercise the race", reads)
	}
	trashWG.Wait()
}

// Cancelling the context must stop the run promptly and leave no artifact.
func TestCancelStopsRun(t *testing.T) {
	e := testEnv(t)
	j := newJob("cancelled")
	entered := make(chan struct{})
	j.build = func(ctx context.Context, e *Env, r *Runner, work, stage string) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, e, []Job{j}) }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled run should return an error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled run did not stop")
	}
	if _, err := os.Stat(j.ArtifactDir(e)); !os.IsNotExist(err) {
		t.Fatal("cancelled run published an artifact")
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
