package ensure

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// harness compiles and runs the probe suite with one particular way of
// invoking a compiler and one particular way of running the result.
type harness struct {
	r    Runner
	rep  *Report
	work string
	opts *options

	cc  []string          // argv prefix that invokes the C compiler
	cxx []string          // argv prefix that invokes the C++ compiler
	env map[string]string // env for compile commands (QEMU_LD_PREFIX for the host qemu)

	target *triple.Triple // nil: do not assert the ELF identity of the output
	prefix string         // toolchain prefix, "" for a native compiler
	launch []string       // argv prefix that runs a HOST binary (qemu ...), nil if native

	runPrefix   []string          // argv prefix to execute a produced binary
	runFallback []string          // as runPrefix, but invoking the musl loader directly
	runEnv      map[string]string
	// How to describe a runFallback retry, "" when it is the normal path.
	fallbackNote string
	// Start a program that has a PT_INTERP via runFallback rather than
	// discovering it failed. See Emulator.LoaderForDynamic.
	loaderForDynamic bool
	norun            bool // compile and inspect only
}

func (h *harness) tool(name string) []string {
	return append(append([]string(nil), h.launch...), Tool(h.prefix, *h.target, name))
}

func (h *harness) compiler(lang string) []string {
	if lang == "c++" {
		return h.cxx
	}
	return h.cc
}

// It is the first thing that fails when a toolchain cannot execute at all, so
// its failure detail carries the qemu/binfmt hint.
func (h *harness) version(ctx context.Context, name string, argv []string, wants ...string) bool {
	dir := h.mkdir("preflight")
	start := time.Now()
	out, err := h.exec(ctx, name, dir, argv, h.env, h.opts.runTimeout)
	if err != nil {
		h.rep.Add(Check{Name: name, Err: err, Dur: time.Since(start),
			Detail: execDetail(argv, dir, out)})
		return false
	}
	for _, w := range wants {
		if !strings.Contains(string(out), w) {
			h.rep.Add(Check{Name: name, Dur: time.Since(start),
				Err:    fmt.Errorf("output does not mention %q", w),
				Detail: execDetail(argv, dir, out)})
			return false
		}
	}
	h.rep.Add(Check{Name: name, OK: true, Dur: time.Since(start), Detail: firstLine(strings.TrimSpace(string(out)))})
	return true
}

// A bare "as" means gcc found nothing and will silently use whatever is on
// $PATH -- the toolchain is not self-contained -- which is worth one decisive
// check instead of a wall of mysterious probe failures.
func (h *harness) progNames(ctx context.Context, prefix string, t triple.Triple) bool {
	dir := h.mkdir("preflight")
	tooldir := ToolDir(prefix, t)
	for _, tool := range toolDirFatal {
		name := "gcc-finds-" + tool
		argv := append(append([]string(nil), h.cc...), "-print-prog-name="+tool)
		start := time.Now()
		out, err := h.exec(ctx, name, dir, argv, h.env, h.opts.runTimeout)
		got := strings.TrimSpace(string(out))
		if err != nil {
			h.rep.Add(Check{Name: name, Err: err, Dur: time.Since(start), Detail: execDetail(argv, dir, out)})
			return false
		}
		if !filepath.IsAbs(got) {
			h.rep.Add(Check{Name: name, Dur: time.Since(start),
				Err: fmt.Errorf("gcc resolved %s to the bare name %q instead of an absolute path", tool, got),
				Detail: fmt.Sprintf("cmd: %s\nthe toolchain is not self-contained: gcc will exec whatever %q"+
					" is first on $PATH, which is the build machine's (expect errors like"+
					" \"as: unrecognized option '--64'\").\nit must resolve inside %s",
					shJoin(argv), tool, tooldir)})
			return false
		}
		detail := got
		if !strings.HasPrefix(got, prefix+string(filepath.Separator)) {
			detail = got + " (warning: outside the toolchain prefix " + prefix + ")"
		}
		h.rep.Add(Check{Name: name, OK: true, Dur: time.Since(start), Detail: detail})
	}
	return true
}

