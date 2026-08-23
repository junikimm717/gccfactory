package recipe

import (
	"context"
	"path/filepath"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/core"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/ensure"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// cross is the BUILD -> T toolchain: binutils, gcc, musl and kernel headers,
// built in the one order that works.
//
// The interleaving is the whole trick. libgcc cannot be built without libc
// headers, and libc cannot be built without libgcc, so the build walks a
// deliberate path through the cycle:
//
//	binutils                    ->  assembler and linker for T
//	gcc: all-gcc                ->  the compiler proper, no target libraries
//	musl: install-headers       ->  headers only, no code
//	gcc: all-target-libgcc      ->  libgcc.a, static, against those headers
//	musl: all + install         ->  libc, compiled by xgcc + that libgcc.a
//	gcc: all                    ->  shared libgcc, libstdc++, libatomic, ...
//
// Getting this order wrong is the single most common way to end up with a musl
// cross toolchain that almost works.
type cross struct {
	t triple.Triple
}

func crossJob(t triple.Triple) *cross {
	return intern("cross_"+t.Raw, func() *cross { return &cross{t: t} })
}

func (j *cross) Name() string { return "cross" }
func (j *cross) Slug() string { return "cross_" + j.t.Raw }

func (j *cross) Deps() []core.Job {
	return srcTreeJobs(append([]string{pkgBinutils, pkgMusl, pkgLinux}, gccSrcPkgs...)...)
}

func (j *cross) ArtifactDir(e *core.Env) string {
	return e.Path(core.DirToolchains, "cross", j.t.Raw)
}

func (j *cross) KeyInputs() map[string]string {
	c := keyCfg(triple.Triple{}, j.t, false)
	in := baseInputs()
	in["target"] = j.t.Raw
	in["target_gcc_config"] = joinFlags(j.t.GCCConfig())
	in["binutils_config"] = joinFlags(binutilsConfig(c))
	in["gcc_config"] = joinFlags(gccConfig(c))
	in["musl_config"] = joinFlags(muslConfig(c))
	in["musl_vars"] = joinFlags(muslVars(c))
	in["sysroot_layout"] = sysrootLayout
	if a, err := kernelArch(j.t); err == nil {
		in["linux_arch"] = a
	}
	addSources(in, pkgBinutils, pkgGCC, pkgMusl, pkgGMP, pkgMPFR, pkgMPC, pkgISL, pkgLinux)
	return in
}

func (j *cross) Build(ctx context.Context, e *core.Env, r *core.Runner, work, stage string) error {
	b := &builder{e: e, r: r, cfg: buildCfg{Work: work, Stage: stage, Target: j.t}}

	if err := b.prepareSources(ctx, false); err != nil {
		return err
	}
	build, err := b.resolveBuildTriple(ctx)
	if err != nil {
		return err
	}
	b.cfg.Build = build

	c := b.cfg
	if err := b.prepareObjSysroot(ctx); err != nil {
		return err
	}

	b.step("binutils-configure")
	if err := b.mkdirs(c.obj("binutils")); err != nil {
		return err
	}
	if err := b.run(ctx, "binutils-configure", c.obj("binutils"), nil,
		append([]string{filepath.Join(c.src("binutils"), "configure")}, binutilsConfig(c)...)...); err != nil {
		return err
	}
	b.step("binutils-make")
	if err := b.make(ctx, "binutils-make", c.obj("binutils"), nil, makeVars{}, "all"); err != nil {
		return err
	}

	b.step("gcc-configure")
	if err := b.mkdirs(c.obj("gcc")); err != nil {
		return err
	}
	if err := b.run(ctx, "gcc-configure", c.obj("gcc"), nil,
		append([]string{filepath.Join(c.src("gcc"), "configure")}, gccConfig(c)...)...); err != nil {
		return err
	}
	b.step("gcc-all-gcc")
	if err := b.make(ctx, "gcc-all-gcc", c.obj("gcc"), nil, makeVars{}, "all-gcc"); err != nil {
		return err
	}

	b.step("musl-configure")
	if err := b.mkdirs(c.obj("musl")); err != nil {
		return err
	}
	if err := b.run(ctx, "musl-configure", c.obj("musl"), nil,
		append([]string{filepath.Join(c.src("musl"), "configure")}, muslConfig(c)...)...); err != nil {
		return err
	}
	b.step("musl-install-headers")
	if err := b.make(ctx, "musl-install-headers", c.obj("musl"), nil, cmdVars(muslVars(c)...),
		"DESTDIR="+c.ObjSysroot(), "install-headers"); err != nil {
		return err
	}

	b.step("gcc-all-target-libgcc")
	// enable_shared=no rides in the MAKE variable so the recursive libgcc make
	// sees it: a shared libgcc cannot be linked before libc exists.
	if err := b.make(ctx, "gcc-all-target-libgcc", c.obj("gcc"), nil,
		innerVars("enable_shared=no"), "all-target-libgcc"); err != nil {
		return err
	}

	b.step("musl-make")
	if err := b.make(ctx, "musl-make", c.obj("musl"), nil, cmdVars(muslVars(c)...)); err != nil {
		return err
	}
	b.step("musl-install-sysroot")
	if err := b.make(ctx, "musl-install-sysroot", c.obj("musl"), nil, cmdVars(muslVars(c)...),
		"DESTDIR="+c.ObjSysroot(), "install"); err != nil {
		return err
	}

	b.step("gcc-rest")
	if err := b.make(ctx, "gcc-rest", c.obj("gcc"), nil, makeVars{}); err != nil {
		return err
	}

	if err := b.kernelHeaders(ctx); err != nil {
		return err
	}

	b.step("install-musl")
	if err := b.make(ctx, "install-musl", c.obj("musl"), nil, cmdVars(muslVars(c)...),
		"DESTDIR="+c.StageSysroot(), "install"); err != nil {
		return err
	}
	b.step("install-binutils")
	if err := b.make(ctx, "install-binutils", c.obj("binutils"), nil, makeVars{},
		"DESTDIR="+c.Stage, "install"); err != nil {
		return err
	}
	b.step("install-gcc")
	if err := b.make(ctx, "install-gcc", c.obj("gcc"), nil, makeVars{},
		"DESTDIR="+c.Stage, "install"); err != nil {
		return err
	}
	b.step("install-kernel-headers")
	if err := b.mkdirs(filepath.Join(c.StageSysroot(), "include")); err != nil {
		return err
	}
	if err := b.sh(ctx, "install-kernel-headers", c.Work, nil,
		"cp -R "+shQuote(filepath.Join(c.obj("kernel_headers"), "staged", "include"))+"/* "+
			shQuote(filepath.Join(c.StageSysroot(), "include"))+"/"); err != nil {
		return err
	}
	if err := b.finalizeSysroot(ctx, c.StageSysroot()); err != nil {
		return err
	}
	if err := b.installCCSymlink(ctx); err != nil {
		return err
	}

	return b.verifyCross(ctx)
}

// canadian builds skip musl and the kernel headers: they take those, already
// built, from cross:<T>.
func (b *builder) prepareSources(ctx context.Context, canadian bool) error {
	b.step("prepare-sources")
	type want struct{ pkg, as string }
	list := []want{
		{pkgBinutils, "binutils"},
		{pkgGCC, "gcc_base"},
		{pkgGMP, "gmp"},
		{pkgMPFR, "mpfr"},
		{pkgMPC, "mpc"},
		{pkgISL, "isl"},
	}
	if !canadian {
		list = append(list, want{pkgMusl, "musl"}, want{pkgLinux, "kernel_headers"})
	}
	for _, w := range list {
		art := srcTreeJob(w.pkg).ArtifactDir(b.e)
		if err := b.hardlinkSrc(ctx, w.pkg, art, w.as); err != nil {
			return err
		}
	}
	return b.linkGCCSrcTree(ctx)
}

func (b *builder) prepareObjSysroot(ctx context.Context) error {
	b.step("prepare-sysroot")
	s := b.cfg.ObjSysroot()
	if err := b.mkdirs(filepath.Join(s, "include")); err != nil {
		return err
	}
	return b.scratchSysrootLinks(ctx, "prepare-sysroot", s)
}

func (b *builder) kernelHeaders(ctx context.Context) error {
	b.step("kernel-headers-install")
	c := b.cfg
	arch, err := kernelArch(c.Target)
	if err != nil {
		return err
	}
	obj := c.obj("kernel_headers")
	staged := filepath.Join(obj, "staged")
	if err := b.mkdirs(staged); err != nil {
		return err
	}
	if err := b.make(ctx, "kernel-headers-install", c.src("kernel_headers"), nil, makeVars{},
		"ARCH="+arch, "O="+obj, "INSTALL_HDR_PATH="+staged, "headers_install"); err != nil {
		return err
	}
	// kbuild litters the staged tree with its own bookkeeping.
	return b.run(ctx, "kernel-headers-clean", filepath.Join(staged, "include"), nil,
		"find", ".", "(", "-name", ".install", "-o", "-name", "..install.cmd", ")", "-exec", "rm", "-f", "{}", "+")
}

// A surprising amount of configure script still looks for <T>-cc.
func (b *builder) installCCSymlink(ctx context.Context) error {
	b.step("install-cc-symlink")
	bin := filepath.Join(b.cfg.Stage, "bin")
	if err := b.mkdirs(bin); err != nil {
		return err
	}
	return b.run(ctx, "install-cc-symlink", bin, nil, "ln", "-sfn", b.cfg.Target.Raw+"-gcc", b.cfg.Target.Raw+"-cc")
}

// verifyCross proves the toolchain works before it is published: every binary
// is a BUILD ELF, and the probe suite compiles for T and runs under qemu.
func (b *builder) verifyCross(ctx context.Context) error {
	if skipVerify() {
		b.e.Log.Warn("skipping verification", "reason", "GCCF_SKIP_VERIFY set", "job", b.cfg.Target.Raw)
		return nil
	}
	b.step("verify")
	qemu := qemuFor(b.e.QemuTarget, b.cfg.Target)
	rep := ensure.CrossToolchain(ctx, ensureRunner{b.r}, b.verifyWork(), b.cfg.Stage, b.cfg.Target, qemu)
	b.e.Log.Info("verification report", "subject", rep.Subject, "ok", rep.OK())
	return reportErr(rep)
}
