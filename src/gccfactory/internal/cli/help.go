package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

const overview = `gccfactory builds canadian-cross GCC toolchains for linux-musl:
compilers that RUN on a HOST triple and EMIT code for a TARGET triple, built
on whatever machine you are sitting at. Output lands in
dist/toolchains/out/<HOST>/<TARGET>/ and is feature-equivalent to
musl-cross-make (binutils, gcc, g++, musl, libstdc++, plus a static make).

Everything is content-addressed: each job has a key hashed over its sources,
flags, triples and its dependencies' keys. Rebuilding with identical inputs is
a no-op; changing one configure flag rebuilds exactly what depends on it.
Two gccfactory processes may share one dist/ safely.`

func tripleHelp() string {
	var b strings.Builder
	// Every Known triple is now a proven target, so starring them all says
	// nothing; the remaining distinction is which two also work as hosts.
	b.WriteString("Supported triples (all are in `--target proven`):\n")
	for _, t := range triple.Known {
		mark := "  "
		for _, p := range triple.ProvenHosts {
			if p == t {
				mark = " H"
			}
		}
		fmt.Fprintf(&b, "  %s %s\n", mark, t)
	}
	b.WriteString("  (H = also in `--host proven`)\n")
	return b.String()
}

const globalHelp = `Global flags (accepted before or after the command):
  --dist DIR       build tree + artifact root. Default <repo>/dist, or $GCCF_DIST.
                   The ./src/gccf shim always sets this for you.
  --qemu-dir DIR   where qemu-<arch>-static lives (default /usr/bin). May instead
                   be a template containing %s, e.g. /opt/qemu/bin/qemu-%s.
                   Only a fallback: foreign binaries are exec'd directly when
                   the kernel can route them. Run "gccfactory doctor" to see
                   which route each architecture actually takes here.
  --color WHEN     auto|always|never (default auto: color only on a terminal).
  -v, --verbose    mirror every command's output to the terminal as it runs.
                   Without it, output still goes to dist/logs/ in full.`

const layoutHelp = `Where things land under dist/:
  toolchains/out/<HOST>/<TARGET>/   the deliverable
  toolchains/cross/<TARGET>/        build->target toolchain (an intermediate)
  tarballs/<HOST-ARCH>/             distributable .tgz + SHA256SUMS (see ` + "`pack`" + `)
  logs/jobs/<slug>/latest/          every command a job ran, with cwd + env
  logs/runs/<stamp>-<pid>/run.jsonl structured event stream for one invocation
  work/, .staging/, .trash/         scratch; safe to delete (see ` + "`clean`" + `)
  src/                              verified source tarballs (keep these)`

func printHelp(w io.Writer, topic string) {
	if topic == "" {
		fmt.Fprintln(w, bold("gccfactory")+" - canadian-cross GCC toolchain factory")
		fmt.Fprintln(w)
		fmt.Fprintln(w, overview)
		fmt.Fprintln(w)
		fmt.Fprintln(w, bold("USAGE"))
		fmt.Fprintln(w, "  gccfactory [global flags] <command> [flags]")
		fmt.Fprintln(w)
		fmt.Fprintln(w, bold("COMMANDS"))
		for _, c := range commands() {
			fmt.Fprintf(w, "  %-9s %s\n", c.Name, c.Short)
		}
		fmt.Fprintf(w, "  %-9s %s\n", "help", "show this, or `help <command>` for the details")
		fmt.Fprintln(w)
		fmt.Fprintln(w, bold("GETTING STARTED"))
		fmt.Fprintln(w, "  ./src/gccf build                 pick hosts/targets interactively")
		fmt.Fprintln(w, "  ./src/gccf build --host x86_64-linux-musl --target aarch64-linux-musl")
		fmt.Fprintln(w, "  ./src/gccf status                what exists, what's stale, what's running")
		fmt.Fprintln(w, "  ./src/gccf verify                prove the toolchains actually work")
		fmt.Fprintln(w, "  ./src/gccf pack                  package them into distributable tarballs")
		fmt.Fprintln(w)
		io.WriteString(w, globalHelp+"\n")
		fmt.Fprintln(w)
		fmt.Fprintln(w, layoutHelp)
		fmt.Fprintln(w)
		fmt.Fprint(w, tripleHelp())
		return
	}
	c := lookup(topic)
	if c == nil {
		fmt.Fprintf(w, "no such command %q\n", topic)
		return
	}
	fmt.Fprintln(w, bold("gccfactory "+c.Name)+" - "+c.Short)
	fmt.Fprintln(w)
	fmt.Fprintln(w, bold("USAGE"))
	fmt.Fprintln(w, "  "+c.Synopsis)
	if c.Long != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, strings.TrimRight(c.Long, "\n"))
	}
	fmt.Fprintln(w)
	io.WriteString(w, globalHelp+"\n")
}
