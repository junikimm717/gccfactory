package ensure

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// BinutilsTools are the target-prefixed tools a musl-cross-make style
// toolchain must ship in <prefix>/bin.
var BinutilsTools = []string{
	"addr2line", "ar", "as", "c++", "c++filt", "cpp", "elfedit", "g++", "gcc",
	"gcc-ar", "gcc-nm", "gcc-ranlib", "gprof", "ld", "ld.bfd", "nm", "objcopy",
	"objdump", "ranlib", "readelf", "size", "strings", "strip",
}

// UnprefixedTools are the tools shipped without a target prefix. Only the
// final canadian deliverable has them: a cross:<T> artifact cannot contain
// make, because make for HOST is built *by* a cross toolchain.
var UnprefixedTools = []string{"make"}

// ToolDirTools are the unprefixed binutils that binutils installs a second
// copy of into <prefix>/<T>/bin. That directory -- not <prefix>/bin -- is what
// gcc searches for its assembler and linker (see -print-search-dirs
// "programs:"), so these binaries run on the HOST, not the target.
var ToolDirTools = []string{"ar", "as", "ld", "nm", "objdump", "ranlib", "strip"}

// toolDirFatal are the two tools whose absence makes every later check
// meaningless: without them gcc silently falls through to the build machine's
// own as/ld on $PATH.
var toolDirFatal = []string{"as", "ld"}

func ToolNames(t triple.Triple, unprefixed ...string) []string {
	out := make([]string, 0, len(BinutilsTools)+len(unprefixed))
	for _, n := range BinutilsTools {
		out = append(out, t.Raw+"-"+n)
	}
	return append(out, unprefixed...)
}

func Sysroot(prefix string, t triple.Triple) string { return filepath.Join(prefix, t.Raw) }

// ToolDir is gcc's private program directory, <prefix>/<t>/bin. It lives
// inside the sysroot path but is not part of the sysroot: it holds HOST
// binaries (as, ld, ...) that gcc execs while compiling.
func ToolDir(prefix string, t triple.Triple) string {
	return filepath.Join(prefix, t.Raw, "bin")
}

func Tool(prefix string, t triple.Triple, name string) string {
	return filepath.Join(prefix, "bin", t.Raw+"-"+name)
}

type options struct {
	hostSysroot    string
	optLevels      []string
	probes         []Probe
	unprefixed     []string
	static         bool
	compileTimeout time.Duration
	runTimeout     time.Duration
}

// The zero set of options is the strict default: -O0 and -O2, dynamic and
// static, every probe.
type Option func(*options)

// WithHostSysroot points qemu at the sysroot holding HOST's musl loader. It is
// required when the toolchain's own binaries are dynamically linked and the
// host libc does not live under the toolchain prefix.
func WithHostSysroot(dir string) Option { return func(o *options) { o.hostSysroot = dir } }

func WithOptLevels(levels ...string) Option {
	return func(o *options) { o.optLevels = append([]string(nil), levels...) }
}

func WithProbes(ps ...Probe) Option {
	return func(o *options) { o.probes = append([]Probe(nil), ps...) }
}

// WithUnprefixedTools sets which unprefixed binaries <prefix>/bin must ship.
// CanadianToolchain requires UnprefixedTools ("make"); CrossToolchain requires
// none, because make for HOST is built by a cross toolchain, not shipped in
// one.
func WithUnprefixedTools(names ...string) Option {
	return func(o *options) { o.unprefixed = append([]string(nil), names...) }
}

func WithStatic(v bool) Option { return func(o *options) { o.static = v } }

func WithTimeouts(compile, run time.Duration) Option {
	return func(o *options) { o.compileTimeout, o.runTimeout = compile, run }
}

func newOptions(opts []Option) *options {
	o := &options{
		optLevels:      []string{"-O0", "-O2"},
		probes:         Probes(),
		static:         true,
		compileTimeout: 10 * time.Minute,
		runTimeout:     5 * time.Minute,
	}
	for _, f := range opts {
		f(o)
	}
	return o
}

