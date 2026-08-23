package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/core"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// Measured on the 8-core / 7.8 GB builder container: a cross: job peaks at
// 4.0 GB and a canadian: job at ~3.0 GB, so a second concurrent gcc build
// swaps. One job at -j6 is the fastest setting that fits.
const (
	defaultWorkers = 1
	defaultJobs    = 6
)

var cmdBuild = &command{
	Name:     "build",
	Short:    "build toolchains for a host x target matrix",
	Synopsis: "gccfactory build [--host LIST] [--target LIST] [-j N] [--workers N] [--dry-run] [--verify] [--keep-work]",
	Long: `Builds every job the requested matrix needs and nothing else. Jobs are
content-addressed, so re-running after a successful build does no work, and
changing one configure flag rebuilds only what depends on it. Interrupting with
Ctrl-C is safe: partial output never becomes an artifact.

MATRIX
  --host and --target both given   build the canadian toolchains HOST x TARGET
                                   into dist/toolchains/out/<HOST>/<TARGET>/
  --target only                    build just the build->TARGET cross toolchains
  --host only                      build the pieces needed to host a toolchain
                                   on HOST (its cross toolchain + static make)
  neither, on a terminal           opens a two-column picker
  neither, no terminal             errors instead of hanging

  Both flags take a comma list, ` + "`all`" + `, or ` + "`proven`" + `.

FLAGS
  --host LIST      triples the finished compiler must RUN on
  --target LIST    triples the finished compiler must EMIT code for
  -j N             make parallelism inside one job (default 6)
  --workers N      jobs built concurrently (default 1)
                   Defaults are measured, not guessed, on the 8-core / 7.8 GB
                   builder container: a cross: job peaks at 4.0 GB (during
                   all-target-libstdc++-v3) and a canadian: job at ~3.0 GB, so
                   two concurrent gcc builds would swap. One job at -j6 is the
                   fastest safe setting; a cross: toolchain takes ~8 min.
                   With more RAM, raise --workers before -j.
  --dry-run        print the plan (slug, key, state) and exit; touches nothing
  --verify         after building, run the full ensure suite on each toolchain
  --keep-work      keep dist/work/<slug>.* on success so ` + "`gccfactory shell`" + `
                   can drop you into a finished build tree

WHEN IT FAILS
  The failing command, its exit code, the last lines of its output and the full
  log path are printed, followed by the two commands worth running next:
  ` + "`gccfactory logs <slug> --failed`" + ` and ` + "`gccfactory shell <slug>`" + `.`,
	Run: runBuild,
}

func runBuild(g *Global, args []string) error {
	fs := g.flagSet("build")
	host := fs.String("host", "", tripleFlagHelp+" the compiler runs on")
	target := fs.String("target", "", tripleFlagHelp+" the compiler emits code for")
	jobs := fs.Int("j", envInt("GCCF_JOBS", defaultJobs), "make parallelism within one job")
	workers := fs.Int("workers", envInt("GCCF_WORKERS", defaultWorkers), "jobs built concurrently")
	dryRun := fs.Bool("dry-run", false, "print the plan and exit")
	doVerify := fs.Bool("verify", false, "run the ensure suite on everything built")
	keepWork := fs.Bool("keep-work", false, "keep build trees on success")
	if err := parse(fs, args); err != nil {
		return finish("build", err)
	}
	if fs.NArg() > 0 {
		return usagef("unexpected argument %q (did you mean --target %s?)", fs.Arg(0), fs.Arg(0))
	}
	if *jobs < 1 || *workers < 1 {
		return usagef("-j and --workers must be >= 1")
	}
	if err := g.resolve(); err != nil {
		return err
	}

	hosts, targets, err := resolveMatrix("build", *host, *target)
	if err != nil {
		return err
	}

	e, done, err := g.env(*jobs, *workers)
	if err != nil {
		return err
	}
	defer done()
	setKeepWork(e, *keepWork)

	roots := rootJobs(hosts, targets)
	if len(roots) == 0 {
		return usagef("nothing selected")
	}

	if *dryRun {
		return printPlan(e, roots)
	}

	fmt.Fprintf(os.Stderr, "%s %s  (workers=%d, -j%d)\n",
		bold("building"), describeMatrix(hosts, targets), *workers, *jobs)
	fmt.Fprintf(os.Stderr, "%s\n\n", dim("logs: "+filepath.Join(e.Dist, "logs", "jobs")))

	ctx, stop := signalContext()
	defer stop()

	if err := runWithProgress(ctx, e, roots, g.Verbose); err != nil {
		return buildFailure(err)
	}

	fmt.Fprintf(os.Stderr, "\n%s %s\n", green("done."), outputHint(e, hosts, targets))
	if *doVerify {
		fmt.Fprintln(os.Stderr)
		return verifyMatrix(ctx, e, hosts, targets)
	}
	return nil
}

// buildFailure turns whatever core.Run returned into the most actionable
// message we can produce, including the exact follow-up commands.
func buildFailure(err error) error {
	var ce *core.CmdError
	if !errors.As(err, &ce) {
		return err
	}
	slug := slugFromLogPath(ce.LogPath)
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s\n", ce.Error())
	fmt.Fprintf(&b, "\n%s\n", bold("next steps"))
	if slug != "" {
		fmt.Fprintf(&b, "  %s   the failing step's log, from the top\n", cyan("gccf logs "+slug+" --failed"))
		fmt.Fprintf(&b, "  %s        a shell with that job's env and build dir\n", cyan("gccf shell "+slug))
	}
	if ce.LogPath != "" {
		fmt.Fprintf(&b, "  %s\n", dim("full log: "+ce.LogPath))
	}
	fmt.Fprint(os.Stderr, b.String())
	return fmt.Errorf("build failed")
}

// slugFromLogPath recovers <slug> from dist/logs/jobs/<slug>/[<attempt>/]<n>-<step>.log
func slugFromLogPath(p string) string {
	parts := strings.Split(filepath.ToSlash(p), "/")
	for i, s := range parts {
		if s == "jobs" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func printPlan(e *core.Env, roots []core.Job) error {
	nodes, err := core.Plan(e, roots)
	if err != nil {
		return err
	}
	t := newTable("STATE", "SLUG", "KEY", "ARTIFACT")
	todo := 0
	for _, n := range nodes {
		state := yellow("missing")
		if n.Valid {
			state = green("ok")
		} else if _, ok := artifactManifest(n.Job.ArtifactDir(e)); ok {
			state = yellow("stale")
		}
		if n.Building {
			state = blue("building")
		}
		if !n.Valid {
			todo++
		}
		t.add(state, n.Job.Slug(), dim(shortKey(n.Key)), dim(rel(e.Dist, n.Job.ArtifactDir(e))))
	}
	t.render(os.Stdout)
	fmt.Printf("\n%d job%s, %d to build\n", len(nodes), plural(len(nodes)), todo)
	return nil
}

func shortKey(k string) string {
	if len(k) > 12 {
		return k[:12]
	}
	return k
}

func rel(base, p string) string {
	if r, err := filepath.Rel(filepath.Dir(base), p); err == nil {
		return r
	}
	return p
}

func outputHint(e *core.Env, hosts, targets []triple.Triple) string {
	if len(hosts) == 0 || len(targets) == 0 {
		return "intermediates are in " + rel(e.Dist, filepath.Join(e.Dist, "toolchains"))
	}
	return "toolchains are in " + rel(e.Dist, filepath.Join(e.Dist, "toolchains", "out"))
}
