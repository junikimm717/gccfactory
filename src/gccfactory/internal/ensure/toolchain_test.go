package ensure

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// fakeToolchain lays out a plausible <prefix> so the structural checks and the
// probe matrix can be driven without an hour of compiling. Binaries are
// synthetic ELF headers, which is exactly what ReadELF looks at.
type fakeToolchain struct {
	prefix string
	host   triple.Triple
	target triple.Triple
}

func newFakeToolchain(t *testing.T, host, target triple.Triple) *fakeToolchain {
	t.Helper()
	f := &fakeToolchain{prefix: t.TempDir(), host: host, target: target}
	if err := os.MkdirAll(filepath.Join(f.prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range ToolNames(target, UnprefixedTools...) {
		synthELF(t, filepath.Join(f.prefix, "bin", n), host, "")
	}
	// binutils' second, unprefixed copy: this is what gcc actually execs.
	if err := os.MkdirAll(ToolDir(f.prefix, target), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range ToolDirTools {
		synthELF(t, filepath.Join(ToolDir(f.prefix, target), n), host, "")
	}
	plugin := filepath.Join(f.prefix, "libexec", "gcc", target.Raw, "14.2.0")
	if err := os.MkdirAll(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "liblto_plugin.a"), []byte("!<arch>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(Sysroot(f.prefix, target), "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, "libc.a"), []byte("!<arch>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	synthELF(t, filepath.Join(Sysroot(f.prefix, target), target.DynamicLinker()), target, "")
	return f
}

// fakeRunner pretends to be the toolchain: it "compiles" by writing a
// synthetic ELF for the target and "runs" by printing the probe's expected
// output. Hooks let a test corrupt exactly one behaviour.
type fakeRunner struct {
	t            *testing.T
	target       triple.Triple
	prefix       string
	mu           sync.Mutex
	cmds         []Cmd
	badOut       string // probe name whose stdout should be wrong
	failComp     string // probe name whose compile should fail
	bareProg     string // tool that -print-prog-name should resolve to a bare name
	noSymbols    bool   // gcc-nm reports nothing, as it would with no linker plugin
	pluginFlag   string // a flag the compiler rejects, as one with no linker plugin does
	fatObjects   bool   // plain nm sees the symbols: the objects are not bitcode-only
	loaderBroken bool   // qemu -L <sysroot> cannot find the interpreter
}

func newFakeRunner(t *testing.T, f *fakeToolchain) *fakeRunner {
	return &fakeRunner{t: t, target: f.target, prefix: f.prefix}
}

func (f *fakeRunner) Run(ctx context.Context, c Cmd) error {
	_, err := f.Output(ctx, c)
	return err
}

func (f *fakeRunner) Output(ctx context.Context, c Cmd) ([]byte, error) {
	f.mu.Lock()
	f.cmds = append(f.cmds, c)
	f.mu.Unlock()

	if has(c.Args, "-dumpmachine") {
		return []byte(f.target.Raw + "\n"), nil
	}
	for _, a := range c.Args {
		tool, ok := strings.CutPrefix(a, "-print-prog-name=")
		if !ok {
			continue
		}
		if tool == f.bareProg {
			return []byte(tool + "\n"), nil // gcc found nothing: $PATH lookup
		}
		return []byte(filepath.Join(ToolDir(f.prefix, f.target), tool) + "\n"), nil
	}
	switch f.toolIn(c.Args) {
	case "nm":
		// A slim LTO archive shows nothing to a plugin-less nm.
		if f.fatObjects {
			return []byte("\nslim.o:\n0000000000000000 T helper_add\n"), nil
		}
		return []byte("\nslim.o:\nnm: slim.o: no symbols\n"), nil
	case "gcc-nm":
		if f.noSymbols {
			return []byte("\na.o:\n"), nil
		}
		return []byte("\na.o:\n0000000000000000 T helper_add\n0000000000000020 T helper_tag\n"), nil
	case "gcc-ar", "gcc-ranlib":
		for _, a := range c.Args {
			if strings.HasSuffix(a, ".a") {
				return nil, os.WriteFile(filepath.Join(c.Dir, a), []byte("!<arch>\n"), 0o644)
			}
		}
		return nil, nil
	}
	if f.pluginFlag != "" && has(c.Args, f.pluginFlag) {
		return []byte("cc1: error: " + f.pluginFlag + " is not supported in this configuration\n"), fakeCmdErr{}
	}
	if filepath.Base(c.Args[0]) == "sh" { // a probe run: sh -c "exec ./probe > probe.stdout"
		p, ok := f.probeIn(c.Dir)
		if !ok {
			return nil, fmt.Errorf("fake: no probe sources in %s", c.Dir)
		}
		if f.loaderBroken && strings.Contains(strings.Join(c.Args, " "), " -L ") {
			return []byte("qemu-" + f.target.QemuName() + ": Could not open '" +
				f.target.DynamicLinker() + "': No such file or directory\n"), fakeExitErr{}
		}
		out := p.Want
		if p.Name == f.badOut {
			out = "wrong\n"
		}
		return nil, os.WriteFile(filepath.Join(c.Dir, "probe.stdout"), []byte(out), 0o644)
	}
	// a compile
	if p, ok := f.probeIn(c.Dir); ok && p.Name == f.failComp {
		return []byte("cc1: error: fake compile failure\n"), fakeCmdErr{}
	}
	out := "a.out"
	for i, a := range c.Args {
		if a == "-o" && i+1 < len(c.Args) {
			out = c.Args[i+1]
		}
	}
	interp := f.target.DynamicLinker()
	// -static-pie produces no PT_INTERP either; modelling that is what makes
	// the matrix test able to catch a wrong link expectation.
	if has(c.Args, "-static") || has(c.Args, "-shared") || has(c.Args, "-static-pie") {
		interp = ""
	}
	synthELF(f.t, filepath.Join(c.Dir, out), f.target, interp)
	return nil, nil
}

// toolIn names the target-prefixed toolchain program an argv invokes (e.g.
// "gcc-nm"), looking past a qemu launcher and any flags.
func (f *fakeRunner) toolIn(args []string) string {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if n, ok := strings.CutPrefix(filepath.Base(a), f.target.Raw+"-"); ok {
			return n
		}
	}
	return ""
}

func (f *fakeRunner) probeIn(dir string) (Probe, bool) {
	for _, p := range Probes() {
		for _, src := range p.Files {
			if strings.HasPrefix(src, "lib") {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, src)); err == nil {
				return p, true
			}
		}
	}
	return Probe{}, false
}

type fakeExitErr struct{}

func (fakeExitErr) Error() string { return "exit status 255" }

// fakeCmdErr stands in for *core.CmdError, which knows its own log file.
type fakeCmdErr struct{}

func (fakeCmdErr) Error() string   { return "exit status 1 (see the log)" }
func (fakeCmdErr) LogPath() string { return "/dist/logs/jobs/j/012-cc.log" }

func has(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestCanadianToolchainHappyPath(t *testing.T) {
	host := triple.MustParse("x86_64-linux-musl")
	target := triple.MustParse("aarch64-linux-musl")
	f := newFakeToolchain(t, host, target)
	r := newFakeRunner(t, f)

	rep := CanadianToolchain(context.Background(), r, t.TempDir(), f.prefix, host, target,
		"/usr/bin/qemu-x86_64", "/usr/bin/qemu-aarch64")
	if !rep.OK() {
		t.Fatalf("expected a clean report:\n%s", rep)
	}

	// The full matrix: every probe at every -O level, plus -static where allowed.
	wantProbeChecks := 0
	for _, p := range Probes() {
		wantProbeChecks += 2 // -O0, -O2 dynamic
		if p.Static && !p.Shared {
			wantProbeChecks += 2
		}
	}
	got := 0
	for _, c := range rep.Checks {
		if strings.HasPrefix(c.Name, "probe:") {
			got++
		}
	}
	if got != wantProbeChecks {
		t.Errorf("ran %d probe cells, want %d\n%s", got, wantProbeChecks, rep)
	}

	// The compiler must have been invoked through the host qemu.
	var sawQemuCompile, sawQemuRun bool
	for _, c := range r.cmds {
		if c.Args[0] == "/usr/bin/qemu-x86_64" && has(c.Args, "-o") {
			sawQemuCompile = true
		}
		if strings.Contains(strings.Join(c.Args, " "), "/usr/bin/qemu-aarch64 -L "+Sysroot(f.prefix, target)+" ./probe") {
			sawQemuRun = true
		}
	}
	if !sawQemuCompile {
		t.Error("compiles did not go through the host qemu")
	}
	if !sawQemuRun {
		t.Errorf("probe runs did not go through the target qemu with -L <sysroot>")
	}
}

func TestCanadianToolchainCatchesBadStdout(t *testing.T) {
	host := triple.MustParse("x86_64-linux-musl")
	target := triple.MustParse("riscv64-linux-musl")
	f := newFakeToolchain(t, host, target)
	r := newFakeRunner(t, f)
	r.badOut = "tls"

	rep := CanadianToolchain(context.Background(), r, t.TempDir(), f.prefix, host, target,
		"qemu-x86_64", "qemu-riscv64", WithOptLevels("-O2"))
	if rep.OK() {
		t.Fatal("a probe printing the wrong thing must fail the report")
	}
	fails := rep.Failures()
	if len(fails) != 2 { // dynamic + static
		t.Fatalf("want exactly the two tls cells to fail, got %d:\n%s", len(fails), rep)
	}
	err := rep.Err().Error()
	for _, frag := range []string{"probe:tls/-O2/dynamic", "stdout mismatch", `want "tls[0]=107 tag=t0\n"`, `got "wrong\n"`} {
		if !strings.Contains(err, frag) {
			t.Errorf("failure text missing %q:\n%s", frag, err)
		}
	}
}

func TestCanadianToolchainCatchesCompileFailure(t *testing.T) {
	host := triple.MustParse("aarch64-linux-musl")
	target := triple.MustParse("powerpc64-linux-musl")
	f := newFakeToolchain(t, host, target)
	r := newFakeRunner(t, f)
	r.failComp = "hello"

	rep := CanadianToolchain(context.Background(), r, t.TempDir(), f.prefix, host, target,
		"qemu-aarch64", "qemu-ppc64", WithOptLevels("-O0"))
	if rep.OK() {
		t.Fatal("a failing compile must fail the report")
	}
	// hello is the preflight probe, so the run aborts early and says so.
	if len(rep.Failures()) != 1 || rep.Failures()[0].Name != "preflight-compile" {
		t.Fatalf("expected a single preflight failure:\n%s", rep)
	}
	c := rep.Failures()[0]
	if c.LogPath != "/dist/logs/jobs/j/012-cc.log" {
		t.Errorf("log path not recovered from the error: %q", c.LogPath)
	}
	if !strings.Contains(c.Detail, "cc1: error: fake compile failure") {
		t.Errorf("compiler output not preserved:\n%s", c.Detail)
	}
}

func TestCanadianToolchainCatchesWrongHostArch(t *testing.T) {
	host := triple.MustParse("x86_64-linux-musl")
	target := triple.MustParse("aarch64-linux-musl")
	f := newFakeToolchain(t, host, target)
	bad := filepath.Join(f.prefix, "bin", target.Raw+"-objdump")
	synthELF(t, bad, triple.MustParse("s390x-linux-musl"), "")

	rep := HostBinDirReport(f.prefix, host)
	if rep.OK() {
		t.Fatal("a non-host binary in bin/ must fail")
	}
	want := bad + ": expected ELF64/LE/EM_X86_64(62), got ELF64/BE/EM_S390(22)"
	if !strings.Contains(rep.Err().Error(), want) {
		t.Fatalf("want %q in:\n%s", want, rep.Err())
	}
}

func TestToolSurfaceMissing(t *testing.T) {
	host := triple.MustParse("x86_64-linux-musl")
	target := triple.MustParse("arm-linux-musleabihf")
	f := newFakeToolchain(t, host, target)
	if err := os.Remove(filepath.Join(f.prefix, "bin", "make")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.prefix, "bin", target.Raw+"-gprof")); err != nil {
		t.Fatal(err)
	}
	rep := ToolSurface(f.prefix, target, "make")
	if rep.OK() {
		t.Fatal("missing tools must fail")
	}
	if !strings.Contains(rep.Err().Error(), "missing (2): arm-linux-musleabihf-gprof make") {
		t.Fatalf("bad message:\n%s", rep.Err())
	}

	full := newFakeToolchain(t, host, target)
	if r := ToolSurface(full.prefix, target, UnprefixedTools...); !r.OK() {
		t.Fatalf("a complete prefix must pass:\n%s", r)
	}
}

func TestSysrootReport(t *testing.T) {
	host := triple.MustParse("x86_64-linux-musl")
	target := triple.MustParse("mips64-linux-musl")
	f := newFakeToolchain(t, host, target)
	if r := SysrootReport(f.prefix, target); !r.OK() {
		t.Fatalf("%s", r)
	}
	if err := os.Remove(filepath.Join(Sysroot(f.prefix, target), target.DynamicLinker())); err != nil {
		t.Fatal(err)
	}
	r := SysrootReport(f.prefix, target)
	if r.OK() || !strings.Contains(r.Err().Error(), "musl loader") {
		t.Fatalf("%s", r)
	}
}

// A cross:<T> artifact has no make -- make for HOST is built *by* a cross
// toolchain -- so requiring it would fail every cross verification.
func TestCrossToolchainDoesNotRequireMake(t *testing.T) {
	host := triple.MustParse("x86_64-linux-musl")
	target := triple.MustParse("x86_64-linux-musl")
	f := newFakeToolchain(t, host, target)
	if err := os.Remove(filepath.Join(f.prefix, "bin", "make")); err != nil {
		t.Fatal(err)
	}
	r := newFakeRunner(t, f)
	rep := CrossToolchain(context.Background(), r, t.TempDir(), f.prefix, target, "/usr/bin/qemu-x86_64",
		WithOptLevels("-O0"), WithProbes(Probes()[0]))
	for _, c := range rep.Failures() {
		if c.Name == "tools" {
			t.Fatalf("cross toolchains must not be asked for make:\n%s", rep)
		}
	}
	crep := ToolSurface(f.prefix, target, UnprefixedTools...)
	if crep.OK() || !strings.Contains(crep.Err().Error(), "missing (1): make") {
		t.Fatalf("canadian surface must demand make:\n%s", crep)
	}
}

// The tooldir binaries run wherever the compiler runs: HOST for a canadian
// toolchain. Seeding it from the cross toolchain plants BUILD binaries there.
func TestToolDirMustBeHostArch(t *testing.T) {
	host := triple.MustParse("x86_64-linux-musl")
	target := triple.MustParse("aarch64-linux-musl")
	f := newFakeToolchain(t, host, target)
	bad := filepath.Join(ToolDir(f.prefix, target), "as")
	synthELF(t, bad, triple.MustParse("aarch64-linux-musl"), "") // BUILD-arch leftover

	rep := ToolDirBinReport(f.prefix, target, host)
	if rep.OK() {
		t.Fatal("a non-host binary in the tooldir must fail")
	}
	want := bad + ": expected ELF64/LE/EM_X86_64(62), got ELF64/LE/EM_AARCH64(183)"
	if !strings.Contains(rep.Err().Error(), want) {
		t.Fatalf("want %q in:\n%s", want, rep.Err())
	}

	full := CanadianToolchain(context.Background(), newFakeRunner(t, f), t.TempDir(), f.prefix,
		host, target, "qemu-x86_64", "qemu-aarch64", WithOptLevels("-O0"))
	if !failed(full, "tooldir-elf") {
		t.Fatalf("CanadianToolchain must inspect the tooldir:\n%s", full)
	}
}

// A missing tooldir as/ld is the single highest-signal sign of a broken
// install: report it once, do not run 38 doomed probes.
func TestMissingToolDirAsAbortsEarly(t *testing.T) {
	host := triple.MustParse("aarch64-linux-musl")
	target := triple.MustParse("x86_64-linux-musl")
	f := newFakeToolchain(t, host, target)
	if err := os.Remove(filepath.Join(ToolDir(f.prefix, target), "as")); err != nil {
		t.Fatal(err)
	}
	rep := CanadianToolchain(context.Background(), newFakeRunner(t, f), t.TempDir(), f.prefix,
		host, target, "qemu-aarch64", "qemu-x86_64")
	if !failed(rep, "tooldir-as-ld") {
		t.Fatalf("missing as must fail:\n%s", rep)
	}
	for _, c := range rep.Checks {
		if strings.HasPrefix(c.Name, "probe:") {
			t.Fatalf("no probe should run once as is missing:\n%s", rep)
		}
	}
	msg := rep.Err().Error()
	for _, frag := range []string{ToolDir(f.prefix, target), "is missing as", "$PATH"} {
		if !strings.Contains(msg, frag) {
			t.Errorf("remediation must mention %q:\n%s", frag, msg)
		}
	}

	crep := CrossToolchain(context.Background(), newFakeRunner(t, f), t.TempDir(), f.prefix, target, "qemu-x86_64")
	if !failed(crep, "tooldir-as-ld") {
		t.Fatalf("cross must check the tooldir too:\n%s", crep)
	}
}

// gcc printing a bare "as" means it never found its own assembler and will
// exec the build machine's.
func TestGccMustResolveItsAssembler(t *testing.T) {
	host := triple.MustParse("x86_64-linux-musl")
	target := triple.MustParse("s390x-linux-musl")
	f := newFakeToolchain(t, host, target)
	r := newFakeRunner(t, f)
	r.bareProg = "as"

	rep := CanadianToolchain(context.Background(), r, t.TempDir(), f.prefix, host, target,
		"qemu-x86_64", "qemu-s390x")
	if !failed(rep, "gcc-finds-as") {
		t.Fatalf("a bare \"as\" must fail:\n%s", rep)
	}
	for _, c := range rep.Checks {
		if strings.HasPrefix(c.Name, "probe:") {
			t.Fatalf("probes must not run when gcc cannot find as:\n%s", rep)
		}
	}
	msg := rep.Err().Error()
	for _, frag := range []string{"bare name \"as\"", ToolDir(f.prefix, target), "$PATH"} {
		if !strings.Contains(msg, frag) {
			t.Errorf("remediation must mention %q:\n%s", frag, msg)
		}
	}

	// The happy path resolves inside the prefix and says where.
	good := CanadianToolchain(context.Background(), newFakeRunner(t, f), t.TempDir(), f.prefix,
		host, target, "qemu-x86_64", "qemu-s390x", WithOptLevels("-O0"))
	if !good.OK() {
		t.Fatalf("%s", good)
	}
	var seen bool
	for _, c := range good.Checks {
		if c.Name == "gcc-finds-ld" && c.Detail == filepath.Join(ToolDir(f.prefix, target), "ld") {
			seen = true
		}
	}
	if !seen {
		t.Errorf("the resolved linker path should be recorded:\n%s", good)
	}
}

// musl installs ld-musl-<arch>.so.1 as an absolute symlink to /lib/libc.so.
// qemu -L <sysroot> resolves the interpreter through the host filesystem and
// cannot follow it, so every dynamic target binary dies.
func TestAbsoluteLoaderSymlinkIsReported(t *testing.T) {
	host := triple.MustParse("aarch64-linux-musl")
	target := triple.MustParse("x86_64-linux-musl")
	f := newFakeToolchain(t, host, target)
	lib := filepath.Join(Sysroot(f.prefix, target), "lib")
	ldso := filepath.Join(lib, "ld-musl-x86_64.so.1")

	// as musl installs it: an absolute link to the *host's* /lib/libc.so
	synthELF(t, filepath.Join(lib, "libc.so"), target, "")
	if err := os.Remove(ldso); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/lib/libc.so", ldso); err != nil {
		t.Fatal(err)
	}

	rep := SysrootReport(f.prefix, target)
	if rep.OK() {
		t.Fatalf("an absolute loader symlink must be reported:\n%s", rep)
	}
	msg := rep.Err().Error()
	for _, frag := range []string{
		"is an absolute symlink to /lib/libc.so",
		"Could not open '/lib/ld-musl-x86_64.so.1'",
		"ln -sf libc.so ld-musl-x86_64.so.1",
	} {
		if !strings.Contains(msg, frag) {
			t.Errorf("message must contain %q:\n%s", frag, msg)
		}
	}
	// The ELF identity of the loader is still verified, through the sysroot.
	if failed(rep, "sysroot-ldso") {
		t.Errorf("the loader itself is fine; only the link is wrong:\n%s", rep)
	}
	if got := LoaderPath(Sysroot(f.prefix, target), target); got != filepath.Join(lib, "libc.so") {
		t.Errorf("LoaderPath must resolve inside the sysroot, got %q", got)
	}

	if err := os.Remove(ldso); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("libc.so", ldso); err != nil {
		t.Fatal(err)
	}
	if r := SysrootReport(f.prefix, target); !r.OK() {
		t.Fatalf("a relative link must pass:\n%s", r)
	}
}

// When qemu -L cannot load the interpreter, ensure retries through the loader
// directly -- and says so, so the toolchain is visibly degraded.
func TestLoaderFallbackIsUsedAndReported(t *testing.T) {
	host := triple.MustParse("x86_64-linux-musl")
	target := triple.MustParse("aarch64-linux-musl")
	f := newFakeToolchain(t, host, target)
	r := newFakeRunner(t, f)
	r.loaderBroken = true

	rep := CanadianToolchain(context.Background(), r, t.TempDir(), f.prefix, host, target,
		"qemu-x86_64", "qemu-aarch64", WithOptLevels("-O2"), WithProbes(Probes()[0]))
	if !rep.OK() {
		t.Fatalf("the fallback must rescue the run:\n%s", rep)
	}
	var degraded int
	for _, c := range rep.Checks {
		if strings.HasPrefix(c.Name, "probe:") {
			if !strings.Contains(c.Detail, "DEGRADED") {
				t.Errorf("%s must be marked degraded, got %q", c.Name, c.Detail)
			}
			degraded++
		}
	}
	if degraded == 0 {
		t.Fatal("no probe ran")
	}
	// The fallback invokes the loader directly, without -L.
	var sawFallback bool
	for _, c := range r.cmds {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, filepath.Join(Sysroot(f.prefix, target), "lib", "ld-musl-aarch64.so.1")) &&
			!strings.Contains(joined, " -L ") {
			sawFallback = true
		}
	}
	if !sawFallback {
		t.Error("expected a retry that hands the binary to the musl loader")
	}

	// If the fallback also fails, the original failure stands.
	f2 := newFakeToolchain(t, host, target)
	if err := os.Remove(filepath.Join(Sysroot(f2.prefix, target), target.DynamicLinker())); err != nil {
		t.Fatal(err)
	}
	r2 := newFakeRunner(t, f2)
	r2.loaderBroken = true
	rep2 := CanadianToolchain(context.Background(), r2, t.TempDir(), f2.prefix, host, target,
		"qemu-x86_64", "qemu-aarch64", WithOptLevels("-O2"), WithProbes(Probes()[0]))
	if rep2.OK() {
		t.Fatalf("with no loader at all this must fail:\n%s", rep2)
	}
}