// preflightCompile proves the compiler can fork its own sub-processes (cc1,
// as, collect2) in this environment before we blame individual probes.
func (h *harness) preflightCompile(ctx context.Context) bool {
	dir := h.mkdir("preflight")
	p := Probes()[0]
	if err := p.Write(dir); err != nil {
		h.rep.Fail("preflight-compile", err, "cannot write probe sources to %s", dir)
		return false
	}
	argv := append(append([]string(nil), h.cc...), "-O0", p.Files[0], "-o", "preflight")
	start := time.Now()
	out, err := h.exec(ctx, "preflight-compile", dir, argv, h.env, h.opts.compileTimeout)
	if err != nil {
		h.rep.Add(Check{Name: "preflight-compile", Err: err, Dur: time.Since(start),
			Detail: execDetail(argv, dir, out) +
				"\nthe compiler could not produce an object; no probe can pass until this works"})
		return false
	}
	h.rep.Pass("preflight-compile", "%s compiles a hello world", strings.Join(h.cc, " "))
	return true
}

func (h *harness) runAll(ctx context.Context, t triple.Triple) {
	// The gcc-ar/-flto path first: it is one cell and it explains a whole
	// class of matrix failures.
	for _, p := range h.opts.probes {
		if p.Name == "lto" {
			h.ltoArchive(ctx, p)
			break
		}
	}
	for _, p := range h.opts.probes {
		if p.Skip != nil {
			if why := p.Skip(t); why != "" {
				h.rep.Skip("probe:"+p.Name, "%s", why)
				continue
			}
		}
		for _, opt := range h.opts.optLevels {
			h.probe(ctx, p, t, opt, false)
			if p.Static && h.opts.static && !p.Shared {
				h.probe(ctx, p, t, opt, true)
			}
		}
	}
}

// probe is one (probe, -O level, link mode) cell: compile, assert the ELF
// identity of what came out, run it, and require its stdout verbatim.
func (h *harness) probe(ctx context.Context, p Probe, t triple.Triple, opt string, static bool) {
	mode := "dynamic"
	if static {
		mode = "static"
	}
	name := fmt.Sprintf("probe:%s/%s/%s", p.Name, opt, mode)
	step := fmt.Sprintf("probe-%s-%s-%s", sanitize(p.Name), sanitize(opt), mode)
	start := time.Now()
	fail := func(err error, detail string) {
		h.rep.Add(Check{Name: name, Err: err, Detail: detail, Dur: time.Since(start)})
	}

	dir := h.mkdir(step)
	if err := p.Write(dir); err != nil {
		fail(err, "cannot write probe sources to "+dir)
		return
	}
	libs, mains := p.SharedSources()

	if len(libs) > 0 {
		so := strings.TrimSuffix(libs[0], filepath.Ext(libs[0])) + ".so"
		argv := append(append([]string(nil), h.compiler(p.Lang)...), opt, "-fPIC", "-shared")
		argv = append(argv, libs...)
		argv = append(argv, "-o", so)
		out, err := h.exec(ctx, step+"-so", dir, argv, h.env, h.opts.compileTimeout)
		if err != nil {
			fail(err, execDetail(argv, dir, out))
			return
		}
		if h.target != nil {
			if err := ExpectELF(filepath.Join(dir, so), *h.target, nil); err != nil {
				fail(err, "the shared library the toolchain produced is for the wrong machine")
				return
			}
		}
	}

	argv := append(append([]string(nil), h.compiler(p.Lang)...), opt, "-Wall")
	if p.Lang == "c++" {
		argv = append(argv, "-std=c++17")
	}
	if static {
		argv = append(argv, "-static")
	}
	argv = append(argv, mains...)
	argv = append(argv, "-o", "probe")
	argv = append(argv, p.Flags(t)...)

	out, err := h.exec(ctx, step, dir, argv, h.env, h.opts.compileTimeout)
	if err != nil {
		fail(err, execDetail(argv, dir, out))
		return
	}

	bin := filepath.Join(dir, "probe")
	info, elfErr := ReadELF(bin)
	desc := info.String()
	if h.target != nil {
		if elfErr != nil {
			fail(elfErr, "compiled with: "+shJoin(argv)+"\ncwd: "+dir)
			return
		}
		wantStatic := static || p.NoInterp
		if err := ExpectELF(bin, *h.target, &wantStatic); err != nil {
			fail(err, "compiled with: "+shJoin(argv)+"\ncwd: "+dir)
			return
		}
	} else if elfErr != nil {
		// BUILD binaries are not necessarily ELF (e.g. on a mac dev box).
		st, err := os.Stat(bin)
		if err != nil {
			fail(err, "the compiler exited 0 but produced nothing\ncompiled with: "+shJoin(argv))
			return
		}
		desc = fmt.Sprintf("%d bytes", st.Size())
	}

	if h.norun {
		h.rep.Add(Check{Name: name, OK: true, Skipped: true, Dur: time.Since(start),
			Detail: "built ok (" + desc + ") but not run: no emulator"})
		return
	}

	stdout, degraded, out, runArgv, err := h.runBinary(ctx, step, dir, "./probe")
	if err != nil {
		fail(err, execDetail(runArgv, dir, out)+"\nbinary: "+desc)
		return
	}
	if stdout != p.Want {
		fail(fmt.Errorf("stdout mismatch"), diffDetail(p.Want, stdout)+"\n"+execDetail(runArgv, dir, out))
		return
	}
	h.rep.Add(Check{Name: name, OK: true, Dur: time.Since(start), Detail: joinDetail(degraded, desc)})
}