// NativeToolchain checks that the BUILD compiler can compile and run C and
// C++. Produced binaries are not ELF-asserted (BUILD is whatever the container
// is) and -static is off by default because a glibc build machine usually has
// no static libc installed.
func NativeToolchain(ctx context.Context, r Runner, workDir, cc, cxx string, opts ...Option) *Report {
	o := newOptions(append([]Option{WithStatic(false)}, opts...))
	rep := NewReport(fmt.Sprintf("native toolchain (cc=%s cxx=%s)", cc, cxx))
	defer timeit(rep)()

	ok := true
	for label, tool := range map[string]string{"cc": cc, "cxx": cxx} {
		p, err := exec.LookPath(tool)
		if err != nil {
			rep.Fail(label+"-exists", err, "%s is not executable or not on PATH", tool)
			ok = false
			continue
		}
		rep.Pass(label+"-exists", "%s", p)
	}
	if !ok {
		return rep
	}

	h := &harness{r: r, rep: rep, work: filepath.Join(workDir, "native"), opts: o,
		cc: []string{cc}, cxx: []string{cxx}, env: map[string]string{}}
	if !h.version(ctx, "cc", []string{cc, "--version"}) || !h.version(ctx, "cxx", []string{cxx, "--version"}) {
		return rep
	}
	h.runAll(ctx, triple.Triple{})
	return rep
}

// CrossToolchain verifies a BUILD->TARGET toolchain at prefix: its binaries
// must be BUILD binaries, the tool surface must be complete, and the probe
// suite must compile for t and run correctly under qemu.
func CrossToolchain(ctx context.Context, r Runner, workDir, prefix string, t triple.Triple, qemu string, opts ...Option) *Report {
	o := newOptions(opts)
	rep := NewReport(fmt.Sprintf("cross toolchain %s at %s", t, prefix))
	defer timeit(rep)()

	rep.Absorb("", ToolSurface(prefix, t, o.unprefixed...))
	rep.Absorb("", SysrootReport(prefix, t))
	rep.Absorb("", BuildBinDirReport(prefix))
	rep.Absorb("", BuildToolDirReport(prefix, t))
	rep.Absorb("", LTOPluginReport(prefix, t))
	if failed(rep, "tooldir-as-ld") {
		return rep
	}

	sysroot := Sysroot(prefix, t)
	gcc := Tool(prefix, t, "gcc")
	gxx := Tool(prefix, t, "g++")
	if !mustExec(rep, gcc) || !mustExec(rep, gxx) {
		return rep
	}

	h := &harness{
		r: r, rep: rep, work: filepath.Join(workDir, "cross-"+t.Raw), opts: o,
		cc: []string{gcc}, cxx: []string{gxx}, env: map[string]string{},
		target: &t, prefix: prefix,
	}
	h.setTargetRun(ctx, qemu, sysroot, t)
	if !h.version(ctx, "gcc-runs", []string{gcc, "-dumpmachine"}, t.Raw) {
		return rep
	}
	if !h.progNames(ctx, prefix, t) {
		return rep
	}
	if !h.backends(ctx) {
		return rep
	}
	h.runAll(ctx, t)
	return rep
}

