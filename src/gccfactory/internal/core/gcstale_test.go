package core

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// stalePid is a pid that cannot be alive, so pidAlive() is false without having
// to spawn and kill anything.
const stalePid = 0x7FFFFFF0

func writeStaleDir(t *testing.T, e *Env, root, slug string, files int) string {
	t.Helper()
	dir := filepath.Join(e.Path(root), fmt.Sprintf("%s.%d.%s", slug, stalePid, "abcd1234"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range files {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The regression: GCStale runs from EnsureDirs on every command, before any
// output. It must hand the unlinking off rather than do it on the calling
// goroutine, or a killed high-worker run makes every later startup wait out a
// full recursive delete. Blocking the remove is what makes that testable: the
// old inline version would never return.
func TestGCStaleDoesNotUnlinkInline(t *testing.T) {
	e := testEnv(t)
	dir := writeStaleDir(t, e, DirWork, "cross_x86_64-linux-musl", 8)

	release := make(chan struct{})
	defer close(release)
	orig := removeAll
	removeAll = func(p string) error {
		<-release
		return orig(p)
	}
	defer func() { removeAll = orig }()

	done := make(chan struct{})
	go func() { defer close(done); e.GCStale(0) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("GCStale blocked on the deletion instead of deferring it")
	}

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the stale dir is still in work/: %v", err)
	}
	if ents, _ := os.ReadDir(e.Path(DirWork)); len(ents) != 0 {
		t.Fatalf("work/ should be empty once retired, got %v", ents)
	}
}

// Trash left by a process that died mid-delete is nobody's job otherwise, so it
// would accumulate forever.
func TestGCStaleSweepsAbandonedTrash(t *testing.T) {
	e := testEnv(t)
	orphan := filepath.Join(e.Path(DirTrash), "leftover.999.dead")
	if err := os.MkdirAll(filepath.Join(orphan, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	e.GCStale(0)
	trashWG.Wait()

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("abandoned trash survived: %v", err)
	}
}

// A live process's in-flight deletion must not be adopted by another collector.
func TestGCStaleLeavesFreshTrashAlone(t *testing.T) {
	e := testEnv(t)
	fresh := filepath.Join(e.Path(DirTrash), "inflight.123.beef")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}

	e.GCStale(time.Hour)
	trashWG.Wait()

	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh trash must be left to its owner: %v", err)
	}
}

// Two gccfactory processes share one dist/; the loser of the rename race must
// not report an error or crash. Renaming makes the race decidable, where two
// concurrent RemoveAll walks would just interleave.
func TestGCStaleConcurrentCollectorsAreSafe(t *testing.T) {
	e := testEnv(t)
	for i := range 8 {
		writeStaleDir(t, e, DirWork, fmt.Sprintf("job%d", i), 4)
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.GCStale(0)
		}()
	}
	wg.Wait()
	trashWG.Wait()

	if ents, _ := os.ReadDir(e.Path(DirWork)); len(ents) != 0 {
		t.Fatalf("work/ should be empty, got %v", ents)
	}
}
