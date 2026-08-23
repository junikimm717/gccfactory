// Package cli follows the design rule in CLAUDE.md, "Principles for UI":
// `gccfactory help` and
// `gccfactory help <command>` are the documentation. Every flag carries a
// sentence that says what it does AND why the default is what it is. If you
// find yourself wanting to write a doc file, put it in a Long string here.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/core"
)

type Global struct {
	Dist      string // abs path to dist/
	QemuDir   string // dir holding qemu-<arch>-static, or a template with %s
	ColorWhen string
	Verbose   bool

	repoRoot string
}

type command struct {
	Name     string
	Short    string // one line, shown in `help`
	Synopsis string // usage line
	Long     string // shown by `help <command>`
	Run      func(g *Global, args []string) error
}

// registry is populated in init rather than as a var initializer: the commands
// reference help rendering, which reads the registry, and Go rejects that as an
// initialization cycle.
var registry []*command

func init() {
	registry = []*command{cmdBuild, cmdStatus, cmdVerify, cmdLogs, cmdShell, cmdClean, cmdSources}
}

func commands() []*command { return registry }

func lookup(name string) *command {
	for _, c := range commands() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func Main(args []string) int {
	g := &Global{
		Dist:      envOr("GCCF_DIST", ""),
		QemuDir:   envOr("GCCF_QEMU_DIR", "/usr/bin"),
		ColorWhen: envOr("GCCF_COLOR", "auto"),
	}
	if len(args) == 0 {
		printHelp(os.Stdout, "")
		return 0
	}

	// Global flags are accepted before the subcommand as well as after it, so
	// `gccfactory -v build` and `gccfactory build -v` both work.
	pre := flag.NewFlagSet("gccfactory", flag.ContinueOnError)
	pre.SetOutput(io.Discard)
	g.register(pre)
	if err := pre.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHelp(os.Stdout, "")
			return 0
		}
		fmt.Fprintf(os.Stderr, "gccfactory: %v\n\nRun `gccfactory help` for usage.\n", err)
		return 2
	}
	rest := pre.Args()
	if len(rest) == 0 {
		printHelp(os.Stdout, "")
		return 0
	}

	name, rest := rest[0], rest[1:]
	switch name {
	case "help", "-h", "--help":
		topic := ""
		if len(rest) > 0 {
			topic = rest[0]
		}
		if topic != "" && lookup(topic) == nil {
			fmt.Fprintf(os.Stderr, "gccfactory: no such command %q\n", topic)
			printHelp(os.Stderr, "")
			return 2
		}
		printHelp(os.Stdout, topic)
		return 0
	}

	cmd := lookup(name)
	if cmd == nil {
		fmt.Fprintf(os.Stderr, "gccfactory: unknown command %q\n\n", name)
		printHelp(os.Stderr, "")
		return 2
	}

	if err := cmd.Run(g, rest); err != nil {
		if errors.Is(err, errUsage) {
			fmt.Fprintf(os.Stderr, "gccfactory %s: %v\n\n", cmd.Name, err)
			printHelp(os.Stderr, cmd.Name)
			return 2
		}
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, dim("interrupted"))
			return 130
		}
		fmt.Fprintf(os.Stderr, "%s %v\n", red("error:"), err)
		return 1
	}
	return 0
}

// errUsage marks an error that should be followed by the command's help text.
var errUsage = errors.New("usage")

type usageError struct{ msg string }

func (e usageError) Error() string         { return e.msg }
func (e usageError) Is(target error) bool  { return target == errUsage }
func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

// register wires the global flags into fs, defaulting to whatever g already
// holds. That is what lets the same flag be given before or after the
// subcommand: the second FlagSet inherits the first one's result.
func (g *Global) register(fs *flag.FlagSet) {
	fs.StringVar(&g.Dist, "dist", g.Dist,
		"build tree + artifact root (default: <repo>/dist; the shim sets this)")
	fs.StringVar(&g.QemuDir, "qemu-dir", g.QemuDir,
		"directory holding qemu-<arch>-static, or a path template containing %s")
	fs.StringVar(&g.ColorWhen, "color", g.ColorWhen,
		"colorize output: auto|always|never")
	fs.BoolVar(&g.Verbose, "v", g.Verbose, "verbose: stream every command's output to the terminal")
	fs.BoolVar(&g.Verbose, "verbose", g.Verbose, "alias for -v")
}

// flagSet returns a FlagSet that also understands the global flags, so they may
// appear on either side of the subcommand name.
func (g *Global) flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("gccfactory "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	g.register(fs)
	return fs
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return errHelpRequested
		}
		return usagef("%v", err)
	}
	return nil
}

var errHelpRequested = errors.New("help requested")

func finish(name string, err error) error {
	if errors.Is(err, errHelpRequested) {
		printHelp(os.Stdout, name)
		return nil
	}
	return err
}

func (g *Global) resolve() error {
	if g.ColorWhen == "" {
		g.ColorWhen = "auto"
	}
	if g.QemuDir == "" {
		g.QemuDir = "/usr/bin"
	}
	setColor(g.ColorWhen)
	if g.Dist == "" {
		root, err := repoRoot()
		if err != nil {
			return err
		}
		g.Dist = filepath.Join(root, "dist")
	}
	abs, err := filepath.Abs(g.Dist)
	if err != nil {
		return err
	}
	g.Dist = abs
	g.repoRoot = filepath.Dir(abs)
	return nil
}

// The returned func must be deferred; it flushes the run log.
func (g *Global) env(jobs, workers int) (*core.Env, func(), error) {
	if err := g.resolve(); err != nil {
		return nil, nil, err
	}
	log, err := newLogger(g.Dist, g.Verbose)
	if err != nil {
		return nil, nil, fmt.Errorf("open run log under %s: %w", filepath.Join(g.Dist, "logs"), err)
	}
	e := &core.Env{
		Dist:       g.Dist,
		RepoRoot:   g.repoRoot,
		Jobs:       jobs,
		MaxWorkers: workers,
		QemuHost:   qemuTemplate(g.QemuDir),
		QemuTarget: qemuTemplate(g.QemuDir),
		Log:        log,
	}
	if err := e.EnsureDirs(); err != nil {
		closeLogger(log)
		return nil, nil, fmt.Errorf("prepare %s: %w", g.Dist, err)
	}
	return e, func() { closeLogger(log) }, nil
}

func repoRoot() (string, error) {
	var starts []string
	if exe, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	for _, s := range starts {
		for d := s; ; d = filepath.Dir(d) {
			if fi, err := os.Stat(filepath.Join(d, "src", "gccf")); err == nil && !fi.IsDir() {
				return d, nil
			}
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	return "", fmt.Errorf("cannot locate the repo root; pass --dist explicitly (or set GCCF_DIST)")
}

// signalContext cancels on the first SIGINT/SIGTERM and hard-exits on the
// second, so a wedged build is always escapable.
func signalContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		fmt.Fprintln(os.Stderr, "\n"+yellow("interrupt: finishing in-flight steps; press Ctrl-C again to abort now"))
		cancel()
		<-ch
		os.Exit(130)
	}()
	return ctx, func() { signal.Stop(ch); cancel() }
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
