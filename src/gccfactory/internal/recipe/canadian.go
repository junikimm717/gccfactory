package recipe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/core"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/ensure"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// canadian is the deliverable: build on B, run on H, emit code for T.
//
// The insight that makes arbitrary H x T work is that a toolchain is two
// disjoint halves:
//
//	host code    gcc, cc1, cc1plus, ld, as, ...   depends on H, not on B
//	target code  libc, libgcc, libstdc++, crt*.o  depends on T, not on H or B
//
// The target half was already built, correctly, by cross:<T>. So this job
// copies that half verbatim and compiles only the host half, with
// `make all-host` / `make install-host`. musl-cross-make instead rebuilds
// everything and therefore only supports H == T; this is the departure that
// buys us the full matrix.
type canadian struct {
	h, t triple.Triple

	// rung records which entry of the fallback ladder actually produced the
	// host compiler. It is written during Build and read back by KeyInputs so
	// the manifest says how the artifact was made.
	//
	// This does not destabilize the key: core resolves and memoizes every key
	// before any Build runs, and a fresh process re-derives the same key from
	// the same empty rung. Only the manifest's human-readable inputs differ.
	mu   sync.Mutex
	rung string
}

func canadianJob(h, t triple.Triple) *canadian {
	return intern(canadianSlug(h, t), func() *canadian { return &canadian{h: h, t: t} })
}

func canadianSlug(h, t triple.Triple) string { return "canadian_" + h.Raw + "__" + t.Raw }

func (j *canadian) Name() string { return "canadian" }
func (j *canadian) Slug() string { return canadianSlug(j.h, j.t) }

func (j *canadian) Deps() []core.Job {
	deps := []core.Job{Cross(j.h), Cross(j.t), HostMake(j.h)}
	return append(deps, srcTreeJobs(append([]string{pkgBinutils}, gccSrcPkgs...)...)...)
}

func (j *canadian) ArtifactDir(e *core.Env) string {
	return e.Path(core.DirToolchains, "out", j.h.Raw, j.t.Raw)
}

func (j *canadian) KeyInputs() map[string]string {
	c := keyCfg(j.h, j.t, true)
	in := baseInputs()
	in["host"] = j.h.Raw
	in["target"] = j.t.Raw
	in["target_gcc_config"] = joinFlags(j.t.GCCConfig())
	in["binutils_config"] = joinFlags(binutilsConfig(c))
	in["gcc_config"] = joinFlags(gccConfig(c))
	in["host_cc"] = hostCC(j.h)
	in["host_cxx"] = hostCXX(j.h)
	in["sysroot_layout"] = sysrootLayout
	if c.HostEqualsTarget() {
		in["sysroot_layout"] += "," + nativeLayout
	}
	in["fallback_ladder"] = strings.Join(rungNames(), " -> ")
	addSources(in, pkgBinutils, pkgGCC, pkgGMP, pkgMPFR, pkgMPC, pkgISL)
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.rung != "" {
		in["host_stage_used"] = j.rung
	}
	return in
}

// -static for gcc, --static for libtool. Both are needed, and the pair is
// musl-cross-make's own recipe. Static host binaries make the toolchain
// runnable under qemu-user with no sysroot and deployable by untarring it.
func hostCC(h triple.Triple) string  { return h.Raw + "-gcc -static --static" }
func hostCXX(h triple.Triple) string { return h.Raw + "-g++ -static --static" }

// Rung 1 is the real path and it is known to work: both directions of the
// x86_64/aarch64 matrix were built end to end on rung 1 alone. fixincludes was
// never the hazard it looked like -- `fixincl` is compiled with CC_FOR_BUILD
// and runs on the build machine quite happily -- the earlier failure was the
// missing `usr` in the build sysroot, which BuildSysroot now supplies.
//
// The lower rungs are kept as an escape hatch for targets nobody has run yet,
// not as an expected outcome. If one of them ever fires, that is a finding
// worth chasing, which is why the rung that won is recorded in the manifest.