// CanadianToolchain is the real proof: every binary in <prefix>/bin is a HOST
// ELF, those binaries run under qemuHost, what they emit is a TARGET ELF, and
// that runs correctly under qemuTarget.
func CanadianToolchain(ctx context.Context, r Runner, workDir, prefix string, host, t triple.Triple, qemuHost, qemuTarget string, opts ...Option) *Report {
	o := newOptions(append([]Option{WithUnprefixedTools(UnprefixedTools...)}, opts...))
	rep := NewReport(fmt.Sprintf("canadian toolchain host=%s target=%s at %s", host, t, prefix))
	defer timeit(rep)()

	// (a) surface + host ELF identity of every shipped binary, including the
	// second copy of as/ld/... in the tooldir that gcc actually execs.
	rep.Absorb("", ToolSurface(prefix, t, o.unprefixed...))
	rep.Absorb("", SysrootReport(prefix, t))
	rep.Absorb("", HostBinDirReport(prefix, host))
	rep.Absorb("", ToolDirBinReport(prefix, t, host))
	rep.Absorb("", LTOPluginReport(prefix, t))
	if failed(rep, "tooldir-as-ld") {
		return rep
	}

	gcc := Tool(prefix, t, "gcc")
	gxx := Tool(prefix, t, "g++")
	if !mustExec(rep, gcc) || !mustExec(rep, gxx) {
		return rep
	}
	// (b) how do we have to launch a HOST binary?
	hostInfo, err := ReadELF(gcc)
	if err != nil {
		rep.Fail("host-gcc-elf", err, "cannot read %s", gcc)
		return rep
	}
	hostSysroot := o.hostSysroot
	if hostSysroot == "" {
		hostSysroot = guessHostSysroot(prefix, host)
	}
	if hostInfo.Static {
		rep.Pass("host-link-mode", "%s is static; no host sysroot needed", filepath.Base(gcc))
	} else {
		rep.Pass("host-link-mode", "%s is dynamic (%s); host sysroot %q",
			filepath.Base(gcc), hostInfo.Interp, hostSysroot)
	}

	targetSysroot := Sysroot(prefix, t)
	h := &harness{
		r: r, rep: rep, work: filepath.Join(workDir, "canadian-"+host.Raw+"-"+t.Raw), opts: o,
		cc: []string{gcc}, cxx: []string{gxx}, env: map[string]string{},
		target: &t, prefix: prefix,
	}
	// Staged preflight so a systemic problem is reported once, not 40 times.
	// chooseHostLaunch is also what establishes that HOST binaries run at all,
	// which is why nothing here requires a qemu binary to exist.
	if !h.chooseHostLaunch(ctx, gcc, gxx, host, t, hostInfo, hostSysroot, qemuHost) {
		return rep
	}
	h.setTargetRun(ctx, qemuTarget, targetSysroot, t)
	if !h.progNames(ctx, prefix, t) {
		return rep
	}
	if !h.backends(ctx) {
		return rep
	}
	if !h.preflightCompile(ctx) {
		return rep
	}
	h.runAll(ctx, t)
	return rep
}

// guessHostSysroot finds the musl loader for host inside the toolchain prefix,
// which is where it lives when host == target.
func guessHostSysroot(prefix string, host triple.Triple) string {
	cand := filepath.Join(prefix, host.Raw)
	if st, err := os.Stat(filepath.Join(cand, host.DynamicLinker())); err == nil && !st.IsDir() {
		return cand
	}
	if st, err := os.Stat(filepath.Join(cand, "lib")); err == nil && st.IsDir() {
		return cand
	}
	return os.Getenv("GCCF_HOST_SYSROOT")
}

func ToolSurface(prefix string, t triple.Triple, unprefixed ...string) *Report {
	rep := NewReport("tool surface " + prefix)
	bin := filepath.Join(prefix, "bin")
	entries, err := os.ReadDir(bin)
	if err != nil {
		rep.Fail("bin-dir", err, "%s must exist and be readable", bin)
		return rep
	}
	have := map[string]bool{}
	var present []string
	for _, e := range entries {
		have[e.Name()] = true
		present = append(present, e.Name())
	}
	want := ToolNames(t, unprefixed...)
	var missing, notExec []string
	for _, n := range want {
		p := filepath.Join(bin, n)
		st, err := os.Stat(p) // follows symlinks: a dangling link counts as missing
		if err != nil {
			missing = append(missing, n)
			continue
		}
		if st.IsDir() || !isExec(st) {
			notExec = append(notExec, n)
		}
	}
	sort.Strings(present)
	switch {
	case len(missing) == 0 && len(notExec) == 0:
		rep.Pass("tools", "%d/%d tools present in %s", len(want), len(want), bin)
	default:
		var d strings.Builder
		if len(missing) > 0 {
			fmt.Fprintf(&d, "missing (%d): %s\n", len(missing), strings.Join(missing, " "))
		}
		if len(notExec) > 0 {
			fmt.Fprintf(&d, "not executable (%d): %s\n", len(notExec), strings.Join(notExec, " "))
		}
		fmt.Fprintf(&d, "%s contains: %s", bin, strings.Join(present, " "))
		rep.Failf("tools", "%s", strings.TrimRight(d.String(), "\n"))
	}
	return rep
}

