package ensure

import (
	"os"
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
