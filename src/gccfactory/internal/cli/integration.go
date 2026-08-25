package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/core"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/ensure"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/logging"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// Without -v the human stream is silenced because the live progress view owns
// the terminal; run.jsonl still records everything.
func newLogger(dist string, verbose bool) (*logging.Logger, error) {
	o := logging.Options{
		RunsRoot: filepath.Join(dist, "logs", "runs"),
		Stderr:   io.Discard,
		Level:    logging.LevelInfo,
		Color:    &colorOn,
	}
	if verbose {
		o.Stderr = os.Stderr
		o.Level = logging.LevelDebug
	}
	return logging.New(o)
}

func closeLogger(l *logging.Logger) {
	if l != nil {
		_ = l.Close()
	}
}

// setKeepWork asks the builder to preserve dist/work/<slug>.* after a
// successful job so `gccfactory shell` can drop you into the tree.
func setKeepWork(e *core.Env, keep bool) {
	if keep {
		_ = os.Setenv("GCCFACTORY_KEEP_WORK", "1")
	}
}

// `verify` uses it so its probe commands are logged exactly like build steps.
// Close it when done.
func newRunner(e *core.Env, slug string) (*core.Runner, error) {
	return core.NewRunner(e, slug)
}

// scratchDir makes a probe workspace named like every other scratch dir
// (<slug>.<pid>.<rand>) so `gccfactory clean` knows how to reap it.
func scratchDir(e *core.Env, slug string) (string, error) {
	root := e.Path(core.DirWork)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	return os.MkdirTemp(root, fmt.Sprintf("%s.%d.", slug, os.Getpid()))
}

// internal/ensure does not import internal/core; it declares its own minimal
// Cmd/Runner interface. ensureRunner adapts a *core.Runner to it so every probe
// compile and qemu run is recorded in the same per-job log tree as a build,
// with the same replayable commands.sh.

type ensureRunner struct{ r *core.Runner }

var _ ensure.Runner = ensureRunner{}

func (a ensureRunner) toCore(c ensure.Cmd) core.Cmd {
	return core.Cmd{Dir: c.Dir, EnvAdd: c.EnvAdd, Args: c.Args, Name: c.Name}
}

func (a ensureRunner) Run(ctx context.Context, c ensure.Cmd) error {
	return a.r.Run(ctx, a.toCore(c))
}

func (a ensureRunner) Output(ctx context.Context, c ensure.Cmd) ([]byte, error) {
	out, err := a.r.Output(ctx, a.toCore(c))
	return []byte(out), err
}

func checkNative(ctx context.Context, r *core.Runner, work, cc, cxx string) *ensure.Report {
	return ensure.NativeToolchain(ctx, ensureRunner{r}, work, cc, cxx)
}

func checkCross(ctx context.Context, r *core.Runner, work, prefix string, t triple.Triple, em ensure.Emulator) *ensure.Report {
	return ensure.CrossToolchain(ctx, ensureRunner{r}, work, prefix, t, em)
}

func checkCanadian(ctx context.Context, r *core.Runner, work, prefix string, h, t triple.Triple, emHost, emTarget ensure.Emulator) *ensure.Report {
	return ensure.CanadianToolchain(ctx, ensureRunner{r}, work, prefix, h, t, emHost, emTarget)
}

// emulatorFor builds the Emulator that can run t: an --exec-wrapper if one is
// configured (expanded straight through EmulatorSpec, which never searches),
// else qemu-user resolved via qemuPath.
func emulatorFor(e *core.Env, t triple.Triple) (ensure.Emulator, error) {
	if len(e.ExecWrapper) > 0 {
		return ensure.EmulatorSpec{Wrapper: e.ExecWrapper, Dist: e.Dist}.For(t)
	}
	return ensure.QemuEmulator(qemuPath(e.QemuTemplate, t)), nil
}

// --qemu-dir may be given either as a directory (/usr/bin) or as a path
// template containing one %s (/opt/qemu/bin/qemu-%s), so a non-Debian layout
// needs no code change. When neither names an existing binary we hand ensure
// the best guess, which then reports a precise "tried ..." failure.
func qemuPath(dirOrTemplate string, t triple.Triple) string {
	if strings.Contains(dirOrTemplate, "%s") {
		return fmt.Sprintf(dirOrTemplate, t.QemuName())
	}
	if p, err := ensure.QemuFor(t, []string{dirOrTemplate}); err == nil {
		return p
	}
	return filepath.Join(dirOrTemplate, "qemu-"+t.QemuName()+"-static")
}

// qemuTemplate is what we store in core.Env.QemuTemplate: a printf template
// with a single %s for triple.QemuName().
func qemuTemplate(dirOrTemplate string) string {
	if strings.Contains(dirOrTemplate, "%s") {
		return dirOrTemplate
	}
	return filepath.Join(dirOrTemplate, "qemu-%s-static")
}
