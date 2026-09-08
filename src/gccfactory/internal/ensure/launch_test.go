package ensure

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// A real aarch64 registration, copied from /proc/sys/fs/binfmt_misc on a
// machine with qemu-user-static installed.
const (
	aarch64Magic = "7f454c460201010000000000000000000200b700"
	aarch64Mask  = "ffffffffffffff00fffffffffffffffffeffffff"
)

// A kernel registration beats a qemu binary sitting on disk: only the former
// survives gcc forking cc1, so preferring it is the whole point of the check.
func TestExecRouteOfPrefersBinfmtOverQemuBinary(t *testing.T) {
	dir := withBinfmtDir(t)
	writeBinfmt(t, dir, "qemu-aarch64", "enabled\ninterpreter /usr/bin/qemu-aarch64-static\n"+
		"flags: F\noffset 0\nmagic "+aarch64Magic+"\nmask "+aarch64Mask+"\n")

	qemuDir := t.TempDir()
	qemu := filepath.Join(qemuDir, "qemu-aarch64-static")
	if err := os.WriteFile(qemu, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, detail := ExecRouteOf(triple.MustParse("aarch64-linux-musl"), []string{qemuDir})
	if got != RouteBinfmt {
		t.Fatalf("got %v (%s), want RouteBinfmt", got, detail)
	}
	if !got.Nested() {
		t.Error("a binfmt_misc route must count as nestable")
	}
}

// A qemu binary with no registration is the case that silently breaks a build
// hours in: it runs the gcc driver and then dies when the driver forks cc1.
func TestExecRouteOfQemuOnlyIsNotNestable(t *testing.T) {
	withBinfmtDir(t)

	qemuDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(qemuDir, "qemu-aarch64-static"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // QemuFor searches PATH last

	got, detail := ExecRouteOf(triple.MustParse("aarch64-linux-musl"), []string{qemuDir})
	if got != RouteQemu {
		t.Fatalf("got %v (%s), want RouteQemu", got, detail)
	}
	if got.Nested() {
		t.Error("an explicit qemu launcher only covers the process it is handed")
	}
}

func TestExecRouteOfNoRoute(t *testing.T) {
	withBinfmtDir(t)
	t.Setenv("PATH", t.TempDir())

	got, _ := ExecRouteOf(triple.MustParse("s390x-linux-musl"), []string{t.TempDir()})
	if got != RouteNone || got.Nested() {
		t.Fatalf("got %v, want RouteNone", got)
	}
}

// The build machine's own architecture needs neither a registration nor qemu,
// which is why a name-based check reports a false failure for it.
func TestExecRouteOfNativeNeedsNothing(t *testing.T) {
	withBinfmtDir(t)
	t.Setenv("PATH", t.TempDir())

	self, ok := nativeIdentity()
	if !ok {
		t.Skip("cannot read /proc/self/exe")
	}
	var native string
	for _, raw := range triple.Known {
		tr := triple.MustParse(raw)
		if m, c, d, ok := tr.ELF(); ok && m == self.Machine && c == self.Class && d == self.Data {
			native = raw
			break
		}
	}
	if native == "" {
		t.Skipf("no triple matches this machine (%s)", self)
	}
	got, detail := ExecRouteOf(triple.MustParse(native), nil)
	if got != RouteNative {
		t.Fatalf("%s: got %v (%s), want RouteNative", native, got, detail)
	}
}

func TestBinfmtRemedyNamesTheQemuBinaries(t *testing.T) {
	got := BinfmtRemedy([]string{"powerpc64le-linux-musl", "riscv32-linux-musl"})
	for _, want := range []string{"ppc64le,riscv32", "binfmt_misc"} {
		if !strings.Contains(got, want) {
			t.Errorf("remedy must mention %q:\n%s", want, got)
		}
	}
}

func TestQemuLaunchDoesNotSetLibraryPath(t *testing.T) {
	qemu := fakeQemu(t, t.TempDir(), "qemu-x86_64-static")
	sys := t.TempDir()
	got, ok := qemuLaunch(qemu, sys, false)
	if !ok {
		t.Fatal("qemuLaunch rejected a real binary")
	}
	if _, ok := got.env["LD_LIBRARY_PATH"]; ok {
		t.Fatal("qemu -L must not paper over host musl with LD_LIBRARY_PATH")
	}
	if got.env["QEMU_LD_PREFIX"] != sys {
		t.Fatalf("QEMU_LD_PREFIX=%q, want %q", got.env["QEMU_LD_PREFIX"], sys)
	}
}

func TestIsolatedEnvironDropsLoaderVars(t *testing.T) {
	t.Setenv("LD_PRELOAD", "/tmp/poison.so")
	t.Setenv("LD_LIBRARY_PATH", "/tmp/poison")
	t.Setenv("LD_DEBUG", "libs")
	t.Setenv("QEMU_LD_PREFIX", "/tmp/wrong")
	t.Setenv("PATH", "/bin")
	got := isolatedEnviron(map[string]string{"QEMU_LD_PREFIX": "/sys"})
	joined := strings.Join(got, "\n")
	for _, bad := range []string{"LD_PRELOAD=", "LD_LIBRARY_PATH=", "LD_DEBUG="} {
		if strings.Contains(joined, bad+"/tmp") || strings.Contains(joined, bad+"libs") {
			t.Errorf("isolated env still has host %s:\n%s", bad, joined)
		}
	}
	if !strings.Contains(joined, "QEMU_LD_PREFIX=/sys") {
		t.Errorf("extra QEMU_LD_PREFIX lost:\n%s", joined)
	}
	if strings.Count(joined, "QEMU_LD_PREFIX=") != 1 {
		t.Errorf("host QEMU_LD_PREFIX leaked:\n%s", joined)
	}
}

func TestSetTargetRunUsesSysrootLoader(t *testing.T) {
	tr := triple.MustParse("x86_64-linux-musl")
	if !isNative(tr) {
		t.Skip("native-loader route is same-arch only")
	}
	sys := t.TempDir()
	lib := filepath.Join(sys, "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, "libc.so"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("libc.so", filepath.Join(sys, tr.DynamicLinker())); err != nil {
		t.Fatal(err)
	}
	h := &harness{r: execRunner{t}, rep: NewReport("t"), opts: newOptions(nil), work: t.TempDir()}
	h.setTargetRun(context.Background(), fakeQemu(t, t.TempDir(), "qemu-x86_64-static"), sys, tr)
	want := []string{filepath.Join(lib, "libc.so"), "--library-path", lib}
	if strings.Join(h.runLoader, " ") != strings.Join(want, " ") {
		t.Fatalf("runLoader=%q, want %q", h.runLoader, want)
	}
	if len(h.runPrefix) != 0 {
		t.Fatalf("same-arch must not use qemu -L, got %q", h.runPrefix)
	}
}

func TestHijackEnv(t *testing.T) {
	for _, k := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "QEMU_LD_PREFIX"} {
		if !hijackEnv(k) {
			t.Errorf("%s must be stripped from probe runs", k)
		}
	}
	if hijackEnv("PATH") || hijackEnv("HOME") {
		t.Error("PATH/HOME must survive isolation")
	}
}

// Debian/Ubuntu musl writes /etc/ld-musl-<arch>.path. Same-arch qemu -L loads
// our interpreter but then the loader reads that host file and searches
// /lib/<triple>, which has no libstdc++. This is the x86_64 verify failure
// from 20260908T003833Z: C probes passed, C++ dynamic probes died.
func TestHostMuslPathFileHijacksWithoutLibraryPath(t *testing.T) {
	self, ok := nativeIdentity()
	if !ok {
		t.Skip("cannot read /proc/self/exe")
	}
	var native triple.Triple
	for _, raw := range triple.Known {
		tr := triple.MustParse(raw)
		if m, c, d, ok := tr.ELF(); ok && m == self.Machine && c == self.Class && d == self.Data {
			native = tr
			break
		}
	}
	if native.Raw == "" {
		t.Skip("no triple matches this machine")
	}
	pathFile := "/etc/ld-musl-" + native.QemuName() + ".path"
	if _, err := os.Stat(pathFile); err != nil {
		t.Skipf("no %s (host musl package is not installed)", pathFile)
	}
	muslGcc, err := exec.LookPath("musl-gcc")
	if err != nil {
		t.Skip("musl-gcc not on PATH")
	}
	qemu, err := exec.LookPath("qemu-" + native.QemuName() + "-static")
	if err != nil {
		t.Skip("qemu-<arch>-static not on PATH")
	}

	sys := t.TempDir()
	lib := filepath.Join(sys, "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	hostLibc := "/lib/" + native.Raw + "/libc.so"
	if _, err := os.Stat(hostLibc); err != nil {
		t.Skipf("no %s", hostLibc)
	}
	libcDst := filepath.Join(lib, "libc.so")
	in, err := os.ReadFile(hostLibc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(libcDst, in, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("libc.so", filepath.Join(lib, filepath.Base(native.DynamicLinker()))); err != nil {
		t.Fatal(err)
	}

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "foo.c"), []byte("int foo(void) { return 42; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.c"), []byte("int foo(void); int main(void) { return foo() != 42; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	foo := filepath.Join(lib, "libfoo.so")
	probe := filepath.Join(src, "probe")
	if out, err := exec.Command(muslGcc, "-shared", "-fPIC", "-o", foo, filepath.Join(src, "foo.c")).CombinedOutput(); err != nil {
		t.Fatalf("musl-gcc libfoo: %v\n%s", err, out)
	}
	if out, err := exec.Command(muslGcc, "-o", probe, filepath.Join(src, "main.c"), "-L"+lib, "-lfoo").CombinedOutput(); err != nil {
		t.Fatalf("musl-gcc probe: %v\n%s", err, out)
	}

	bare := exec.Command(qemu, "-L", sys, probe)
	bare.Dir = src
	if out, err := bare.CombinedOutput(); err == nil {
		t.Skipf("this qemu isolates /etc; host musl path file is not a factor:\n%s", out)
	}

	h := &harness{r: execRunner{t}, rep: NewReport("t"), opts: newOptions(nil), work: t.TempDir()}
	h.setTargetRun(context.Background(), qemu, sys, native)
	if len(h.runLoader) == 0 {
		t.Fatal("same-arch verify must use the sysroot loader, not qemu -L")
	}
	_, _, combined, argv, err := h.runBinary(context.Background(), "hijack", src, probe)
	if err != nil {
		t.Fatalf("sysroot loader must find libfoo despite host %s: %v\nargv=%q\n%s", pathFile, err, argv, combined)
	}
}