func SysrootReport(prefix string, t triple.Triple) *Report {
	rep := NewReport("sysroot " + prefix)
	sysroot := Sysroot(prefix, t)
	st, err := os.Stat(sysroot)
	if err != nil || !st.IsDir() {
		rep.Failf("sysroot", "%s must be a directory (the target sysroot): %v", sysroot, err)
		return rep
	}
	var libdir string
	for _, d := range []string{"lib", "usr/lib", "lib64"} {
		if _, err := os.Stat(filepath.Join(sysroot, d, "libc.a")); err == nil {
			libdir = filepath.Join(sysroot, d)
			break
		}
	}
	if libdir == "" {
		rep.Failf("sysroot-libc", "no libc.a under %s/{lib,usr/lib,lib64}", sysroot)
		return rep
	}
	rep.Pass("sysroot-libc", "%s", filepath.Join(libdir, "libc.a"))

	ldso := filepath.Join(sysroot, t.DynamicLinker())
	li, err := os.Lstat(ldso)
	if err != nil {
		rep.Failf("sysroot-ldso", "musl loader %s is missing (dynamic probes cannot run): %v", ldso, err)
		return rep
	}
	real := ldso
	if li.Mode()&os.ModeSymlink != 0 {
		tgt, err := os.Readlink(ldso)
		if err != nil {
			rep.Fail("sysroot-ldso", err, "cannot read the loader symlink %s", ldso)
			return rep
		}
		if filepath.IsAbs(tgt) {
			real = filepath.Join(sysroot, tgt)
			rep.Failf("sysroot-ldso-link", "%s is an absolute symlink to %s.\n"+
				"qemu -L <sysroot> resolves the interpreter through the *host* filesystem, so it cannot"+
				" follow it: every dynamic target binary dies with"+
				" \"Could not open '%s': No such file or directory\".\n"+
				"fix it in the sysroot: ln -sf %s %s",
				ldso, tgt, t.DynamicLinker(), filepath.Base(tgt), filepath.Base(ldso))
		} else {
			real = filepath.Join(filepath.Dir(ldso), tgt)
			rep.Pass("sysroot-ldso-link", "relative symlink -> %s", tgt)
		}
	} else {
		rep.Pass("sysroot-ldso-link", "not a symlink")
	}
	if _, err := os.Stat(real); err != nil {
		rep.Failf("sysroot-ldso", "the musl loader %s resolves to %s, which does not exist: %v", ldso, real, err)
		return rep
	}
	if err := ExpectELF(real, t, nil); err != nil {
		rep.Fail("sysroot-ldso", err, "the musl loader in the sysroot is not a %s binary", t)
		return rep
	}
	rep.Pass("sysroot-ldso", "%s", real)
	return rep
}

// LoaderPath returns the real musl loader file inside a sysroot, resolving the
// ld-musl-<arch>.so.1 symlink *within* the sysroot (an absolute link points at
// the host's /lib, which is never what we mean). It returns "" if there is no
// usable loader. Running `qemu <loader> ./prog` is the way to execute a
// dynamic target binary when `qemu -L <sysroot>` cannot follow that symlink.
func LoaderPath(sysroot string, t triple.Triple) string {
	ldso := filepath.Join(sysroot, t.DynamicLinker())
	real := ldso
	if li, err := os.Lstat(ldso); err == nil && li.Mode()&os.ModeSymlink != 0 {
		if tgt, err := os.Readlink(ldso); err == nil {
			if filepath.IsAbs(tgt) {
				real = filepath.Join(sysroot, tgt)
			} else {
				real = filepath.Join(filepath.Dir(ldso), tgt)
			}
		}
	}
	if st, err := os.Stat(real); err != nil || st.IsDir() {
		return ""
	}
	return real
}

