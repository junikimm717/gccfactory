package recipe

// This file is the single seam between internal/recipe and the packages the
// other agents own (core / sources / ensure). Every signature we are not
// certain about is funnelled through here, so integrating is a mechanical edit
// to one file instead of a hunt across the package.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/core"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/ensure"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/sources"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// Source names as they appear in internal/sources/sources.json.
const (
	pkgBinutils  = "binutils"
	pkgGCC       = "gcc"
	pkgMusl      = "musl"
	pkgGMP       = "gmp"
	pkgMPFR      = "mpfr"
	pkgMPC       = "mpc"
	pkgISL       = "isl"
	pkgLinux     = "linux-headers"
	pkgMake      = "make"
	pkgConfigSub = "config.sub"
)

// The database is compiled in, so an unknown name is a programming error, not
// a runtime condition.
func src(name string) sources.Source {
	s, err := sources.Get(name)
	if err != nil {
		panic(fmt.Sprintf("recipe: unknown source %q: %v (see internal/sources/sources.json)", name, err))
	}
	return s
}

// fetch downloads and verifies a pinned source, returning the local file path.
func fetch(ctx context.Context, e *core.Env, s sources.Source) (string, error) {
	return sources.Fetch(ctx, e.Dist, s)
}

// extract unpacks a tarball into dst, which must not exist yet. dst becomes the
// source root: the archive's single top-level directory is stripped.
func extract(ctx context.Context, s sources.Source, tarball, dst string) error {
	return sources.Extract(ctx, tarball, dst, s)
}

// patchesFor returns the embedded patch set for a source, sorted by filename
// (the order they must be applied in).
func patchesFor(s sources.Source) ([]sources.Patch, error) {
	if err := checkArchDirs(s); err != nil {
		return nil, err
	}
	ps, err := sources.Patches(s)
	if err != nil {
		return nil, fmt.Errorf("recipe: patches for %s: %w", s.Slug(), err)
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Name < ps[j].Name })
	return ps, nil
}

// patchSetHash is a stable digest of a source's patch set. It goes into the
// job key so that editing or adding a diff rebuilds everything downstream.
func patchSetHash(s sources.Source) string {
	ps, err := patchesFor(s)
	if err != nil {
		// A broken embed must not silently collide with a working one.
		return "ERROR:" + err.Error()
	}
	if len(ps) == 0 {
		return ""
	}
	return hashPatches(ps)
}

// internal/ensure deliberately does not import internal/core; it declares its
// own Cmd/Runner. ensureRunner adapts our *core.Runner so probe compiles land
// in the same per-job log tree as the build that produced the toolchain.
//
// Run is for commands whose failure is a build failure; Output is for the
// probes themselves, where a non-zero exit is a Check result rather than a
// fatal error and so must not poison the job log.

type ensureRunner struct{ r *core.Runner }

var _ ensure.Runner = ensureRunner{}

func (a ensureRunner) Run(ctx context.Context, c ensure.Cmd) error {
	return a.r.Run(ctx, core.Cmd{Dir: c.Dir, EnvAdd: c.EnvAdd, Args: c.Args, Name: c.Name})
}

// Output keeps the same per-step log file as Run: a probe that fails is
// exactly the thing we most want a full transcript of.
func (a ensureRunner) Output(ctx context.Context, c ensure.Cmd) ([]byte, error) {
	out, err := a.r.Output(ctx, core.Cmd{Dir: c.Dir, EnvAdd: c.EnvAdd, Args: c.Args, Name: c.Name})
	return []byte(out), err
}

// qemuFor resolves the qemu-user binary for a triple from the template stored
// in core.Env. Both a directory (/usr/bin) and a printf template
// (/usr/bin/qemu-%s-static) are accepted, matching internal/cli.
func qemuFor(tmpl string, t triple.Triple) string {
	if tmpl == "" {
		return ""
	}
	if strings.Contains(tmpl, "%s") {
		return fmt.Sprintf(tmpl, t.QemuName())
	}
	return filepath.Join(tmpl, "qemu-"+t.QemuName()+"-static")
}

func expectELF(path string, t triple.Triple, wantStatic *bool) error {
	return ensure.ExpectELF(path, t, wantStatic)
}

// reportErr turns a failed ensure.Report into a build error carrying the whole
// pretty-printed report, so a verification failure is readable in one screen.
func reportErr(rep *ensure.Report) error {
	if rep == nil {
		return nil
	}
	return rep.Err()
}

// archScopedPkgs may carry per-architecture patch subdirectories. The list is
// deliberately short: musl and the kernel headers are the only sources built
// for exactly one architecture in every job that reads them, so "which arch
// does this patch affect" has a single answer. gcc and binutils are read by
// cross (emitting for the target) and canadian (running on the host) alike, so
// an arch subdirectory there would have two answers and silently pick one.
var archScopedPkgs = map[string]bool{pkgMusl: true, pkgLinux: true}

// archPatchesFor returns the patches scoped to one architecture. It rejects an
// arch directory under a package that cannot express one, rather than ignoring
// it: a patch that never applies is worse than a build error.
func archPatchesFor(s sources.Source, arch string) ([]sources.Patch, error) {
	if err := checkArchDirs(s); err != nil {
		return nil, err
	}
	ps, err := sources.ArchPatches(s, arch)
	if err != nil {
		return nil, fmt.Errorf("recipe: arch patches for %s/%s: %w", s.Slug(), arch, err)
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Name < ps[j].Name })
	return ps, nil
}

// checkArchDirs runs for every package, not just the scopable ones: an arch
// directory nobody reads applies to nothing, and silence would make that look
// like a landed patch.
func checkArchDirs(s sources.Source) error {
	arches := sources.PatchArches(s)
	if len(arches) == 0 || archScopedPkgs[s.Name] {
		return nil
	}
	return fmt.Errorf("recipe: %s has architecture patch dirs %v, but only %v may scope patches by architecture; move them up a level or the patches never apply",
		s.Slug(), arches, archScopedPkgNames())
}

func archScopedPkgNames() []string {
	out := make([]string, 0, len(archScopedPkgs))
	for k := range archScopedPkgs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// archPatchSetHash digests the arch-scoped patches only. It is empty when the
// architecture has no subdirectory, which keeps every key that exists today
// byte-identical.
func archPatchSetHash(s sources.Source, arch string) string {
	ps, err := archPatchesFor(s, arch)
	if err != nil {
		return "ERROR:" + err.Error()
	}
	if len(ps) == 0 {
		return ""
	}
	return hashPatches(ps)
}

func hashPatches(ps []sources.Patch) string {
	h := sha256.New()
	var n [8]byte
	for _, p := range ps {
		binary.BigEndian.PutUint64(n[:], uint64(len(p.Name)))
		h.Write(n[:])
		h.Write([]byte(p.Name))
		binary.BigEndian.PutUint64(n[:], uint64(len(p.Data)))
		h.Write(n[:])
		h.Write(p.Data)
	}
	return hex.EncodeToString(h.Sum(nil))
}
