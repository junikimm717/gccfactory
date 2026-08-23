// Package recipe turns the pinned upstream tarballs into toolchains.
//
// Three job families, and one DAG:
//
//	srctree:<pkg>-<ver>   extracted + patched + config.sub-refreshed source
//	cross:<T>             BUILD -> T toolchain            (needs srctrees)
//	hostmake:<H>          GNU make, static, runs on H     (needs cross:<H>)
//	canadian:<H>:<T>      the deliverable: runs on H, emits code for T
//	                      (needs cross:<H>, cross:<T>, hostmake:<H>)
//
// gmp/mpfr/mpc/isl are never separate jobs. They are symlinked into the gcc
// source tree and gcc's toplevel builds them for whichever --host is in effect,
// which is exactly why no host-dependency stage is needed.
//
// Everything is installed with an empty --prefix and a DESTDIR, so the result
// can be untarred anywhere. See config.go for why.
package recipe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/core"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// Version is the recipe generation. Bump it by hand whenever the *procedure*
// changes in a way the configure flags do not capture — a new step, a different
// ordering, a changed install layout. Every job key includes it, so bumping it
// rebuilds the world.
const Version = 1

// sysrootLayout describes the shape of a published sysroot. It is keyed
// separately from Version so that changing it rebuilds the toolchains without
// invalidating the (expensive, unrelated) srctree artifacts. Change the string
// whenever finalizeSysroot or the canadian seed changes what lands on disk.
const sysrootLayout = "usr-symlink,relative-ldso,seed-include-lib,seed-libgcc-objects"

// nativeLayout is what host == target additionally does; see toolchain-traps.
const nativeLayout = "native-gxx-dir,prefixed-tool-aliases"

// Cross returns the BUILD -> t toolchain job.
func Cross(t triple.Triple) core.Job { return crossJob(t) }

// HostMake returns the job producing a static GNU make that runs on h.
func HostMake(h triple.Triple) core.Job { return hostMakeJob(h) }

// Canadian returns the deliverable: a toolchain that runs on h and emits code
// for t.
func Canadian(h, t triple.Triple) core.Job { return canadianJob(h, t) }

// Matrix returns the DAG roots for a host x target selection.
//
//	hosts and targets   the canadian toolchains, one per pair
//	targets only        the BUILD -> target cross toolchains
//	hosts only          everything needed to host a toolchain on each host
//
// The one-sided forms exist because those intermediates are the cheapest way to
// bisect a failure.
func Matrix(hosts, targets []triple.Triple) []core.Job {
	var jobs []core.Job
	switch {
	case len(hosts) > 0 && len(targets) > 0:
		// H != T first: the diagonal is a different gcc configuration and a
		// weaker test, so it must not be what aborts the run.
		for _, diagonal := range []bool{false, true} {
			for _, h := range hosts {
				for _, t := range targets {
					if (h.Raw == t.Raw) == diagonal {
						jobs = append(jobs, Canadian(h, t))
					}
				}
			}
		}
	case len(targets) > 0:
		for _, t := range targets {
			jobs = append(jobs, Cross(t))
		}
	case len(hosts) > 0:
		for _, h := range hosts {
			jobs = append(jobs, Cross(h), HostMake(h))
		}
	}
	return jobs
}

// Constructors are memoized by slug so that Cross(x) called from ten places
// yields one object. core dedupes by slug anyway, but pointer identity makes
// that check free and lets canadianJob carry the fallback rung it discovered.

var jobCache sync.Map // slug -> core.Job

func intern[T core.Job](slug string, mk func() T) T {
	if v, ok := jobCache.Load(slug); ok {
		return v.(T)
	}
	j := mk()
	actual, _ := jobCache.LoadOrStore(slug, core.Job(j))
	return actual.(T)
}

// Steps are methods on it so no path is ever concatenated at a call site.
type builder struct {
	e   *core.Env
	r   *core.Runner
	cfg buildCfg
}

// It shows up in heartbeats, in `gccfactory status`, and as the prefix of the
// log file for every command that follows.
func (b *builder) step(name string) {
	b.r.Step(name)
	b.e.Log.Info("step", "step", name)
}

func (b *builder) run(ctx context.Context, name, dir string, envAdd map[string]string, argv ...string) error {
	return b.r.Run(ctx, core.Cmd{Name: name, Dir: dir, EnvAdd: envAdd, Args: argv})
}

// Used only where a glob, a redirect or `find -exec` is genuinely the clearest
// thing; the script text lands in the log header so it is never a mystery.
func (b *builder) sh(ctx context.Context, name, dir string, envAdd map[string]string, script string) error {
	return b.run(ctx, name, dir, envAdd, "sh", "-ec", script)
}

// make runs make in dir with the shared flags always applied.
func (b *builder) make(ctx context.Context, name, dir string, envAdd map[string]string, vars makeVars, args ...string) error {
	return b.run(ctx, name, dir, envAdd, makeArgv(b.e.MakeJobs(), vars, args...)...)
}

// It sits inside the job's work dir so a failed verification leaves the probe
// sources and objects behind next to the build that produced them.
func (b *builder) verifyWork() string { return filepath.Join(b.cfg.Work, "verify") }

