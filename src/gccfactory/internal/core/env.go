// Package core is the build engine: content-addressed job keys, the
// crash-safe artifact store, cross-process locking, and the scheduler that
// turns a job DAG into parallel work.
//
// The invariants every other package can rely on:
//
//   - An artifact directory is either absent or complete. It is published by
//     a single rename, with its manifest written last.
//   - A job is rebuilt only when its Merkle key changes.
//   - Two gccfactory processes may share one dist/ safely.
package core

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/logging"
)

// Subdirectories of dist/.
const (
	DirSrc        = "src"
	DirToolchains = "toolchains"
	DirWork       = "work"
	DirStaging    = ".staging"
	DirTrash      = ".trash"
	DirLocks      = "locks"
	DirLogs       = "logs"
	DirState      = "state"
)

// ManifestName is the stamp file that makes an artifact directory valid.
const ManifestName = ".gccfactory.json"

// StaleAge is how old a pid-tagged scratch directory must be, on top of having
// a dead owner, before startup GC removes it.
const StaleAge = 10 * time.Minute

// Env is the global runtime context shared by every job. Set Dist to an
// absolute path: EnsureDirs will otherwise rewrite it, which is unsafe once
// other goroutines are reading the Env.
type Env struct {
	Dist       string
	RepoRoot   string
	Jobs       int // -j parallelism handed to make
	MaxWorkers int
	QemuHost   string // qemu-user binary for HOST-arch binaries
	QemuTarget string // qemu-user binary for TARGET-arch binaries
	KeepWork   bool   // retain work trees after a successful build, for `shell`
	Log        *logging.Logger
}

func (e *Env) Path(parts ...string) string {
	return filepath.Join(append([]string{e.Dist}, parts...)...)
}

func (e *Env) Workers() int {
	if e.MaxWorkers < 1 {
		return 1
	}
	return e.MaxWorkers
}

func (e *Env) MakeJobs() int {
	if e.Jobs < 1 {
		return runtime.NumCPU()
	}
	return e.Jobs
}

// LockPath is the flock file for a job slug. Lock files are never deleted:
// removing one would break flock identity for anyone holding it open.
func (e *Env) LockPath(slug string) string { return e.Path(DirLocks, slug+".lock") }

// JobLogDir is the parent of all attempt directories.
func (e *Env) JobLogDir(slug string) string { return e.Path(DirLogs, "jobs", slug) }

var distSubdirs = []string{
	DirSrc,
	DirToolchains,
	DirWork,
	DirStaging,
	DirTrash,
	DirLocks,
	filepath.Join(DirLogs, "runs"),
	filepath.Join(DirLogs, "jobs"),
	filepath.Join(DirState, "heartbeats"),
}

// EnsureDirs creates the dist/ skeleton and opportunistically collects scratch
// directories left behind by processes that died.
func (e *Env) EnsureDirs() error {
	if e.Dist == "" {
		return fmt.Errorf("core: Env.Dist is empty")
	}
	abs, err := filepath.Abs(e.Dist)
	if err != nil {
		return fmt.Errorf("core: resolve dist: %w", err)
	}
	if abs != e.Dist { // never write when unchanged: Env is read concurrently
		e.Dist = abs
	}
	for _, d := range distSubdirs {
		if err := os.MkdirAll(e.Path(d), 0o755); err != nil {
			return fmt.Errorf("core: create %s: %w", d, err)
		}
	}
	e.GCStale(StaleAge)
	return nil
}

// GCStale retires work/ and .staging/ directories whose owning pid is gone and
// which have not been touched for at least age, plus heartbeats left behind by
// dead builders. It is best-effort: losing a race with another collector is
// harmless.
//
// EnsureDirs calls this on every command before anything is printed, and
// unlinking a scratch tree is hundreds of thousands of syscalls: a killed
// --workers 96 run leaves one tree per worker, which turned every subsequent
// startup into minutes of silence. Renaming is O(1); Run does the deleting.
func (e *Env) GCStale(age time.Duration) {
	e.gcHeartbeats()
	var doomed []string
	for _, root := range []string{e.Path(DirWork), e.Path(DirStaging)} {
		ents, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, ent := range ents {
			if !ent.IsDir() {
				continue
			}
			pid, ok := pidFromScratchName(ent.Name())
			if !ok || pidAlive(pid) {
				continue
			}
			info, err := ent.Info()
			if err != nil || time.Since(info.ModTime()) < age {
				continue
			}
			path := filepath.Join(root, ent.Name())
			trash, err := e.retire(path, ent.Name())
			if err != nil {
				// A concurrent collector getting there first is the expected race.
				if !os.IsNotExist(err) {
					e.Log.Warn("could not retire stale dir", "path", path, "err", err)
				}
				continue
			}
			e.Log.Info("collecting stale scratch dir", "path", path, "dead_pid", pid)
			doomed = append(doomed, trash)
		}
	}
	sweepTrash(append(doomed, e.abandonedTrash(age)...))
}

func (e *Env) retire(path, name string) (string, error) {
	trash := e.Path(DirTrash, fmt.Sprintf("%s.%d.%s", name, os.Getpid(), randHex(4)))
	if err := os.MkdirAll(e.Path(DirTrash), 0o755); err != nil {
		return "", err
	}
	return trash, os.Rename(path, trash)
}

// The age bound is what keeps this from adopting a live process's in-flight
// deletion.
func (e *Env) abandonedTrash(age time.Duration) []string {
	ents, err := os.ReadDir(e.Path(DirTrash))
	if err != nil {
		return nil
	}
	var out []string
	for _, ent := range ents {
		info, err := ent.Info()
		if err != nil || time.Since(info.ModTime()) < age {
			continue
		}
		out = append(out, e.Path(DirTrash, ent.Name()))
	}
	return out
}

// gcHeartbeats drops heartbeat files whose builder is gone, so `status` never
// reports a build that a crash ended long ago.
func (e *Env) gcHeartbeats() {
	dir := e.Path(DirState, "heartbeats")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, ent := range ents {
		name := ent.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		hb, err := ReadHeartbeat(e, strings.TrimSuffix(name, ".json"))
		if err == nil && hb.Live() {
			continue
		}
		os.Remove(filepath.Join(dir, name))
	}
}

// scratchName builds "<slug>.<pid>.<rand>", the naming convention that makes
// abandoned directories attributable to a dead process.
func scratchName(slug string) string {
	return fmt.Sprintf("%s.%d.%s", slug, os.Getpid(), randHex(4))
}

func pidFromScratchName(name string) (int, bool) {
	parts := strings.Split(name, ".")
	if len(parts) < 3 {
		return 0, false
	}
	pid, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// pidAlive reports whether pid names a live process on this machine. A dist/
// shared across machines would defeat this; the mtime guard keeps that case
// merely wasteful rather than dangerous.
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
