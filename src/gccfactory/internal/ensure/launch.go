package ensure

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// launchOpt is one candidate way of executing a binary built for another
// architecture.
type launchOpt struct {
	argv []string
	env  map[string]string
	why  string
}

// Not memoised: BinfmtDir is a handful of small files, registrations can change
// under a long run, and a live read keeps the survey honest.
func binfmtEntries() []BinfmtEntry {
	e, _ := Binfmt()
	return e
}

var (
	selfOnce sync.Once
	selfELF  ELFInfo
	selfOK   bool
)

// nativeIdentity is the ELF identity of the running process, which is by
// construction one this kernel can execute without help.
func nativeIdentity() (ELFInfo, bool) {
	selfOnce.Do(func() {
		info, err := ReadELF("/proc/self/exe")
		selfELF, selfOK = info, err == nil
	})
	return selfELF, selfOK
}

// isNative reports whether t has the ELF identity of the running process, which
// is by construction one this kernel executes without help.
func isNative(t triple.Triple) bool {
	m, c, d, ok := t.ELF()
	if !ok {
		return false
	}
	self, sok := nativeIdentity()
	return sok && self.Machine == m && self.Class == c && self.Data == d
}

// directRoute names the reason a plain exec of a t binary works. It is only
// called once one has actually succeeded, so an identity we cannot attribute is
// still a fact worth printing rather than a contradiction.
func directRoute(t triple.Triple) string {
	if isNative(t) {
		return "native"
	}
	if e, ok := BinfmtFor(t, binfmtEntries()); ok {
		return fmt.Sprintf("binfmt_misc %q -> %s (flags %q)", e.Name, e.Interpreter, e.Flags)
	}
	return "accepted by the kernel directly (32-bit compat, or a handler we did not recognise)"
}

// Route is how this machine can execute a binary for some architecture. The
// order matters: only RouteNative and RouteBinfmt are kernel-level, and only a
// kernel-level route survives one foreign process exec'ing another, which is
// what gcc does when it forks cc1/as/ld.
type Route int

const (
	RouteNone Route = iota
	RouteNative
	RouteBinfmt
	RouteQemu
)

// Nested reports whether a process started this way can itself exec another
// binary of the same architecture.
func (r Route) Nested() bool { return r == RouteNative || r == RouteBinfmt }

func (r Route) String() string {
	switch r {
	case RouteNative:
		return "native"
	case RouteBinfmt:
		return "binfmt_misc"
	case RouteQemu:
		return "qemu only"
	}
	return "none"
}

// ExecRouteOf reports, without running anything, how a binary for t would be
// executed here. It is advisory: the authoritative answer is to attempt the
// exec, which is what the toolchain checks do. Its job is to turn a failure, or
// a pre-build survey, into something a user can act on.
func ExecRouteOf(t triple.Triple, qemuSearch []string) (Route, string) {
	if isNative(t) {
		return RouteNative, "this kernel runs " + t.Raw + " binaries directly"
	}
	if e, ok := BinfmtFor(t, binfmtEntries()); ok {
		return RouteBinfmt, fmt.Sprintf("%s -> %s (flags %q)", e.Name, e.Interpreter, e.Flags)
	}
	// No claim is made about 32-bit compat (an x86_64 kernel usually runs i386
	// binaries, an aarch64 one often cannot run arm): guessing wrong here would
	// promise a route that is not there, so the qemu answer stands and the exec
	// itself gets the final word.
	if p, err := QemuFor(t, qemuSearch); err == nil {
		return RouteQemu, p + " (no binfmt_misc registration)"
	}
	return RouteNone, "neither a binfmt_misc registration nor a qemu-user binary for " + t.Raw
}

func ExecRoute(t triple.Triple, qemuSearch []string) string {
	r, detail := ExecRouteOf(t, qemuSearch)
	return r.String() + ": " + detail
}

// BinfmtRemedy is the fix-it text for architectures this machine cannot run.
func BinfmtRemedy(arches []string) string {
	var qemus []string
	for _, a := range arches {
		if t, err := triple.Parse(a); err == nil {
			qemus = append(qemus, t.QemuName())
		}
	}
	return "these need a kernel-level route. gcc forks cc1/as/ld, and a qemu-user\n" +
		"binary alone only ever covers the one process it is handed, so the kernel\n" +
		"has to route the foreign exec: that means a binfmt_misc registration.\n\n" +
		"fix, either:\n" +
		"  docker run --privileged --rm tonistiigi/binfmt --install " + strings.Join(qemus, ",") + "\n" +
		"  or install qemu-user-static / qemu-user-binfmt from your distro\n" +
		"then confirm with:\n" +
		"  ls /proc/sys/fs/binfmt_misc/"
}

// binfmtAdvice is the remedy printed when nothing can execute t.
func binfmtAdvice(t triple.Triple) string {
	return fmt.Sprintf("this machine cannot execute %s binaries (%s).\n%s",
		t.Raw, ExecRoute(t, nil), BinfmtRemedy([]string{t.Raw}))
}