// LTOPluginReport records how the LTO linker plugin is shipped. An absent .so
// is expected: --enable-builtin-lto-plugin links it into ld and ar instead. It
// never fails, and it is not the verdict -- lto-plugin-link is.
func LTOPluginReport(prefix string, t triple.Triple) *Report {
	rep := NewReport("lto plugin " + prefix)
	glob := filepath.Join(prefix, "libexec", "gcc", t.Raw, "*", "liblto_plugin.*")
	found, _ := filepath.Glob(glob)
	var so, a []string
	for _, p := range found {
		switch {
		case strings.HasSuffix(p, ".so"), strings.Contains(filepath.Base(p), ".so."):
			so = append(so, p)
		case strings.HasSuffix(p, ".a"):
			a = append(a, p)
		}
	}
	switch {
	case len(so) > 0:
		rep.Pass("lto-plugin", "liblto_plugin.so present (%s): a dynamic linker could also dlopen it", so[0])
	case len(a) > 0:
		rep.Pass("lto-plugin", "liblto_plugin.a only (%s): the plugin is linked into ld/ar rather than"+
			" dlopen'd, which is what static host tools need -- lto-plugin-link proves it works", a[0])
	default:
		rep.Skip("lto-plugin", "no liblto_plugin shipped under %s; nothing to dlopen, which is normal"+
			" for a linker with the plugin built in -- lto-plugin-link is the verdict", filepath.Dir(glob))
	}
	return rep
}

// HostBinDirReport asserts every regular file in <prefix>/bin is an ELF for
// host. This is the check that catches a "canadian" build which actually
// produced BUILD binaries.
func HostBinDirReport(prefix string, host triple.Triple) *Report {
	return elfDirReport("host binaries "+prefix, "bin-elf", filepath.Join(prefix, "bin"),
		host.String()+" ELFs", func(p string) error { return ExpectELF(p, host, nil) })
}

// BuildBinDirReport asserts the toolchain's binaries match the machine we are
// running on. It is skipped where our own executable is not an ELF (macOS).
func BuildBinDirReport(prefix string) *Report {
	ref, err := SelfELF()
	if err != nil {
		rep := NewReport("build binaries " + prefix)
		rep.Skip("bin-elf", "cannot use this process as a BUILD-arch reference: %v", err)
		return rep
	}
	return elfDirReport("build binaries "+prefix, "bin-elf", filepath.Join(prefix, "bin"),
		"BUILD ("+ELFID(ref.Class, ref.Data, ref.Machine)+") binaries",
		func(p string) error { return ExpectELFLike(p, ref, nil) })
}

// ToolDirBinReport checks <prefix>/<t>/bin, the directory gcc actually
// searches for as/ld. Those binaries run wherever the compiler runs, so for a
// canadian toolchain they must be HOST binaries -- want is the machine they
// must be built for, which is not necessarily t.
func ToolDirBinReport(prefix string, t triple.Triple, want triple.Triple) *Report {
	dir := ToolDir(prefix, t)
	rep := NewReport("tooldir " + dir)
	if !toolDirPresence(rep, dir) {
		return rep
	}
	rep.Absorb("", elfDirReport("", "tooldir-elf", dir, want.String()+" ELFs",
		func(p string) error { return ExpectELF(p, want, nil) }))
	return rep
}

// BuildToolDirReport is ToolDirBinReport for a B->T cross toolchain, whose
// tooldir binaries must run on the build machine.
func BuildToolDirReport(prefix string, t triple.Triple) *Report {
	dir := ToolDir(prefix, t)
	rep := NewReport("tooldir " + dir)
	if !toolDirPresence(rep, dir) {
		return rep
	}
	ref, err := SelfELF()
	if err != nil {
		rep.Skip("tooldir-elf", "cannot use this process as a BUILD-arch reference: %v", err)
		return rep
	}
	rep.Absorb("", elfDirReport("", "tooldir-elf", dir,
		"BUILD ("+ELFID(ref.Class, ref.Data, ref.Machine)+") binaries",
		func(p string) error { return ExpectELFLike(p, ref, nil) }))
	return rep
}