// runBinary executes a freshly built target binary. If `qemu -L <sysroot>`
// cannot find the interpreter -- which happens when the sysroot's ld-musl is an
// absolute symlink -- it retries by handing the binary to the musl loader
// directly and says so, so a toolchain that only works that way is visibly
// degraded instead of quietly passing.
func (h *harness) runBinary(ctx context.Context, step, dir, prog string) (stdout, degraded string, out []byte, argv []string, err error) {
	argv = append(append([]string(nil), h.runPrefix...), prog)
	// Deciding from the ELF beats parsing the failure: a bare exec of a
	// dynamic program fails with plain ENOENT, which is indistinguishable
	// from the program itself being absent.
	if h.loaderForDynamic && len(h.runFallback) > 0 && hasInterp(filepath.Join(dir, prog)) {
		argv = append(append([]string(nil), h.runFallback...), prog)
	}
	stdout, out, err = h.execCapture(ctx, step+"-run", dir, argv, h.runEnv, h.opts.runTimeout)
	if err == nil || len(h.runFallback) == 0 || !loaderMissing(out) {
		return stdout, "", out, argv, err
	}
	alt := append(append([]string(nil), h.runFallback...), prog)
	altOut, altCombined, altErr := h.execCapture(ctx, step+"-run-ldso", dir, alt, h.runEnv, h.opts.runTimeout)
	if altErr != nil {
		return stdout, "", append(out, altCombined...), argv, err
	}
	var note string
	if h.fallbackNote != "" {
		note = h.fallbackNote + shJoin(h.runFallback)
	}
	return altOut, note, altCombined, alt, nil
}

func joinDetail(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, " | ")
}

func hasInterp(path string) bool {
	info, err := ReadELF(path)
	return err == nil && info.Interp != ""
}

// loaderMissing recognises qemu failing to open the musl interpreter, e.g.
// "qemu-x86_64-static: Could not open '/lib/ld-musl-x86_64.so.1': No such file".
func loaderMissing(out []byte) bool {
	s := string(out)
	return strings.Contains(s, "ld-musl") &&
		(strings.Contains(s, "Could not open") || strings.Contains(s, "No such file"))
}