func TestLTOPluginParity(t *testing.T) {
	host := triple.MustParse("x86_64-linux-musl")
	target := triple.MustParse("aarch64-linux-musl")
	f := newFakeToolchain(t, host, target)
	plugin := filepath.Join(f.prefix, "libexec", "gcc", target.Raw, "14.2.0")

	// The plugin is linked into ld, so a .a-only install is the normal shape
	// and must not read as a deficiency.
	rep := LTOPluginReport(f.prefix, target)
	if !rep.OK() || len(rep.Checks) != 1 || rep.Checks[0].Skipped {
		t.Fatalf("a .a-only toolchain is the expected shape, not a gap:\n%s", rep)
	}
	if !strings.Contains(rep.Checks[0].Detail, "linked into ld/ar") {
		t.Errorf("detail = %q", rep.Checks[0].Detail)
	}

	// musl-cross-make parity: a shared plugin.
	if err := os.WriteFile(filepath.Join(plugin, "liblto_plugin.so"), []byte("\x7fELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	rep = LTOPluginReport(f.prefix, target)
	if !rep.OK() || rep.Checks[0].Skipped || !strings.Contains(rep.Checks[0].Detail, "liblto_plugin.so present") {
		t.Fatalf("a .so must read as parity:\n%s", rep)
	}

	// Never fatal, even with nothing at all -- and it must point at the check
	// that actually decides.
	if err := os.RemoveAll(filepath.Join(f.prefix, "libexec")); err != nil {
		t.Fatal(err)
	}
	r := LTOPluginReport(f.prefix, target)
	if !r.OK() {
		t.Fatalf("the plugin check must never fail a toolchain:\n%s", r)
	}
	if !strings.Contains(r.Checks[0].Detail, "lto-plugin-link") {
		t.Errorf("an absent plugin must defer to the functional check, got %q", r.Checks[0].Detail)
	}
}

// gcc-nm seeing no symbols in an -flto archive is how a missing linker plugin
// actually manifests.
func TestLTOArchiveDetectsPluginlessNm(t *testing.T) {
	host := triple.MustParse("x86_64-linux-musl")
	target := triple.MustParse("aarch64-linux-musl")
	f := newFakeToolchain(t, host, target)
	r := newFakeRunner(t, f)
	r.noSymbols = true

	rep := CanadianToolchain(context.Background(), r, t.TempDir(), f.prefix, host, target,
		"qemu-x86_64", "qemu-aarch64", WithOptLevels("-O2"), WithProbes(Probes()...))
	if !failed(rep, "gcc-nm-lto") {
		t.Fatalf("gcc-nm listing nothing must fail:\n%s", rep)
	}
	if !strings.Contains(rep.Err().Error(), "linker plugin is not being loaded") {
		t.Errorf("the failure should name the cause:\n%s", rep.Err())
	}
	// The archive still links and runs, so that check is independent.
	if failed(rep, "lto-archive") {
		t.Errorf("lto-archive should be judged on its own:\n%s", rep)
	}
	// The same blindness fails the sharper parity check.
	if !failed(rep, "lto-plugin-nm-parity") {
		t.Errorf("a gcc-nm that sees nothing must fail the parity check:\n%s", rep)
	}
	var ltoCells int
	for _, c := range rep.Checks {
		if strings.HasPrefix(c.Name, "probe:lto/") {
			ltoCells++
			if !c.OK {
				t.Errorf("%s failed:\n%s", c.Name, rep)
			}
		}
	}
	if ltoCells != 2 { // -O2 dynamic + static
		t.Errorf("expected 2 lto probe cells, got %d", ltoCells)
	}
}

// A toolchain that quietly loses plugin LTO must not verify: each step of
// lto-plugin is one only a working plugin can take.
func TestLTOPluginChecksCatchABrokenPlugin(t *testing.T) {
	host := triple.MustParse("x86_64-linux-musl")
	target := triple.MustParse("aarch64-linux-musl")
	lto, err := ProbesNamed([]string{"lto"})
	if err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, tweak func(*fakeRunner)) *Report {
		t.Helper()
		f := newFakeToolchain(t, host, target)
		r := newFakeRunner(t, f)
		tweak(r)
		return CanadianToolchain(context.Background(), r, t.TempDir(), f.prefix, host, target,
			"qemu-x86_64", "qemu-aarch64", WithOptLevels("-O2"), WithProbes(lto...))
	}
	ran := func(rep *Report, name string) bool {
		for _, c := range rep.Checks {
			if c.Name == name {
				return true
			}
		}
		return false
	}

	// gcc without a working plugin rejects -fno-fat-lto-objects outright.
	t.Run("no slim objects", func(t *testing.T) {
		rep := run(t, func(r *fakeRunner) { r.pluginFlag = "-fno-fat-lto-objects" })
		if !failed(rep, "lto-slim-object") {
			t.Fatalf("a rejected -fno-fat-lto-objects must fail:\n%s", rep)
		}
		if ran(rep, "lto-plugin-link") {
			t.Errorf("nothing downstream should be claimed once slim objects fail:\n%s", rep)
		}
		if !strings.Contains(rep.Err().Error(), "builtin-lto-plugin") {
			t.Errorf("the failure should name the remedy:\n%s", rep.Err())
		}
	})

	// The plugin link path is separately fatal: the old gcc error was
	// "-fuse-linker-plugin is not supported in this configuration".
	t.Run("no plugin link", func(t *testing.T) {
		rep := run(t, func(r *fakeRunner) { r.pluginFlag = "-fuse-linker-plugin" })
		if !failed(rep, "lto-plugin-link") {
			t.Fatalf("a rejected -fuse-linker-plugin must fail:\n%s", rep)
		}
		if failed(rep, "lto-slim-object") || failed(rep, "lto-plugin-nm-parity") {
			t.Errorf("only the link should fail here:\n%s", rep)
		}
	})

	// Fat objects would let every other step "work" without a plugin, which is
	// exactly the silent regression worth catching.
	t.Run("objects are not slim", func(t *testing.T) {
		rep := run(t, func(r *fakeRunner) { r.fatObjects = true })
		if !failed(rep, "lto-plugin-nm-parity") {
			t.Fatalf("plain nm seeing helper_add must fail:\n%s", rep)
		}
		if !strings.Contains(rep.Err().Error(), "did not take effect") {
			t.Errorf("the failure should say what it means:\n%s", rep.Err())
		}
	})

	// And the happy path exercises all three.
	t.Run("working plugin", func(t *testing.T) {
		rep := run(t, func(*fakeRunner) {})
		if !rep.OK() {
			t.Fatalf("a working plugin must pass:\n%s", rep)
		}
		for _, name := range []string{"lto-slim-object", "lto-plugin-nm-parity", "lto-plugin-link"} {
			if !ran(rep, name) {
				t.Errorf("%s did not run:\n%s", name, rep)
			}
		}
	})
}