type rung struct {
	name string
	// noFixincludes adds --disable-fixincludes (a real gcc >= 12 option).
	// For musl targets include-fixed/ contains only a README, so skipping
	// fixincludes costs nothing but build time.
	noFixincludes bool
	// full builds everything rather than only host code, naming the installed
	// B->T tools explicitly. This is mcm's NATIVE path; the target half it
	// produces is then overwritten by the cross:<T> copy again.
	full bool
}

var rungs = []rung{
	{name: "all-host"},
	{name: "all-host+disable-fixincludes", noFixincludes: true},
	{name: "full+for-target", noFixincludes: true, full: true},
}

func rungNames() []string {
	out := make([]string, len(rungs))
	for i, r := range rungs {
		out[i] = r.name
	}
	return out
}

func (j *canadian) Build(ctx context.Context, e *core.Env, r *core.Runner, work, stage string) error {
	crossH := Cross(j.h).ArtifactDir(e)
	crossT := Cross(j.t).ArtifactDir(e)
	hmake := HostMake(j.h).ArtifactDir(e)

	b := &builder{e: e, r: r, cfg: buildCfg{
		Work: work, Stage: stage,
		Host: j.h, Target: j.t, Canadian: true,
		CrossH: crossH, CrossT: crossT, HostMake: hmake,
	}}

	if err := b.prepareSources(ctx, true); err != nil {
		return err
	}
	build, err := b.resolveBuildTriple(ctx)
	if err != nil {
		return err
	}
	b.cfg.Build = build

	// Everything below compiles with the B->H compiler and assembles with the
	// B->T tools, so both cross toolchains go on PATH. crossT first: its <T>-*
	// binaries are the ones gcc's configure probes for.
	env := map[string]string{
		"PATH": pathWith(filepath.Join(crossT, "bin"), filepath.Join(crossH, "bin")),
		"CC":   hostCC(j.h),
		"CXX":  hostCXX(j.h),
	}

	if err := b.prepareBuildSysroot(ctx); err != nil {
		return err
	}
	if err := b.seedTarget(ctx); err != nil {
		return err
	}

	c := b.cfg
	b.step("binutils-configure")
	if err := b.mkdirs(c.obj("binutils")); err != nil {
		return err
	}
	if err := b.run(ctx, "binutils-configure", c.obj("binutils"), env,
		append([]string{filepath.Join(c.src("binutils"), "configure")}, binutilsConfig(c)...)...); err != nil {
		return err
	}
	b.step("binutils-make")
	if err := b.make(ctx, "binutils-make", c.obj("binutils"), env, makeVars{}); err != nil {
		return err
	}
	b.step("binutils-install")
	if err := b.make(ctx, "binutils-install", c.obj("binutils"), env, makeVars{}, "DESTDIR="+c.Stage, "install"); err != nil {
		return err
	}

	used, err := b.gccHostStage(ctx, env)
	if err != nil {
		return err
	}
	j.mu.Lock()
	j.rung = used.name
	j.mu.Unlock()
	e.Log.Info("canadian gcc host stage complete", "rung", used.name, "host", j.h.Raw, "target", j.t.Raw)

	if used.full {
		// A full build produced its own target libraries with a different
		// compiler than cross:<T> used. Put the known-good ones back.
		if err := b.seedTarget(ctx); err != nil {
			return err
		}
	}

	b.step("install-make")
	if err := b.mkdirs(filepath.Join(c.Stage, "bin")); err != nil {
		return err
	}
	if err := b.run(ctx, "install-make", c.Work, nil,
		"cp", filepath.Join(hmake, "bin", "make"), filepath.Join(c.Stage, "bin", "make")); err != nil {
		return err
	}
	if err := b.aliasNativeTools(ctx); err != nil {
		return err
	}
	if err := b.installCCSymlink(ctx); err != nil {
		return err
	}
	if err := b.stripHost(ctx); err != nil {
		return err
	}

	return b.verifyCanadian(ctx)
}

