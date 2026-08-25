package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/core"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/ensure"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/recipe"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

var cmdVerify = &command{
	Name:     "verify",
	Short:    "prove built toolchains actually work (compile + run under qemu)",
	Synopsis: "gccfactory verify [--host LIST] [--target LIST] [--native] [--cross]",
	Long: `Runs the ensure suite. This is a real proof, not a smoke test:

  * every binary in <prefix>/bin is checked to be an ELF for the HOST arch
  * the musl-cross-make tool surface (gcc, g++, ar, ld, strip, ..., make) must
    all be present and executable
  * the compilers are executed under qemu-<host> to compile a probe suite for
    TARGET (printf, libm, pthreads, TLS, atomics, C++ iostreams, exceptions,
    std::regex/thread, dlopen of a -fPIC shared object, and -static), at -O0 and
    -O2, and each resulting binary is ELF-checked for TARGET and then run under
    qemu-<target>

With no --host/--target, everything currently built in dist/ is verified, so
` + "`gccfactory verify`" + ` on its own is always a valid thing to type.

FLAGS
  --host LIST     restrict to these host triples
  --target LIST   restrict to these target triples
  --native        also verify the BUILD machine's own gcc/g++ (implied when no
                  toolchains are built yet)
  --cross         also verify the intermediate build->target cross toolchains

Exit status is non-zero if any check fails. Each check's log path is printed.`,
	Run: runVerify,
}

func runVerify(g *Global, args []string) error {
	fs := g.flagSet("verify")
	host := fs.String("host", "", tripleFlagHelp)
	target := fs.String("target", "", tripleFlagHelp)
	native := fs.Bool("native", false, "also verify the BUILD machine's gcc/g++")
	cross := fs.Bool("cross", false, "also verify the build->target cross toolchains")
	if err := parse(fs, args); err != nil {
		return finish("verify", err)
	}
	if err := g.resolve(); err != nil {
		return err
	}
	var hosts, targets []triple.Triple
	var err error
	if *host != "" {
		if hosts, err = parseTriples("host", *host); err != nil {
			return err
		}
	}
	if *target != "" {
		if targets, err = parseTriples("target", *target); err != nil {
			return err
		}
	}

	e, done, err := g.env(defaultJobs, defaultWorkers)
	if err != nil {
		return err
	}
	defer done()

	ctx, stop := signalContext()
	defer stop()

	if hosts == nil {
		hosts = allTriples()
	}
	if targets == nil {
		targets = allTriples()
	}

	v := &verifier{e: e, ctx: ctx}
	if *native {
		v.native()
	}
	if *cross {
		for _, t := range targets {
			v.cross(t)
		}
	}
	for _, h := range hosts {
		for _, t := range targets {
			v.canadian(h, t)
		}
	}
	if v.ran == 0 {
		if !*native {
			v.native()
		}
		if v.ran == 0 {
			fmt.Fprintf(os.Stderr, "nothing to verify: no toolchains built yet. Run %s first.\n",
				cyan("gccf build --host proven --target proven"))
			return nil
		}
	}
	return v.finish()
}

// verifyMatrix is what `build --verify` calls.
func verifyMatrix(ctx context.Context, e *core.Env, hosts, targets []triple.Triple) error {
	v := &verifier{e: e, ctx: ctx}
	if len(hosts) == 0 || len(targets) == 0 {
		for _, t := range append(append([]triple.Triple{}, hosts...), targets...) {
			v.cross(t)
		}
	}
	for _, h := range hosts {
		for _, t := range targets {
			v.canadian(h, t)
		}
	}
	return v.finish()
}

type verifier struct {
	e      *core.Env
	ctx    context.Context
	ran    int
	failed int
}

func (v *verifier) report(r *ensure.Report) {
	v.ran++
	fmt.Println(r.String())
	if !r.OK() {
		v.failed++
	}
}

// Everything a check does is logged under dist/logs/jobs/verify_<slug>/.
func (v *verifier) with(slug string, body func(r *core.Runner, work string) *ensure.Report) {
	slug = "verify_" + slug
	r, err := newRunner(v.e, slug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s cannot open log for %s: %v\n", red("error:"), slug, err)
		v.failed++
		return
	}
	defer r.Close()
	work, err := scratchDir(v.e, slug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s cannot create a probe workspace: %v\n", red("error:"), err)
		v.failed++
		return
	}
	defer os.RemoveAll(work)
	v.report(body(r, work))
}

func (v *verifier) native() {
	fmt.Printf("%s the BUILD machine's own compiler\n", bold("verify native:"))
	v.with("native", func(r *core.Runner, work string) *ensure.Report {
		return checkNative(v.ctx, r, work, "cc", "c++")
	})
}

func (v *verifier) cross(t triple.Triple) {
	j := recipe.Cross(t)
	prefix := j.ArtifactDir(v.e)
	if !v.exists(prefix) {
		return
	}
	em, err := emulatorFor(v.e, t)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", red("error:"), err)
		v.failed++
		return
	}
	fmt.Printf("%s %s\n", bold("verify cross:"), t.Raw)
	v.with(j.Slug(), func(r *core.Runner, work string) *ensure.Report {
		return checkCross(v.ctx, r, work, prefix, t, em)
	})
}

func (v *verifier) canadian(h, t triple.Triple) {
	j := recipe.Canadian(h, t)
	prefix := j.ArtifactDir(v.e)
	if !v.exists(prefix) {
		return
	}
	emHost, err := emulatorFor(v.e, h)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", red("error:"), err)
		v.failed++
		return
	}
	emTarget, err := emulatorFor(v.e, t)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", red("error:"), err)
		v.failed++
		return
	}
	fmt.Printf("%s host=%s target=%s\n", bold("verify canadian:"), h.Raw, t.Raw)
	v.with(j.Slug(), func(r *core.Runner, work string) *ensure.Report {
		return checkCanadian(v.ctx, r, work, prefix, h, t, emHost, emTarget)
	})
}

func (v *verifier) exists(prefix string) bool {
	_, ok := artifactManifest(prefix)
	return ok
}

func (v *verifier) finish() error {
	if v.ran == 0 {
		return nil
	}
	if v.failed > 0 {
		return fmt.Errorf("%d of %d toolchains failed verification", v.failed, v.ran)
	}
	fmt.Printf("\n%s all %d toolchain%s pass\n", green("PASS"), v.ran, plural(v.ran))
	return nil
}