func (b *builder) mkdirs(paths ...string) error {
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return fmt.Errorf("recipe: mkdir %s: %w", p, err)
		}
	}
	return nil
}

// `cp -al` is a hardlink copy: near-instant, and it isolates the file *set* so
// a build that creates or deletes files cannot disturb the shared tree.
func (b *builder) hardlinkSrc(ctx context.Context, pkg, artifact, as string) error {
	dst := b.cfg.src(as)
	return b.run(ctx, "src-"+pkg, b.cfg.Work, nil, "cp", "-al", filepath.Join(artifact, srcTreeSubdir), dst)
}

// linkGCCSrcTree builds mcm's src_gcc, with gmp/mpc/mpfr/isl grafted on so
// gcc's toplevel configures and builds them in-tree for whatever --host is in
// effect.
func (b *builder) linkGCCSrcTree(ctx context.Context) error {
	script := `
rm -rf src_gcc src_gcc.tmp
mkdir src_gcc.tmp
cd src_gcc.tmp
ln -sf ../src_gcc_base/* .
ln -sfn ../src_gmp gmp
ln -sfn ../src_mpc mpc
ln -sfn ../src_mpfr mpfr
ln -sfn ../src_isl isl
cd ..
mv src_gcc.tmp src_gcc
`
	return b.sh(ctx, "prepare-srcgcc", b.cfg.Work, nil, script)
}

// scratchSysrootLinks adds the three conventions gcc's target configs expect of
// a sysroot it is *building against*: `usr` folding back on itself, and lib32
// and lib64 folding onto lib (musl has no such split). mcm creates exactly
// these in obj_sysroot.
func (b *builder) scratchSysrootLinks(ctx context.Context, name, dir string) error {
	return b.sh(ctx, name, dir, nil, scratchSysrootScript)
}

// finalizeSysroot makes a *published* sysroot usable by everything else.
//
// Two things are wrong with it as musl and gcc leave it:
//
//   - There is no `usr`, so any tool following the /usr/include convention
//     (gcc's own BUILD_SYSTEM_HEADER_DIR among them) looks in the wrong place.
//   - musl installs ld-musl-<arch>.so.1 as an *absolute* symlink to
//     /lib/libc.so. Inside a relocatable sysroot that dangles, and
//     `qemu-user -L <sysroot>` cannot follow it, so every dynamically linked
//     target binary fails to start with "Could not open
//     '/lib/ld-musl-<arch>.so.1'". A relative link fixes both.
func (b *builder) finalizeSysroot(ctx context.Context, sysroot string) error {
	b.step("sysroot-finalize")
	return b.sh(ctx, "sysroot-finalize", sysroot, nil, finalizeSysrootScript)
}

func pathWith(dirs ...string) string {
	return strings.Join(append(dirs, os.Getenv("PATH")), string(os.PathListSeparator))
}

// Sentinel paths used when rendering configure flags for the *key*. Real work
// and artifact directories are pid-tagged and change every run; substituting
// them keeps the key a function of the recipe rather than of the filesystem.
const (
	keyWork     = "@WORK@"
	keyStage    = "@STAGE@"
	keyBuild    = "@BUILD@"
	keyCrossH   = "@CROSSH@"
	keyCrossT   = "@CROSST@"
	keyHostMake = "@HOSTMAKE@"
)

// The BUILD triple is deliberately a sentinel: resolving it for real means
// running config.guess out of the gcc source tree, which does not exist yet at
// planning time. `build_platform` below stands in for it — dist/ is machine
// local (pids, flocks, work dirs all assume one machine), so GOARCH/GOOS is a
// faithful proxy and moving a dist/ between architectures still invalidates.
func keyCfg(h, t triple.Triple, canadian bool) buildCfg {
	c := buildCfg{Work: keyWork, Stage: keyStage, Build: keyBuild, Target: t, Canadian: canadian}
	if canadian {
		c.Host, c.CrossH, c.CrossT, c.HostMake = h, keyCrossH, keyCrossT, keyHostMake
	}
	return c
}

func baseInputs() map[string]string {
	return map[string]string{
		"recipe_version": strconv.Itoa(Version),
		"build_platform": buildPlatform(),
		"make_flags":     strings.Join(makeFlags, " "),
	}
}

// A retagged tarball or an edited diff therefore rebuilds the world.
func addSources(in map[string]string, pkgs ...string) {
	for _, p := range pkgs {
		s := src(p)
		in["src_"+p] = s.Version + ":" + s.SHA256
		if h := patchSetHash(s); h != "" {
			in["patches_"+p] = h
		}
	}
}

func joinFlags(f []string) string { return strings.Join(f, " ") }

// skipVerify is the escape hatch for bisecting a broken pipeline: set
// GCCF_SKIP_VERIFY=1 to publish an artifact without proving it works. It is
// deliberately an environment variable and not a flag, because nobody should
// reach for it by accident.
func skipVerify() bool {
	v := os.Getenv("GCCF_SKIP_VERIFY")
	return v != "" && v != "0"
}