// qemuLaunch returns a launcher that names qemuBin explicitly, or ok=false when
// that file is not there. A dynamic binary also needs the sysroot holding its
// loader, which qemu resolves through the host filesystem.
func qemuLaunch(qemuBin, sysroot string, static bool) (launchOpt, bool) {
	if qemuBin == "" {
		return launchOpt{}, false
	}
	st, err := os.Stat(qemuBin)
	if err != nil || st.IsDir() || !isExec(st) {
		return launchOpt{}, false
	}
	o := launchOpt{argv: []string{qemuBin}, env: map[string]string{}, why: "qemu-user " + qemuBin}
	if !static && sysroot != "" {
		o.argv = append(o.argv, "-L", sysroot)
		o.env["QEMU_LD_PREFIX"] = sysroot
		o.why += " -L " + sysroot
	}
	return o, true
}

// chooseHostLaunch decides how this toolchain's own HOST binaries get run, by
// trying each candidate rather than by looking for a file with a known name.
// Plain exec comes first: it is the only mode in which gcc can fork cc1/as/ld,
// and it is the mode a machine with binfmt_misc but no qemu-user binary on disk
// must use. The qemu launcher is kept as a fallback for the case where the
// binary is dynamic and its loader only exists inside a sysroot.
func (h *harness) chooseHostLaunch(ctx context.Context, gcc, gxx string, host, t triple.Triple, info ELFInfo, hostSysroot, qemuHost string) bool {
	cands := []launchOpt{{env: map[string]string{}, why: "plain exec (" + directRoute(host) + ")"}}
	if q, ok := qemuLaunch(qemuHost, hostSysroot, info.Static); ok {
		cands = append(cands, q)
	}

	dir := h.mkdir("preflight")
	var tried []string
	for _, c := range cands {
		argv := append(append([]string(nil), c.argv...), gcc, "-dumpmachine")
		out, err := h.exec(ctx, "host-launch", dir, argv, c.env, h.opts.runTimeout)
		if err == nil && strings.Contains(string(out), t.Raw) {
			h.cc = append(append([]string(nil), c.argv...), gcc)
			h.cxx = append(append([]string(nil), c.argv...), gxx)
			h.launch = append([]string(nil), c.argv...)
			h.env = c.env
			h.rep.Pass("host-launch", "%s runs via %s", filepath.Base(gcc), c.why)
			return true
		}
		why := firstLine(strings.TrimSpace(string(out)))
		if why == "" && err != nil {
			why = err.Error()
		}
		tried = append(tried, fmt.Sprintf("  tried %s\n    -> %s", c.why, why))
	}

	detail := strings.Join(tried, "\n")
	if !info.Static && hostSysroot == "" {
		detail += fmt.Sprintf("\n  %s is dynamically linked against %s and no host sysroot is known;"+
			" pass ensure.WithHostSysroot(<cross:%s prefix>/%s)", gcc, info.Interp, host, host)
	}
	h.rep.Failf("host-launch", "%s\n%s", detail, binfmtAdvice(host))
	return false
}

// setTargetRun configures how a freshly built TARGET binary is executed. qemu
// is preferred here (unlike the host side) because `-L sysroot` is what makes a
// dynamic target binary find its loader; when no qemu binary exists we exec the
// binary directly and keep the sysroot's loader as the fallback, which is the
// same trick without the emulator.
func (h *harness) setTargetRun(ctx context.Context, qemuTarget, sysroot string, t triple.Triple) {
	ld := LoaderPath(sysroot, t)
	if q, ok := qemuLaunch(qemuTarget, sysroot, false); ok {
		h.runPrefix, h.runEnv = q.argv, q.env
		if ld != "" {
			h.runFallback = []string{qemuTarget, ld}
		}
		h.rep.Pass("target-run-mode", "%s binaries run via %s", t.Raw, q.why)
		return
	}
	if ld != "" {
		h.runFallback = []string{ld}
	}
	// The sysroot's musl loader is itself a TARGET ELF, so it is the one binary
	// available to answer "can this kernel exec this architecture" before the
	// probe suite has been compiled.
	if ld == "" || !h.canExec(ctx, ld) {
		h.norun = true
		h.rep.Failf("target-run-mode", "%s", binfmtAdvice(t))
		return
	}
	h.rep.Pass("target-run-mode", "%s binaries run by plain exec (%s)", t.Raw, directRoute(t))
}

// canExec reports whether the kernel will exec this architecture at all. Any
// outcome other than a refusal of the binary format counts as yes; what prog
// then does with its own exit status is irrelevant.
func (h *harness) canExec(ctx context.Context, prog string) bool {
	out, err := h.exec(ctx, "target-exec-probe", h.mkdir("preflight"), []string{prog}, nil, h.opts.runTimeout)
	if err == nil {
		return true
	}
	s := strings.ToLower(string(out) + " " + err.Error())
	return !strings.Contains(s, "exec format error") && !strings.Contains(s, "enoexec")
}