// ltoArchive proves the gcc-ar/gcc-ranlib/gcc-nm path over an -flto object,
// which is exactly where a missing liblto_plugin.so would bite.
func (h *harness) ltoArchive(ctx context.Context, p Probe) {
	if h.prefix == "" || h.target == nil {
		return
	}
	start := time.Now()
	dir := h.mkdir("lto-archive")
	fail := func(err error, detail string) {
		h.rep.Add(Check{Name: "lto-archive", Err: err, Detail: detail, Dur: time.Since(start)})
	}
	if err := p.Write(dir); err != nil {
		fail(err, "cannot write probe sources to "+dir)
		return
	}
	steps := [][]string{
		append(append([]string(nil), h.cc...), "-flto", "-O2", "-c", "ltohelp.c", "-o", "a.o"),
		append(h.tool("gcc-ar"), "rcs", "liba.a", "a.o"),
		append(h.tool("gcc-ranlib"), "liba.a"),
	}
	for i, argv := range steps {
		out, err := h.exec(ctx, fmt.Sprintf("lto-archive-%d", i), dir, argv, h.env, h.opts.compileTimeout)
		if err != nil {
			fail(err, execDetail(argv, dir, out))
			return
		}
	}

	nmArgv := append(h.tool("gcc-nm"), "liba.a")
	nmOut, err := h.exec(ctx, "lto-archive-nm", dir, nmArgv, h.env, h.opts.compileTimeout)
	switch {
	case err != nil:
		h.rep.Add(Check{Name: "gcc-nm-lto", Err: err, Detail: execDetail(nmArgv, dir, nmOut)})
	case !strings.Contains(string(nmOut), "helper_add"):
		h.rep.Add(Check{Name: "gcc-nm-lto",
			Err: fmt.Errorf("gcc-nm did not list helper_add in the LTO archive"),
			Detail: execDetail(nmArgv, dir, nmOut) +
				"\nthe linker plugin is not being loaded, so LTO objects look empty to ar/nm"})
	default:
		h.rep.Pass("gcc-nm-lto", "gcc-nm reads symbols out of an -flto archive")
	}

	link := append(append([]string(nil), h.cc...), "-flto", "-O2", "lto.c", "liba.a", "-o", "probe")
	out, err := h.exec(ctx, "lto-archive-link", dir, link, h.env, h.opts.compileTimeout)
	if err != nil {
		fail(err, execDetail(link, dir, out))
		return
	}
	wantStatic := false
	if err := ExpectELF(filepath.Join(dir, "probe"), *h.target, &wantStatic); err != nil {
		fail(err, "linked with: "+shJoin(link))
		return
	}
	if h.norun {
		h.rep.Add(Check{Name: "lto-archive", OK: true, Skipped: true, Dur: time.Since(start),
			Detail: "built ok but not run: no emulator"})
		return
	}
	stdout, degraded, runOut, runArgv, err := h.runBinary(ctx, "lto-archive", dir, "./probe")
	if err != nil {
		fail(err, execDetail(runArgv, dir, runOut))
		return
	}
	if stdout != p.Want {
		fail(fmt.Errorf("stdout mismatch"), diffDetail(p.Want, stdout)+"\n"+execDetail(runArgv, dir, runOut))
		return
	}
	h.rep.Add(Check{Name: "lto-archive", OK: true, Dur: time.Since(start),
		Detail: joinDetail(degraded, "gcc-ar/gcc-ranlib/gcc-nm + -flto link")})
}

