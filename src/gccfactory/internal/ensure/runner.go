// Package ensure proves that a toolchain works. It inspects binaries (ELF
// identity, link mode, interpreter), asserts the tool surface matches
// musl-cross-make, and compiles + runs a suite of probe programs whose exact
// stdout is known in advance.
//
// The package deliberately depends on nothing but the standard library and
// internal/triple. Command execution is injected through the Runner interface
// below so that internal/core can own logging without ensure importing it.
package ensure

import (
	"context"
	"regexp"
	"strings"
)

// Cmd mirrors core.Cmd; the caller's adapter translates between them.
type Cmd struct {
	// Name is the step name, used for the log file (e.g. "probe-hello-O2").
	Name string
	// Always set by ensure.
	Dir string
	Args []string
	// EnvAdd is overlaid on the inherited environment.
	EnvAdd map[string]string
}

// Runner executes commands on the BUILD machine and logs them.
//
// It is implemented by a thin adapter around *core.Runner (see the package
// docs of internal/cli). Run is used for commands whose failure is a real
// build failure; Output is used for probes, where a non-zero exit is an
// expected outcome that ensure turns into a failed Check rather than a fatal
// error, so it must not poison the job log.
type Runner interface {
	Run(ctx context.Context, c Cmd) error
	Output(ctx context.Context, c Cmd) ([]byte, error)
}

// LogPather is an optional interface on errors that know where their full log
// lives (*core.CmdError should satisfy it, or carry "full log: <path>" in its
// message, which ensure also understands).
type LogPather interface {
	LogPath() string
}

var logPathRE = regexp.MustCompile(`full log: (\S+)`)

// logPathOf digs the log file path out of an error, so a failed Check can
// point at it without the caller threading it through.
func logPathOf(err error) string {
	if err == nil {
		return ""
	}
	var lp LogPather
	if asLogPather(err, &lp) {
		return lp.LogPath()
	}
	if m := logPathRE.FindStringSubmatch(err.Error()); m != nil {
		return strings.TrimRight(m[1], ".,;")
	}
	return ""
}

func asLogPather(err error, out *LogPather) bool {
	for err != nil {
		if lp, ok := err.(LogPather); ok {
			*out = lp
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