// seedTarget copies the target half of cross:<T> into the stage. Those files
// are pure target code: nothing in them depends on which machine the compiler
// runs on, so copying is not a shortcut, it is the correct answer.
//
// What is copied is deliberately narrow, because cross:<T> also contains BUILD
// binaries that would be poison here:
//
//   - <T>/bin/ holds ten BUILD-arch copies of ar/as/ld/... . binutils' install
//     does re-create all ten, but relying on that ordering is how you end up
//     shipping the wrong architecture the day someone reorders a step. Only
//     <T>/include and <T>/lib are seeded.
//   - lib/gcc/<T>/<ver>/plugin/ holds libcc1plugin/libcp1plugin, which are
//     BUILD-arch glibc shared objects. install-host does NOT overwrite them --
//     a `-static --static` host build produces no shared plugins at all -- so
//     they would ship verbatim. Only objects and archives are seeded;
//     include/, include-fixed/ and install-tools/ come from install-host,
//     which was verified to install all three.
//
// aliasNativeTools adds the <T>-prefixed names. When host == target, binutils
// configures as native and installs unprefixed tools only.
func (b *builder) aliasNativeTools(ctx context.Context) error {
	if !b.cfg.HostEqualsTarget() {
		return nil
	}
	b.step("alias-native-tools")
	return b.sh(ctx, "alias-native-tools", filepath.Join(b.cfg.Stage, "bin"), nil,
		nativeToolAliasScript(b.cfg.Target.Raw, ensure.BinutilsTools))
}

func (b *builder) seedTarget(ctx context.Context) error {
	b.step("seed-target")
	c := b.cfg
	crossSysroot := filepath.Join(c.CrossT, c.Target.Raw)
	if err := b.mkdirs(filepath.Join(c.StageSysroot(), "include"), filepath.Join(c.StageSysroot(), "lib"),
		filepath.Join(c.Stage, "lib", "gcc")); err != nil {
		return err
	}
	for _, d := range seedSysrootDirs {
		if err := b.run(ctx, "seed-target-"+d, c.Work, nil,
			"cp", "-a", filepath.Join(crossSysroot, d)+"/.", filepath.Join(c.StageSysroot(), d)+"/"); err != nil {
			return err
		}
	}
	if err := b.sh(ctx, "seed-target-libgcc", filepath.Join(c.CrossT, "lib", "gcc"), nil,
		libgccSeedScript(filepath.Join(c.Stage, "lib", "gcc"))); err != nil {
		return err
	}
	if c.HostEqualsTarget() {
		if err := b.sh(ctx, "seed-target-cxx-native", c.Stage, nil,
			nativeGxxSeedScript(c.Target.Raw, c.NativeGxxIncludeDir())); err != nil {
			return err
		}
	}
	return b.finalizeSysroot(ctx, c.StageSysroot())
}

// prepareBuildSysroot stages the directory --with-build-sysroot points at. See
// buildCfg.BuildSysroot for why cross:<T>'s installed sysroot cannot be used
// directly.
func (b *builder) prepareBuildSysroot(ctx context.Context) error {
	b.step("prepare-build-sysroot")
	c := b.cfg
	bs := c.BuildSysroot()
	if err := b.mkdirs(bs); err != nil {
		return err
	}
	crossSysroot := filepath.Join(c.CrossT, c.Target.Raw)
	if err := b.sh(ctx, "prepare-build-sysroot", bs, nil, buildSysrootScript(crossSysroot)); err != nil {
		return err
	}
	return b.scratchSysrootLinks(ctx, "prepare-build-sysroot-links", bs)
}

