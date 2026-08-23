package ensure

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

//go:embed probes/*
var probeFS embed.FS

// Probe is one self-checking program: it is compiled by the toolchain under
// test, its ELF identity is asserted, then it is executed and its stdout must
// equal Want byte for byte.
type Probe struct {
	Name string
	// Files are basenames under internal/ensure/probes/. A file whose name
	// starts with "lib" is compiled to a shared object of the same base name
	// (libprobe.c -> libprobe.so) instead of being linked into the program.
	Files   []string
	Lang    string   // "c" | "c++"
	ExtraCC []string // extra compiler/linker flags, e.g. {"-lm"}
	Want    string   // exact expected stdout
	// Static also builds and runs the probe with -static.
	Static bool
	// Shared marks a probe that builds a shared library and dlopens it; such a
	// probe can never be linked -static.
	Shared bool
	// Skip reports why this probe cannot run for a target ("" means run it).
	Skip func(triple.Triple) string
	// ExtraCCFor adds target-dependent flags on top of ExtraCC.
	ExtraCCFor func(triple.Triple) []string
	// NoInterp marks a probe whose output has no PT_INTERP despite not being
	// built with -static, i.e. -static-pie.
	NoInterp bool
}

func (p Probe) Flags(t triple.Triple) []string {
	out := append([]string(nil), p.ExtraCC...)
	if p.ExtraCCFor != nil {
		out = append(out, p.ExtraCCFor(t)...)
	}
	return out
}

func (p Probe) Source(name string) ([]byte, error) {
	return probeFS.ReadFile("probes/" + name)
}

func (p Probe) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range p.Files {
		b, err := p.Source(f)
		if err != nil {
			return fmt.Errorf("probe %s: %w", p.Name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (p Probe) SharedSources() (lib, main []string) {
	for _, f := range p.Files {
		if strings.HasPrefix(f, "lib") {
			lib = append(lib, f)
		} else {
			main = append(main, f)
		}
	}
	return
}

// atomicsNeedLibatomic: on 32-bit targets a 64-bit __atomic_fetch_add is a
// libatomic call rather than an instruction.
func atomicsNeedLibatomic(t triple.Triple) []string {
	if t.Bits() == 32 {
		return []string{"-latomic"}
	}
	return nil
}

// staticPIESkip: arm has no upstream -static-pie support (gcc PR 106356,
// still open); mips64's n64 ABI is untested here. Skip both rather than risk
// a false failure blocking the build.
func staticPIESkip(t triple.Triple) string {
	switch t.Raw {
	case "":
		// Native: the build machine's libc, which we neither ship nor control.
		return "native host may not ship a static libc (Fedora splits it into glibc-static)"
	case "arm-linux-musleabi", "arm-linux-musleabihf":
		return "gcc has no -static-pie support for arm (PR 106356)"
	case "mips64-linux-musl":
		return "static-pie on mips64 n64 is unproven here"
	}
	return ""
}

// Probes returns the probe suite, in the order it should be run (cheapest and
// most fundamental first, so a broken toolchain fails fast and legibly).
func Probes() []Probe {
	return []Probe{
		{
			Name: "hello", Files: []string{"hello.c"}, Lang: "c", Static: true,
			Want: "hello/42/1.50/x\nMUSL TOOLCHAIN\nOK hello\n",
		},
		{
			Name: "math", Files: []string{"math.c"}, Lang: "c", Static: true,
			ExtraCC: []string{"-lm"},
			Want: "sqrt=1.414214\npow=1024.000000\nhypot=5.000000\nfmod=1.000000\n" +
				"exp=2.718282\nfloor=-1.000000 ceil=1.000000\nOK math\n",
		},
		{
			Name: "pthread", Files: []string{"pthread.c"}, Lang: "c", Static: true,
			ExtraCC: []string{"-pthread"},
			Want:    "started=4\nsum=100000\nOK pthread\n",
		},
		{
			Name: "tls", Files: []string{"tls.c"}, Lang: "c", Static: true,
			ExtraCC: []string{"-pthread"},
			Want: "tls[0]=107 tag=t0\ntls[1]=118 tag=t1\ntls[2]=129 tag=t2\n" +
				"main slot=1 base=7 tag=none\nOK tls\n",
		},
		{
			Name: "atomic", Files: []string{"atomic.c"}, Lang: "c", Static: true,
			ExtraCC: []string{"-pthread"}, ExtraCCFor: atomicsNeedLibatomic,
			Want: "counter=80000\ncas_hit=1\ncas_miss=0\nafter=1 witness=1\n" +
				"flag=5 free=1\nOK atomic\n",
		},
		{
			Name: "static", Files: []string{"static.c"}, Lang: "c", Static: true,
			Want: "sorted: 0 1 2 3 4 5 6 7 8 9\nbsearch=6\nwords=alpha,bravo,charlie,delta\n" +
				"realloc=abcdef len=6\nstrtol=-12345 rest=xyz\nOK static\n",
		},
		{
			Name: "hello++", Files: []string{"hello.cc"}, Lang: "c++", Static: true,
			Want: "joined=alpha,bravo,charlie\ntotal=15\naccumulate=55\n" +
				"substr=bravo size=19\nOK hello++\n",
		},
		{
			Name: "except", Files: []string{"except.cc"}, Lang: "c++", Static: true,
			Want: "caught=boom at bottom code=17\ndestroyed=4\nhierarchy=range\n" +
				"int=42\nnested=nested\nOK except\n",
		},
		{
			Name: "stdcxx", Files: []string{"stdcxx.cc"}, Lang: "c++", Static: true,
			ExtraCC: []string{"-pthread"},
			Want: "sorted=alpha,bravo,charlie,delta,echo\nmap=5 a=5 c=7\nthreads=500500\n" +
				"regex=riscv/1234\nchain=123\nlambda=charlie\nOK stdcxx\n",
		},
		{
			// Two translation units so -flto has real cross-unit work; also the
			// source of the gcc-ar/gcc-nm archive check (see harness.ltoArchive).
			Name: "lto", Files: []string{"lto.c", "ltohelp.c"}, Lang: "c", Static: true,
			ExtraCC: []string{"-flto"},
			Want:    "add=42\nscale=42\ntag=lto\ncalls=2\nsecret=42\nOK lto\n",
		},
		{
			Name: "dlopen", Files: []string{"dlopen.c", "libprobe.c"}, Lang: "c",
			Shared: true, ExtraCC: []string{"-ldl"},
			Want: "name=libprobe\nadd=42\nadd=0\ncalls=2\nmissing=null\nOK dlopen\n",
		},
		{
			Name: "static-pie", Files: []string{"staticpie.c"}, Lang: "c",
			ExtraCC: []string{"-fPIE", "-static-pie"}, Skip: staticPIESkip, NoInterp: true,
			Want: "counter=22 ptr=22 call=32\nOK static-pie\n",
		},
	}
}

// Unknown names are an error so a typo in `--probes` is not silently a
// no-op.
func ProbesNamed(want []string) ([]Probe, error) {
	all := Probes()
	if len(want) == 0 {
		return all, nil
	}
	byName := map[string]Probe{}
	var names []string
	for _, p := range all {
		byName[p.Name] = p
		names = append(names, p.Name)
	}
	var out []Probe
	for _, w := range want {
		p, ok := byName[w]
		if !ok {
			return nil, fmt.Errorf("unknown probe %q (have: %s)", w, strings.Join(names, ", "))
		}
		out = append(out, p)
	}
	return out, nil
}
