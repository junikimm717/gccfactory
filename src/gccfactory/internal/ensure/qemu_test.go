package ensure

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

func touchExec(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
}

func TestQemuFor(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	touchExec(t, filepath.Join(a, "qemu-aarch64"), 0o755)
	touchExec(t, filepath.Join(a, "qemu-x86_64"), 0o644)        // present but not executable
	touchExec(t, filepath.Join(b, "qemu-x86_64-static"), 0o755) // the fallback name
	touchExec(t, filepath.Join(b, "qemu-riscv64"), 0o755)
	t.Setenv("PATH", filepath.Join(root, "empty"))

	aarch64 := triple.MustParse("aarch64-linux-musl")
	x86 := triple.MustParse("x86_64-linux-musl")
	s390 := triple.MustParse("s390x-linux-musl")
	rv := triple.MustParse("riscv64-linux-musl")

	if got, err := QemuFor(aarch64, []string{a, b}); err != nil || got != filepath.Join(a, "qemu-aarch64") {
		t.Errorf("aarch64: %q %v", got, err)
	}
	if got, err := QemuFor(x86, []string{a, b}); err != nil || got != filepath.Join(b, "qemu-x86_64-static") {
		t.Errorf("x86_64 must fall through the non-executable one: %q %v", got, err)
	}
	// A direct path to a binary is accepted too.
	if got, err := QemuFor(rv, []string{filepath.Join(b, "qemu-riscv64")}); err != nil || got != filepath.Join(b, "qemu-riscv64") {
		t.Errorf("direct path: %q %v", got, err)
	}
	// ...but only if its name is one qemu could use for that target.
	if _, err := QemuFor(x86, []string{filepath.Join(b, "qemu-riscv64")}); err == nil {
		t.Error("qemu-riscv64 must not be accepted for x86_64")
	}

	_, err := QemuFor(s390, []string{a, b})
	if err == nil {
		t.Fatal("expected a failure for s390x")
	}
	for _, frag := range []string{"qemu-s390x or qemu-s390x-static", a, b, "$PATH"} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("error should mention %q: %v", frag, err)
		}
	}
}

func TestQemuNames(t *testing.T) {
	got := QemuNames(triple.MustParse("powerpc64le-linux-musl"))
	if got[0] != "qemu-ppc64le" || got[1] != "qemu-ppc64le-static" {
		t.Fatalf("%v", got)
	}
}

// With no way to execute target binaries we still compile everything and
// check the ELF identity, but say plainly that nothing was executed.
func TestCrossToolchainNoEmulator(t *testing.T) {
	host := triple.MustParse("x86_64-linux-musl")
	target := triple.MustParse("i386-linux-musl")
	f := newFakeToolchain(t, host, target)
	r := newFakeRunner(t, f)

	rep := CrossToolchain(context.Background(), r, t.TempDir(), f.prefix, target, Emulator{},
		WithOptLevels("-O0"), WithProbes(Probes()[0]))
	var built, ran int
	for _, c := range rep.Checks {
		if strings.HasPrefix(c.Name, "probe:") {
			if !c.Skipped {
				ran++
			}
			built++
			if !strings.Contains(c.Detail, "not run: no emulator") {
				t.Errorf("%s: detail = %q", c.Name, c.Detail)
			}
		}
	}
	if built != 2 || ran != 0 { // hello, dynamic + static
		t.Fatalf("built %d ran %d\n%s", built, ran, rep)
	}
	if rep.OK() {
		t.Fatalf("a missing qemu must be reported as a failure:\n%s", rep)
	}
}

// The BUILD-arch check is ignored because the machine running the test is
// not necessarily the one the fake prefix pretends to be.
func TestCrossToolchainMatrix(t *testing.T) {
	host := triple.MustParse("x86_64-linux-musl")
	target := triple.MustParse("arm-linux-musleabi")
	f := newFakeToolchain(t, host, target)
	r := newFakeRunner(t, f)

	rep := CrossToolchain(context.Background(), r, t.TempDir(), f.prefix, target, QemuEmulator("/usr/bin/qemu-arm"),
		WithOptLevels("-O2"), WithProbes(Probes()...))
	for _, c := range rep.Failures() {
		if c.Name != "bin-elf" && c.Name != "tooldir-elf" && c.Name != "gcc-nm-lto" {
			t.Errorf("unexpected failure %s:\n%s", c.Name, rep)
		}
	}
	var probes int
	for _, c := range rep.Checks {
		if strings.HasPrefix(c.Name, "probe:") {
			probes++
		}
	}
	if probes != 22 { // 10 probes x (dynamic+static) + dlopen and static-pie, one cell each
		t.Fatalf("ran %d probe cells\n%s", probes, rep)
	}
	// 32-bit targets must link libatomic for the 64-bit atomics probe.
	var sawLibatomic bool
	for _, c := range r.cmds {
		if has(c.Args, "-latomic") {
			sawLibatomic = true
		}
	}
	if !sawLibatomic {
		t.Error("the atomic probe must pass -latomic on a 32-bit target")
	}
}
