package cli

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/core"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/ensure"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/recipe"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

var cmdVerify = &command{
	Name:     "verify",
	Short:    "prove built toolchains actually work (compile, then run what they emit)",
	Synopsis: "gccfactory verify [--host LIST] [--target LIST] [--native] [--cross] [--workers N]",
	Long: `Runs the ensure suite. This is a real proof, not a smoke test:

  * every binary in <prefix>/bin is checked to be an ELF for the HOST arch
  * the musl-cross-make tool surface (gcc, g++, ar, ld, strip, ..., make) must
    all be present and executable
  * the compilers are executed -- natively, via binfmt_misc, or under
    qemu-<host>, whichever this machine actually supports -- to compile a probe
    suite for TARGET (printf, libm, pthreads, TLS, atomics, C++ iostreams,
    exceptions, std::regex/thread, dlopen of a -fPIC shared object, and
    -static), at -O0 and -O2, and each resulting binary is ELF-checked for
    TARGET and then run

` + "`gccfactory doctor`" + ` reports how each architecture gets executed here,
and is worth running before a long build rather than after it.

With no --host/--target, everything currently built in dist/ is verified, so
` + "`gccfactory verify`" + ` on its own is always a valid thing to type.

FLAGS
  --host LIST     restrict to these host triples
  --target LIST   restrict to these target triples
  --native        also verify the BUILD machine's own gcc/g++ (implied when no
                  toolchains are built yet)
  --cross         also verify the intermediate build->target cross toolchains
  --workers N     toolchains verified concurrently (default: cores, capped at 4)

                  Toolchains share nothing here, so this scales close to
                  linearly. One verification is a single qemu-emulated compile
                  at a time -- about one core and a few hundred MB, far cheaper
                  than a build worker -- so it can be raised toward the core
                  count.

                  Reports are printed in matrix order no matter which
                  verification finishes first, so --workers changes only how
                  long the run takes, never what it prints.

Exit status is non-zero if any check fails. Each check's log path is printed.`,
	Run: runVerify,
}

// One verification is a single emulated compile at a time, so unlike a gcc
// bootstrap it is memory-cheap; the cap only keeps a shared box usable.
func defaultVerifyWorkers() int {
	if n := runtime.NumCPU(); n < 4 {
		return n
	}
	return 4
}

func runVerify(g *Global, args []string) error {
	fs := g.flagSet("verify")
	host := fs.String("host", "", tripleFlagHelp)
	target := fs.String("target", "", tripleFlagHelp)
	native := fs.Bool("native", false, "also verify the BUILD machine's gcc/g++")
	cross := fs.Bool("cross", false, "also verify the build->target cross toolchains")
	workers := fs.Int("workers", envInt("GCCF_WORKERS", defaultVerifyWorkers()), "toolchains verified concurrently")
	if err := parse(fs, args); err != nil {
		return finish("verify", err)
	}
	if *workers < 1 {
		return usagef("--workers must be >= 1")
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

	e, done, err := g.env(defaultJobs, *workers)
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
	if len(v.tasks) == 0 {
		if !*native {
			v.native()
		}
		if len(v.tasks) == 0 {
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

type verifyTask struct {
	header string
	slug   string
	body   func(r *core.Runner, work string) *ensure.Report
}

type verifyResult struct {
	report  *ensure.Report
	errText string // the harness could not run at all, so there is no report
	skipped bool   // cancelled before it started
}

type verifier struct {
	e     *core.Env
	ctx   context.Context
	tasks []verifyTask
	exec  func(verifyTask) verifyResult // tests substitute the work; nil means the real suite
}

func (v *verifier) add(header, slug string, body func(r *core.Runner, work string) *ensure.Report) {
	v.tasks = append(v.tasks, verifyTask{header: header, slug: "verify_" + slug, body: body})
}

// Everything a check does is logged under dist/logs/jobs/verify_<slug>/, and
// probes are compiled in a scratch dir of their own, so tasks never collide.
func (v *verifier) run(t verifyTask) verifyResult {
	if v.exec != nil {
		return v.exec(t)
	}
	r, err := newRunner(v.e, t.slug)
	if err != nil {
		return verifyResult{errText: fmt.Sprintf("%s cannot open log for %s: %v", red("error:"), t.slug, err)}
	}
	defer r.Close()
	work, err := scratchDir(v.e, t.slug)
	if err != nil {
		return verifyResult{errText: fmt.Sprintf("%s cannot create a probe workspace: %v", red("error:"), err)}
	}
	defer os.RemoveAll(work)
	return verifyResult{report: t.body(r, work)}
}

func (v *verifier) native() {
	v.add(fmt.Sprintf("%s the BUILD machine's own compiler", bold("verify native:")), "native",
		func(r *core.Runner, work string) *ensure.Report {
			return checkNative(v.ctx, r, work, "cc", "c++")
		})
}

func (v *verifier) cross(t triple.Triple) {
	j := recipe.Cross(t)
	prefix := j.ArtifactDir(v.e)
	if !v.exists(prefix) {
		return
	}
	v.add(fmt.Sprintf("%s %s", bold("verify cross:"), t.Raw), j.Slug(),
		func(r *core.Runner, work string) *ensure.Report {
			return checkCross(v.ctx, r, work, prefix, t, qemuPath(v.qemuDir(), t))
		})
}

func (v *verifier) canadian(h, t triple.Triple) {
	j := recipe.Canadian(h, t)
	prefix := j.ArtifactDir(v.e)
	if !v.exists(prefix) {
		return
	}
	v.add(fmt.Sprintf("%s host=%s target=%s", bold("verify canadian:"), h.Raw, t.Raw), j.Slug(),
		func(r *core.Runner, work string) *ensure.Report {
			return checkCanadian(v.ctx, r, work, prefix, h, t, qemuPath(v.qemuDir(), h), qemuPath(v.qemuDir(), t))
		})
}

func (v *verifier) qemuDir() string { return v.e.QemuHost }

func (v *verifier) exists(prefix string) bool {
	_, ok := artifactManifest(prefix)
	return ok
}

func (v *verifier) workers() int {
	if v.e == nil {
		return 1
	}
	return v.e.Workers()
}

// Reports print in matrix order however the work finishes, so --workers never
// changes the output; each is flushed the moment its turn comes up, so a
// serial run still streams as it goes.
func (v *verifier) finish() error {
	if len(v.tasks) == 0 {
		return nil
	}
	ctx := v.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	results := make([]verifyResult, len(v.tasks))
	done := make([]chan struct{}, len(v.tasks))
	for i := range done {
		done[i] = make(chan struct{})
	}
	sem := make(chan struct{}, v.workers())

	var wg sync.WaitGroup
	for i := range v.tasks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer close(done[i])
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = verifyResult{skipped: true}
				return
			}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				results[i] = verifyResult{skipped: true}
				return
			}
			results[i] = v.run(v.tasks[i])
		}(i)
	}

	ran, failed := 0, 0
	for i := range v.tasks {
		<-done[i]
		switch res := results[i]; {
		case res.skipped:
		case res.errText != "":
			fmt.Fprintln(os.Stderr, res.errText)
			failed++
		default:
			fmt.Println(v.tasks[i].header)
			fmt.Println(res.report.String())
			ran++
			if !res.report.OK() {
				failed++
			}
		}
	}
	wg.Wait()

	if failed > 0 {
		return fmt.Errorf("%d of %d toolchains failed verification", failed, ran)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if ran == 0 {
		return nil
	}
	fmt.Printf("\n%s all %d toolchain%s pass\n", green("PASS"), ran, plural(ran))
	return nil
}