// gccHostStage walks the fallback ladder, returning the rung that worked. Each
// rung reconfigures from scratch, because a gcc object tree cannot be
// meaningfully reconfigured in place.
func (b *builder) gccHostStage(ctx context.Context, env map[string]string) (rung, error) {
	var errs []string
	for i, rg := range rungs {
		b.cfg.NoFixincludes = rg.noFixincludes
		b.cfg.ForTargetTools = rg.full
		err := b.gccHostAttempt(ctx, env, rg, i)
		if err == nil {
			return rg, nil
		}
		errs = append(errs, fmt.Sprintf("  [%d] %s: %v", i+1, rg.name, err))
		if i == len(rungs)-1 {
			break
		}
		b.e.Log.Warn("canadian gcc rung failed, falling back",
			"failed", rg.name, "next", rungs[i+1].name, "err", err)
		if err := os.RemoveAll(b.cfg.obj("gcc")); err != nil {
			return rung{}, fmt.Errorf("recipe: clearing obj_gcc before fallback: %w", err)
		}
	}
	return rung{}, fmt.Errorf("recipe: every gcc host-stage strategy failed for %s -> %s:\n%s",
		b.cfg.Host.Raw, b.cfg.Target.Raw, strings.Join(errs, "\n"))
}

// Retries get a step suffix so their logs sit alongside the failed attempt's
// rather than over it.
func (b *builder) gccHostAttempt(ctx context.Context, env map[string]string, rg rung, idx int) error {
	c := b.cfg
	suffix := ""
	if idx > 0 {
		suffix = fmt.Sprintf("-retry%d", idx+1)
	}
	b.step("gcc-configure" + suffix)
	if err := b.mkdirs(c.obj("gcc")); err != nil {
		return err
	}
	if err := b.run(ctx, "gcc-configure"+suffix, c.obj("gcc"), env,
		append([]string{filepath.Join(c.src("gcc"), "configure")}, gccConfig(c)...)...); err != nil {
		return err
	}
	buildTarget, installTarget := "all-host", "install-host"
	if rg.full {
		buildTarget, installTarget = "all", "install"
	}
	b.step("gcc-" + buildTarget + suffix)
	if err := b.make(ctx, "gcc-"+buildTarget+suffix, c.obj("gcc"), env, makeVars{}, buildTarget); err != nil {
		return err
	}
	b.step("gcc-" + installTarget + suffix)
	return b.make(ctx, "gcc-"+installTarget+suffix, c.obj("gcc"), env, makeVars{}, "DESTDIR="+c.Stage, installTarget)
}

// Best effort: bin/ also holds shell wrappers and symlinks, and strip failing
// on those is not interesting.
func (b *builder) stripHost(ctx context.Context) error {
	b.step("strip")
	c := b.cfg
	strip := filepath.Join(c.CrossH, "bin", c.Host.Raw+"-strip")
	script := fmt.Sprintf("find %s %s -type f -exec %s {} ';' 2>&1 | tail -40 || true",
		shQuote(filepath.Join(c.Stage, "bin")), shQuote(filepath.Join(c.Stage, "libexec")), shQuote(strip))
	return b.sh(ctx, "strip", c.Work, nil, script)
}

// verifyCanadian is the real proof: the binaries are H ELFs, they run under
// qemu-H to compile the probe suite for T, and the results run under qemu-T.
func (b *builder) verifyCanadian(ctx context.Context) error {
	if skipVerify() {
		b.e.Log.Warn("skipping verification", "reason", "GCCF_SKIP_VERIFY set", "job", b.cfg.Host.Raw+"->"+b.cfg.Target.Raw)
		return nil
	}
	b.step("verify")
	// Host binaries are static, so no host sysroot is needed; naming cross:<H>'s
	// anyway keeps verification working if that ever changes.
	rep := ensure.CanadianToolchain(ctx, ensureRunner{b.r}, b.verifyWork(), b.cfg.Stage,
		b.cfg.Host, b.cfg.Target,
		qemuFor(b.e.QemuHost, b.cfg.Host), qemuFor(b.e.QemuTarget, b.cfg.Target),
		ensure.WithHostSysroot(filepath.Join(b.cfg.CrossH, b.cfg.Host.Raw)))
	b.e.Log.Info("verification report", "subject", rep.Subject, "ok", rep.OK())
	return reportErr(rep)
}
