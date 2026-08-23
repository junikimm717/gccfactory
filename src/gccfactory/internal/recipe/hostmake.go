package recipe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/core"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// hostMake builds GNU make as a static binary for H, so the shipped toolchain
// is self-contained: musl-cross-make's output includes make, and a toolchain
// you cannot drive is not a toolchain.
type hostMake struct {
	h triple.Triple
}

func hostMakeJob(h triple.Triple) *hostMake {
	return intern("hostmake_"+h.Raw, func() *hostMake { return &hostMake{h: h} })
}

func (j *hostMake) Name() string { return "hostmake" }
func (j *hostMake) Slug() string { return "hostmake_" + j.h.Raw }

// Deps includes the gcc source tree only because its config.guess is where the
// BUILD triple comes from; nothing of gcc is compiled here.
func (j *hostMake) Deps() []core.Job {
	return append([]core.Job{Cross(j.h)}, srcTreeJobs(pkgMake, pkgGCC)...)
}

func (j *hostMake) ArtifactDir(e *core.Env) string {
	return e.Path(core.DirToolchains, "hostmake", j.h.Raw)
}

func (j *hostMake) KeyInputs() map[string]string {
	c := buildCfg{Work: keyWork, Stage: keyStage, Build: keyBuild, Host: j.h, Target: j.h}
	in := baseInputs()
	in["host"] = j.h.Raw
	in["make_config"] = joinFlags(hostMakeConfig(c))
	addSources(in, pkgMake)
	return in
}

func (j *hostMake) Build(ctx context.Context, e *core.Env, r *core.Runner, work, stage string) error {
	b := &builder{e: e, r: r, cfg: buildCfg{Work: work, Stage: stage, Host: j.h, Target: j.h}}
	crossH := Cross(j.h).ArtifactDir(e)

	b.step("prepare-sources")
	if err := b.hardlinkSrc(ctx, pkgMake, srcTreeJob(pkgMake).ArtifactDir(e), "make"); err != nil {
		return err
	}
	build, err := b.resolveBuildTriple(ctx)
	if err != nil {
		return err
	}
	b.cfg.Build = build
	c := b.cfg

	// cross:<H>/bin supplies <H>-gcc and the whole <H>-* tool set that
	// configure derives from --host.
	env := map[string]string{"PATH": pathWith(filepath.Join(crossH, "bin"))}

	b.step("make-configure")
	if err := b.mkdirs(c.obj("make")); err != nil {
		return err
	}
	if err := b.run(ctx, "make-configure", c.obj("make"), env,
		append([]string{filepath.Join(c.src("make"), "configure")}, hostMakeConfig(c)...)...); err != nil {
		return err
	}
	b.step("make-build")
	if err := b.make(ctx, "make-build", c.obj("make"), env, makeVars{}); err != nil {
		return err
	}
	b.step("make-install")
	if err := b.make(ctx, "make-install", c.obj("make"), env, makeVars{}, "DESTDIR="+c.Stage, "install"); err != nil {
		return err
	}

	// Only the binary is wanted; the man pages and info files would collide
	// with nothing useful in the final toolchain.
	b.step("prune")
	for _, d := range []string{"share", "include", "lib"} {
		if err := os.RemoveAll(filepath.Join(c.Stage, d)); err != nil {
			return fmt.Errorf("recipe: prune %s: %w", d, err)
		}
	}

	b.step("verify")
	if skipVerify() {
		b.e.Log.Warn("skipping verification", "reason", "GCCF_SKIP_VERIFY set", "job", j.Slug())
		return nil
	}
	static := true
	bin := filepath.Join(c.Stage, "bin", "make")
	if err := expectELF(bin, j.h, &static); err != nil {
		return fmt.Errorf("recipe: %s is not a static %s binary: %w", bin, j.h.Raw, err)
	}
	b.e.Log.Info("hostmake verified", "path", bin, "host", j.h.Raw, "static", true)
	return nil
}
