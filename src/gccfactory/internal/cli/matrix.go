package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/core"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/recipe"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

const tripleFlagHelp = "comma list of triples, or `all` / `proven`"

func parseTriples(flagName, value string) ([]triple.Triple, error) {
	role := triple.RoleAny
	switch flagName {
	case "host":
		role = triple.RoleHost
	case "target":
		role = triple.RoleTarget
	}
	ts, err := triple.ParseListFor(role, value)
	if err != nil {
		return nil, fmt.Errorf("--%s: %w\n\naccepted values: all, proven, or a comma list of those", flagName, err)
	}
	if len(ts) == 0 {
		return nil, fmt.Errorf("--%s: no triples given", flagName)
	}
	return ts, nil
}

// noSelectionErr names the exact flags rather than just complaining.
func noSelectionErr(cmd string) error {
	return fmt.Errorf(`no --host/--target given and stdin is not a terminal, so there is nothing to pick from.

Pass them explicitly, e.g.:
  gccfactory %s --host x86_64-linux-musl --target aarch64-linux-musl
  gccfactory %s --host proven --target proven
  gccfactory %s --host all --target all

Both flags accept a comma list, `+"`all`"+` (all %d supported triples) or `+"`proven`"+`.
  --host proven   = %s
  --target proven = %s`,
		cmd, cmd, cmd, len(triple.Known),
		strings.Join(triple.ProvenHosts, ", "), strings.Join(triple.ProvenTargets, ", "))
}

// resolveMatrix turns the --host/--target flags into two lists, falling back to
// the interactive picker when both are empty and we have a terminal.
func resolveMatrix(cmd, hostFlag, targetFlag string) (hosts, targets []triple.Triple, err error) {
	if hostFlag == "" && targetFlag == "" {
		if !isTTY(os.Stdin) {
			return nil, nil, noSelectionErr(cmd)
		}
		return pickMatrix(cmd)
	}
	if hostFlag != "" {
		if hosts, err = parseTriples("host", hostFlag); err != nil {
			return nil, nil, err
		}
	}
	if targetFlag != "" {
		if targets, err = parseTriples("target", targetFlag); err != nil {
			return nil, nil, err
		}
	}
	return hosts, targets, nil
}

// rootJobs turns a matrix into the DAG roots to build.
//
//	--host H --target T  ->  the canadian toolchains H x T (the deliverable)
//	--target T only      ->  just the build->T cross toolchains
//	--host H only        ->  everything needed to host a toolchain on H
//
// The one-sided forms exist because those intermediates are useful on their
// own and are by far the cheapest way to bisect a failure.
func rootJobs(hosts, targets []triple.Triple) []core.Job {
	var jobs []core.Job
	switch {
	case len(hosts) > 0 && len(targets) > 0:
		for _, h := range hosts {
			for _, t := range targets {
				jobs = append(jobs, recipe.Canadian(h, t))
			}
		}
	case len(targets) > 0:
		for _, t := range targets {
			jobs = append(jobs, recipe.Cross(t))
		}
	case len(hosts) > 0:
		for _, h := range hosts {
			jobs = append(jobs, recipe.Cross(h), recipe.HostMake(h))
		}
	}
	return jobs
}

func describeMatrix(hosts, targets []triple.Triple) string {
	switch {
	case len(hosts) > 0 && len(targets) > 0:
		return fmt.Sprintf("%d host%s x %d target%s = %d toolchain%s",
			len(hosts), plural(len(hosts)), len(targets), plural(len(targets)),
			len(hosts)*len(targets), plural(len(hosts)*len(targets)))
	case len(targets) > 0:
		return fmt.Sprintf("%d build->target cross toolchain%s", len(targets), plural(len(targets)))
	case len(hosts) > 0:
		return fmt.Sprintf("host support for %d triple%s", len(hosts), plural(len(hosts)))
	}
	return "nothing"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func names(ts []triple.Triple) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Raw
	}
	sort.Strings(out)
	return out
}

func allTriples() []triple.Triple {
	ts, err := triple.ParseList("all")
	if err != nil {
		panic(err) // triple.Known must parse
	}
	return ts
}

// mergeEnv applies an overlay to a base environment (or to os.Environ when the
// base is nil), matching core.Cmd's Env/EnvAdd semantics.
func mergeEnv(base []string, add map[string]string) []string {
	env := base
	if env == nil {
		env = os.Environ()
	}
	if len(add) == 0 {
		return env
	}
	out := make([]string, 0, len(env)+len(add))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if _, override := add[k]; !override {
			out = append(out, kv)
		}
	}
	for _, k := range sortedKeys(add) {
		out = append(out, k+"="+add[k])
	}
	return out
}
