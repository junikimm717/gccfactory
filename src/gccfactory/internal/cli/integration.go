package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
	return core.Cmd{Dir: c.Dir, Env: c.Env, EnvAdd: c.EnvAdd, Args: c.Args, Name: c.Name}
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

func checkCross(ctx context.Context, r *core.Runner, work, prefix string, t triple.Triple, qemu string) *ensure.Report {
	return ensure.CrossToolchain(ctx, ensureRunner{r}, work, prefix, t, qemu)
}

func checkCanadian(ctx context.Context, r *core.Runner, work, prefix string, h, t triple.Triple, qemuHost, qemuTarget string) *ensure.Report {
	return ensure.CanadianToolchain(ctx, ensureRunner{r}, work, prefix, h, t, qemuHost, qemuTarget)
}

// Directory form always names qemu-<arch>-static. qemu-user (no -static) plus
// binfmt_misc will exec a dynamic target binary against the host's
// /lib/ld-musl-*.so.1, which is how a "working" doctor used to lie.
func qemuPath(dirOrTemplate string, t triple.Triple) string {
	if strings.Contains(dirOrTemplate, "%s") {
		return fmt.Sprintf(dirOrTemplate, t.QemuName())
	}
	return filepath.Join(dirOrTemplate, "qemu-"+t.QemuName()+"-static")
}

func missingStaticQemu(dirOrTemplate string, ts []triple.Triple) []string {
	seen := map[string]bool{}
	var missing []string
	for _, t := range ts {
		p := qemuPath(dirOrTemplate, t)
		if seen[p] {
			continue
		}
		seen[p] = true
		st, err := os.Stat(p)
		if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	return missing
}

func staticQemuErr(missing []string) error {
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("missing qemu-user-static binaries:\n  %s\ninstall qemu-user-static (Debian/Ubuntu: apt install qemu-user-static).\nqemu-user + binfmt_misc is not enough: verify runs qemu-<arch>-static -L <sysroot>",
		strings.Join(missing, "\n  "))
}

// qemuTemplate is what we store in core.Env.QemuHost/QemuTarget: a printf
// template with a single %s for triple.QemuName().
func qemuTemplate(dirOrTemplate string) string {
	if strings.Contains(dirOrTemplate, "%s") {
		return dirOrTemplate
	}
	return filepath.Join(dirOrTemplate, "qemu-%s-static")
}
