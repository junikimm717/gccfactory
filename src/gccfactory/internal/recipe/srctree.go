package recipe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/core"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/sources"
)

// The extra level keeps .gccfactory.json out of the tree, so `cp -al` never
// drops a manifest into a gcc source directory.
const srcTreeSubdir = "tree"

// srcTree is a source tree that has been extracted, patched and had every
// config.sub in it replaced with the pinned one, published once and then
// hardlink-copied into each build.
//
// Extracting gcc takes about two minutes; six builds would otherwise pay it six
// times. Making it a first-class job also means the patch step gets the same
// locking, logging, atomic publish and staleness handling as everything else --
// and a broken patch fails one small job instead of a four-hour toolchain.
type srcTree struct {
	s sources.Source
	// Set only when this package ships patches for it, so an unpatched arch
	// stays on the single shared artifact.
	arch string
}

func srcTreeJob(pkg string) *srcTree { return srcTreeJobFor(pkg, "") }

// Falls back to the shared tree unless the package really carries patches for
// arch, which is what made adding this a no-op for every existing key.
func srcTreeJobFor(pkg, arch string) *srcTree {
	s := src(pkg)
	if arch != "" && !hasArchPatches(s, arch) {
		arch = ""
	}
	return intern(srcTreeSlugFor(s, arch), func() *srcTree { return &srcTree{s: s, arch: arch} })
}

func hasArchPatches(s sources.Source, arch string) bool {
	ps, err := archPatchesFor(s, arch)
	// A malformed layout must reach the key, not silently widen the dep.
	return err != nil || len(ps) > 0
}

func srcTreeSlug(s sources.Source) string { return srcTreeSlugFor(s, "") }

func srcTreeSlugFor(s sources.Source, arch string) string {
	slug := "srctree_" + s.Name + "-" + s.Version
	if arch != "" {
		slug += "_" + arch
	}
	return slug
}

func (j *srcTree) Name() string     { return "srctree" }
func (j *srcTree) Slug() string     { return srcTreeSlugFor(j.s, j.arch) }
func (j *srcTree) Deps() []core.Job { return nil }

func (j *srcTree) ArtifactDir(e *core.Env) string {
	dir := j.s.Name + "-" + j.s.Version
	if j.arch != "" {
		dir += "_" + j.arch
	}
	return e.Path("srctrees", dir)
}

func (j *srcTree) KeyInputs() map[string]string {
	cs := src(pkgConfigSub)
	in := map[string]string{
		"recipe_version": strconv.Itoa(Version),
		"package":        j.s.Name,
		"version":        j.s.Version,
		"sha256":         j.s.SHA256,
		"config_sub":     cs.Version + ":" + cs.SHA256,
	}
	if h := patchSetHash(j.s); h != "" {
		in["patches"] = h
	}
	if j.arch != "" {
		in["arch"] = j.arch
		in["arch_patches"] = archPatchSetHash(j.s, j.arch)
	}
	return in
}

// The move is a rename within dist/, so the artifact is never observed
// half-patched.
func (j *srcTree) Build(ctx context.Context, e *core.Env, r *core.Runner, work, stage string) error {
	b := &builder{e: e, r: r, cfg: buildCfg{Work: work, Stage: stage}}

	b.step("fetch")
	tarball, err := fetch(ctx, e, j.s)
	if err != nil {
		return err
	}
	cfgSub, err := fetch(ctx, e, src(pkgConfigSub))
	if err != nil {
		return err
	}

	b.step("extract")
	tree := filepath.Join(work, "tree")
	if err := extract(ctx, j.s, tarball, tree); err != nil {
		return err
	}

	patches, err := patchesFor(j.s)
	if err != nil {
		return err
	}
	archPatches, err := archPatchesFor(j.s, j.arch)
	if err != nil {
		return err
	}
	patches = append(patches, archPatches...)
	if len(patches) > 0 {
		b.step("patch")
		dir := filepath.Join(work, "patches")
		if err := b.mkdirs(dir); err != nil {
			return err
		}
		for i, p := range patches {
			f := filepath.Join(dir, p.Name)
			if err := os.WriteFile(f, p.Data, 0o644); err != nil {
				return fmt.Errorf("recipe: write patch %s: %w", p.Name, err)
			}
			// A rejected hunk means the patch set and the tarball disagree.
			// That is never something to carry on from.
			name := fmt.Sprintf("patch-%02d-%s", i+1, p.Name)
			if err := b.run(ctx, name, tree, nil, "patch", "-p1", "--no-backup-if-mismatch", "-i", f); err != nil {
				return err
			}
		}
	}

	// Release config.sub predates every one of these tarballs, so fresh
	// architectures (riscv32, powerpc64le) are unrecognized until it is
	// replaced. Replace every copy, including the ones in bundled subprojects.
	b.step("config-sub")
	if err := b.run(ctx, "config-sub", tree, nil,
		"find", ".", "-name", "config.sub", "-exec", "cp", cfgSub, "{}", ";"); err != nil {
		return err
	}

	b.step("publish")
	if err := os.Rename(tree, filepath.Join(stage, srcTreeSubdir)); err != nil {
		return fmt.Errorf("recipe: publish source tree: %w", err)
	}
	return nil
}

func srcTreeJobs(pkgs ...string) []core.Job {
	out := make([]core.Job, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, srcTreeJob(p))
	}
	return out
}

// gccSrcPkgs get symlinked into the gcc source tree and built in-tree.
var gccSrcPkgs = []string{pkgGCC, pkgGMP, pkgMPFR, pkgMPC, pkgISL}