// toolDirPresence checks the tooldir holds the binutils gcc execs. Missing
// as/ld is fatal: gcc would silently fall through to the build machine's own
// assembler on $PATH and fail with things like "as: unrecognized option
// '--64'", so there is no point running any probe.
func toolDirPresence(rep *Report, dir string) bool {
	var missing, fatal []string
	for _, n := range ToolDirTools {
		st, err := os.Stat(filepath.Join(dir, n))
		if err != nil || st.IsDir() || !isExec(st) {
			missing = append(missing, n)
			if contains(toolDirFatal, n) {
				fatal = append(fatal, n)
			}
		}
	}
	if len(fatal) > 0 {
		rep.Failf("tooldir-as-ld", "%s is missing %s.\n"+
			"that directory is what gcc searches for its assembler and linker"+
			" (it is the last entry of `<T>-gcc -print-search-dirs` programs:),"+
			" so gcc would silently use the build machine's own tools from $PATH.\n"+
			"the toolchain is incompletely installed; binutils must install into --prefix with"+
			" --with-sysroot so it populates %s", dir, strings.Join(fatal, " and "), dir)
		return false
	}
	rep.Pass("tooldir-as-ld", "%s/{as,ld} present", dir)
	if len(missing) > 0 {
		rep.Failf("tooldir-tools", "%s is missing %s (expected: %s)",
			dir, strings.Join(missing, " "), strings.Join(ToolDirTools, " "))
	} else {
		rep.Pass("tooldir-tools", "%d/%d tools present", len(ToolDirTools), len(ToolDirTools))
	}
	return true
}

// elfDirReport is the single scanner behind every "this directory holds
// binaries for machine X" check; callers differ only in directory and
// reference.
func elfDirReport(subject, checkName, dir, want string, check func(string) error) *Report {
	rep := NewReport(subject)
	n, scripts, bad := scanDir(dir, check)
	switch {
	case len(bad) > 0:
		rep.Failf(checkName, "%d of %d files in %s are not %s:\n%s",
			len(bad), n, dir, want, strings.Join(bad, "\n"))
	case n == 0:
		rep.Failf(checkName, "%s is empty", dir)
	case len(scripts) > 0:
		rep.Pass(checkName, "%d binaries are %s (%d arch-neutral script(s): %s)",
			n-len(scripts), want, len(scripts), strings.Join(scripts, ", "))
	default:
		rep.Pass(checkName, "%d binaries are %s", n, want)
	}
	return rep
}

func scanDir(dir string, check func(string) error) (n int, scripts []string, bad []string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, nil, []string{fmt.Sprintf("  %s: %v", dir, err)}
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		st, err := os.Stat(p)
		if err != nil {
			bad = append(bad, fmt.Sprintf("  %s: %v", p, err))
			continue
		}
		if st.IsDir() || !st.Mode().IsRegular() {
			continue
		}
		n++
		// binutils installs embedspu as /bin/sh for powerpc. A script has no
		// architecture, so it can never be a wrong-arch binary.
		if isScript(p) {
			scripts = append(scripts, e.Name())
			continue
		}
		if err := check(p); err != nil {
			bad = append(bad, "  "+err.Error())
		}
	}
	return n, scripts, bad
}

func isScript(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [2]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	return magic[0] == '#' && magic[1] == '!'
}

func failed(rep *Report, name string) bool {
	for _, c := range rep.Checks {
		if c.Name == name && !c.OK {
			return true
		}
	}
	return false
}

func mustExec(rep *Report, p string) bool {
	st, err := os.Stat(p)
	if err != nil {
		rep.Fail("exists:"+filepath.Base(p), err, "%s is required to run the probe suite", p)
		return false
	}
	if !isExec(st) {
		rep.Failf("exists:"+filepath.Base(p), "%s is not executable (mode %s)", p, st.Mode())
		return false
	}
	return true
}

func timeit(rep *Report) func() {
	start := time.Now()
	return func() { rep.Dur = time.Since(start) }
}
