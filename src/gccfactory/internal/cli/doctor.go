package cli

import (
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/ensure"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

var cmdDoctor = &command{
	Name:     "doctor",
	Short:    "can this machine execute the binaries a build will produce?",
	Synopsis: "gccfactory doctor [--host LIST] [--target LIST]",
	Long: `Answers the one question that decides whether a build can be verified at all:
for each architecture in the matrix, how does this machine execute a binary
built for it?

Run it before a long build. Every failure it reports is a failure you would
otherwise hit hours later, at the verification step.

  ROUTES
    ` + "`native`" + `        the kernel runs these directly
    ` + "`binfmt_misc`" + `   the kernel routes them to a registered interpreter
    ` + "`qemu only`" + `     a qemu-user binary exists, but the kernel has no registration
    ` + "`none`" + `          nothing here can execute them

WHY qemu ALONE IS NOT ENOUGH FOR A HOST
  gcc is not one process. The driver forks cc1, as and collect2, and a
  qemu-user launcher only ever covers the process it was handed. Nesting works
  only when the kernel itself routes a foreign exec, which means binfmt_misc.
  So a HOST architecture needs ` + "`native`" + ` or ` + "`binfmt_misc`" + `, while a TARGET
  architecture -- whose binaries are only ever run as leaves -- is fine under
  ` + "`qemu only`" + `.

This reads /proc/sys/fs/binfmt_misc and looks for qemu binaries; it does not
need dist/ and builds nothing.`,
	Run: runDoctor,
}

// role is what a triple has to be able to do on this machine, which is what
// decides whether "qemu only" counts as working for it.
type role struct{ host, target bool }

func (r role) needsFork() bool { return r.host }

func (r role) String() string {
	switch {
	case r.host && r.target:
		return "host+target"
	case r.host:
		return "host"
	}
	return "target"
}

func runDoctor(g *Global, args []string) error {
	fs := g.flagSet("doctor")
	host := fs.String("host", "proven", tripleFlagHelp)
	target := fs.String("target", "proven", tripleFlagHelp)
	if err := parse(fs, args); err != nil {
		return finish("doctor", err)
	}
	hosts, err := parseTriples("host", *host)
	if err != nil {
		return err
	}
	targets, err := parseTriples("target", *target)
	if err != nil {
		return err
	}

	roles := map[string]role{}
	for _, h := range hosts {
		r := roles[h.Raw]
		r.host = true
		roles[h.Raw] = r
	}
	for _, t := range targets {
		r := roles[t.Raw]
		r.target = true
		roles[t.Raw] = r
	}
	names := make([]string, 0, len(roles))
	for n := range roles {
		names = append(names, n)
	}
	sort.Strings(names)

	fmt.Printf("%s %s\n\n", bold("exec routes:"), dim("how this machine runs binaries for each architecture"))
	tbl := newTable("ARCH", "ROLE", "ROUTE")
	var broken []string
	for _, n := range names {
		t, err := triple.Parse(n)
		if err != nil {
			return finish("doctor", err)
		}
		r := roles[n]
		route, detail := ensure.ExecRouteOf(t, []string{qemuPath(g.QemuDir, t)})
		var label string
		switch {
		case route == ensure.RouteNone:
			label, broken = red(route.String()), append(broken, n)
		case r.needsFork() && !route.Nested():
			label, broken = yellow(route.String()), append(broken, n)
			detail += "; a host must fork cc1/as/ld, which qemu alone cannot do"
		default:
			label = green(route.String())
		}
		tbl.add(n, dim(r.String()), label+dim("  "+detail))
	}
	tbl.render(os.Stdout)
	fmt.Println()

	if len(broken) == 0 {
		fmt.Printf("%s all %d architectures can be executed here\n", green("ok"), len(names))
		return nil
	}
	return finish("doctor", fmt.Errorf("%d of %d architectures cannot be executed here: %s\n\n%s",
		len(broken), len(names), strings.Join(broken, ", "), ensure.BinfmtRemedy(broken)))
}

// warnUnroutable says up front that some of this matrix will not be verifiable
// here. It only warns: building without being able to run the result is a
// legitimate thing to want (build here, verify elsewhere), and a build that is
// already hours long should not be refused over it.
func warnUnroutable(w io.Writer, g *Global, hosts, targets []triple.Triple) {
	var bad []string
	for _, t := range append(append([]triple.Triple{}, hosts...), targets...) {
		route, _ := ensure.ExecRouteOf(t, []string{qemuPath(g.QemuDir, t)})
		needsFork := false
		for _, h := range hosts {
			if h.Raw == t.Raw {
				needsFork = true
			}
		}
		if route == ensure.RouteNone || (needsFork && !route.Nested()) {
			if !slices.Contains(bad, t.Raw) {
				bad = append(bad, t.Raw)
			}
		}
	}
	if len(bad) == 0 {
		return
	}
	fmt.Fprintf(w, "%s cannot execute %s here, so those toolchains will build but not verify.\n",
		yellow("warning:"), strings.Join(bad, ", "))
	fmt.Fprintf(w, "%s\n\n", dim("  run `gccfactory doctor` for the details and the fix"))
}