func (h *harness) mkdir(name string) string {
	dir := filepath.Join(h.work, name)
	_ = os.RemoveAll(dir)
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// A non-zero exit is a normal outcome here (it becomes a failed Check), so
// Runner.Output is used.
func (h *harness) exec(ctx context.Context, step, dir string, argv []string, env map[string]string, timeout time.Duration) ([]byte, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return h.r.Output(ctx, Cmd{Name: step, Dir: dir, Args: argv, EnvAdd: env})
}

// execCapture runs argv with stdout redirected to a file so the comparison
// against Probe.Want sees pure stdout, while stderr still reaches the log.
func (h *harness) execCapture(ctx context.Context, step, dir string, argv []string, env map[string]string, timeout time.Duration) (stdout string, combined []byte, err error) {
	const outFile = "probe.stdout"
	if !haveShell() {
		combined, err = h.exec(ctx, step, dir, argv, env, timeout)
		return string(combined), combined, err
	}
	sh := []string{shellPath, "-c", "exec " + shJoin(argv) + " > " + shQuote(outFile)}
	combined, err = h.exec(ctx, step, dir, sh, env, timeout)
	b, rerr := os.ReadFile(filepath.Join(dir, outFile))
	if rerr != nil && err == nil {
		return "", combined, fmt.Errorf("cannot read captured stdout: %w", rerr)
	}
	return string(b), combined, err
}

const shellPath = "/bin/sh"

func haveShell() bool {
	st, err := os.Stat(shellPath)
	return err == nil && !st.IsDir() && isExec(st)
}

func execDetail(argv []string, dir string, out []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "cmd: %s\ncwd: %s", shJoin(argv), dir)
	if hint := execHint(out); hint != "" {
		b.WriteString("\nhint: " + hint)
	}
	if t := tail(out, 40); t != "" {
		b.WriteString("\noutput:\n" + indent(t, "  "))
	}
	return b.String()
}

// execHint recognises the two failure modes that are environmental rather than
// toolchain bugs, because they otherwise look like a mysterious compiler crash.
func execHint(out []byte) string {
	s := string(out)
	switch {
	case strings.Contains(s, "Exec format error"), strings.Contains(s, "cannot execute binary file"):
		return "a foreign binary was exec'd without qemu: gcc forks cc1/as/ld, which needs binfmt_misc" +
			" registered for this architecture (or a qemu-user that re-execs itself)"
	case strings.Contains(s, "No such file or directory") && strings.Contains(s, "ld-musl"):
		return "the musl loader was not found: pass -L <sysroot> / QEMU_LD_PREFIX=<sysroot> to qemu"
	case strings.Contains(s, "Could not open") && strings.Contains(s, "qemu"):
		return "qemu could not open the binary; check the path and that it is a file"
	}
	return ""
}

func diffDetail(want, got string) string {
	wl, gl := strings.SplitAfter(want, "\n"), strings.SplitAfter(got, "\n")
	var b strings.Builder
	b.WriteString("stdout does not match the expected output:")
	n := len(wl)
	if len(gl) > n {
		n = len(gl)
	}
	for i := 0; i < n; i++ {
		w, g := at(wl, i), at(gl, i)
		if w == g {
			continue
		}
		fmt.Fprintf(&b, "\n  line %d: want %q\n           got %q", i+1, w, g)
		if i > 6 {
			b.WriteString("\n  ... (further differences elided)")
			break
		}
	}
	return b.String()
}

func at(ss []string, i int) string {
	if i < len(ss) {
		return ss[i]
	}
	return ""
}

func tail(b []byte, n int) string {
	s := strings.TrimRight(string(b), "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append([]string{fmt.Sprintf("... (%d earlier lines omitted)", len(lines)-n)}, lines[len(lines)-n:]...)
	}
	return strings.Join(lines, "\n")
}

func indent(s, with string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = with + lines[i]
	}
	return strings.Join(lines, "\n")
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == '+':
			b.WriteString("x")
		default:
			b.WriteRune('_')
		}
	}
	return strings.TrimLeft(b.String(), "-")
}

func shQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("-_./=:+,@", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func shJoin(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = shQuote(a)
	}
	return strings.Join(out, " ")
}
