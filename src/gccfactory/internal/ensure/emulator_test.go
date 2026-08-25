package ensure

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

func TestQemuLaunchTellsQemuTheSysroot(t *testing.T) {
	rv := triple.MustParse("riscv64-linux-musl")
	em := QemuEmulator("/usr/bin/qemu-riscv64-static")

	argv, env := em.Launch("/sr", rv, false)
	if got, want := strings.Join(argv, " "), "/usr/bin/qemu-riscv64-static -L /sr"; got != want {
		t.Errorf("dynamic launch = %q, want %q", got, want)
	}
	if env["QEMU_LD_PREFIX"] != "/sr" {
		t.Errorf("dynamic launch env = %v, want QEMU_LD_PREFIX=/sr", env)
	}

	argv, env = em.Launch("/sr", rv, true)
	if got, want := strings.Join(argv, " "), "/usr/bin/qemu-riscv64-static"; got != want {
		t.Errorf("static launch = %q, want %q", got, want)
	}
	if len(env) != 0 {
		t.Errorf("static launch must not set a sysroot env, got %v", env)
	}
}

// One launch prefix runs every probe, static and dynamic alike, so a wrapper
// must not bake the musl loader into it: `<loader> ./probe` on a static binary
// dies with "Not a valid dynamic program". The loader belongs in the retry.
func TestWrapperLaunchStaysBareSoStaticProbesRun(t *testing.T) {
	target := triple.MustParse("riscv64-linux-musl")
	f := newFakeToolchain(t, triple.MustParse("x86_64-linux-musl"), target)
	sysroot := Sysroot(f.prefix, target)
	ld := LoaderPath(sysroot, target)
	if ld == "" {
		t.Fatal("fixture has no loader; the rest of this test is meaningless")
	}
	em := WrapperEmulator([]string{"docker", "run", "--rm", "alpine"})

	for _, static := range []bool{false, true} {
		argv, env := em.Launch(sysroot, target, static)
		if got, want := strings.Join(argv, " "), "docker run --rm alpine"; got != want {
			t.Errorf("Launch(static=%v) = %q, want %q", static, got, want)
		}
		if _, ok := env["QEMU_LD_PREFIX"]; ok {
			t.Errorf("Launch(static=%v): a wrapper is not qemu, QEMU_LD_PREFIX must not be set", static)
		}
	}

	want := "docker run --rm alpine " + ld + " --library-path " + filepath.Join(sysroot, "lib")
	if got := strings.Join(em.Loader(sysroot, target), " "); got != want {
		t.Errorf("Loader = %q, want %q", got, want)
	}
	// A wrapper always reaches a dynamic program this way, so saying so on
	// every probe would be noise, not information.
	if note := em.FallbackNote(); note != "" {
		t.Errorf("wrapper FallbackNote = %q, want empty", note)
	}
	if note := QemuEmulator("qemu-riscv64").FallbackNote(); note == "" {
		t.Error("a qemu loader retry is a real degradation and must be reported")
	}
}

func TestZeroEmulatorIsUnusable(t *testing.T) {
	var em Emulator
	if em.Usable() {
		t.Fatal("the zero Emulator must not claim it can run anything")
	}
	if argv, _ := em.Launch("/sr", triple.MustParse("x86_64-linux-musl"), false); argv != nil {
		t.Errorf("Launch = %v, want nil", argv)
	}
	if got := em.Loader("/sr", triple.MustParse("x86_64-linux-musl")); got != nil {
		t.Errorf("Loader = %v, want nil", got)
	}
}

func TestSpecPrefersWrapperOverQemu(t *testing.T) {
	rv := triple.MustParse("riscv64-linux-musl")
	s := EmulatorSpec{Qemu: "/usr/bin/qemu-%s-static", Wrapper: []string{"myrunner"}}
	em, err := s.For(rv)
	if err != nil {
		t.Fatal(err)
	}
	if em.Qemu {
		t.Error("a wrapper must not be treated as qemu")
	}
	if got, want := em.String(), "myrunner"; got != want {
		t.Errorf("For = %q, want %q", got, want)
	}
}

func TestSpecExpandsPlaceholders(t *testing.T) {
	s := EmulatorSpec{
		Dist:    "/var/tmp/dist",
		Wrapper: []string{"docker", "run", "-v", "{dist}:{dist}", "--platform", "{platform}", "img", "--for={triple}", "--arch={arch}"},
	}
	em, err := s.For(triple.MustParse("aarch64-linux-musl"))
	if err != nil {
		t.Fatal(err)
	}
	want := "docker run -v /var/tmp/dist:/var/tmp/dist --platform linux/arm64 img --for=aarch64-linux-musl --arch=aarch64"
	if got := em.String(); got != want {
		t.Errorf("expanded = %q, want %q", got, want)
	}
}

// riscv32 has no OCI platform. Substituting an empty string would build a
// silently wrong `--platform ` argument, so it has to be an error.
func TestSpecRejectsUndefinedPlatform(t *testing.T) {
	s := EmulatorSpec{Wrapper: []string{"docker", "run", "--platform", "{platform}", "img"}}
	if _, err := s.For(triple.MustParse("riscv32-linux-musl")); err == nil {
		t.Fatal("want an error for {platform} on riscv32, got none")
	}
	if _, err := s.For(triple.MustParse("riscv64-linux-musl")); err != nil {
		t.Fatalf("riscv64 does have a platform: %v", err)
	}
}

func TestEmptySpecYieldsUnusableEmulator(t *testing.T) {
	em, err := EmulatorSpec{}.For(triple.MustParse("x86_64-linux-musl"))
	if err != nil {
		t.Fatal(err)
	}
	if em.Usable() {
		t.Fatal("no qemu and no wrapper must not produce a usable emulator")
	}
}

// The point of the whole abstraction: with no qemu anywhere, a wrapper still
// gets the probe binaries executed, and it is the wrapper's argv that runs.
func TestCrossToolchainExecutesProbesThroughTheWrapper(t *testing.T) {
	target := triple.MustParse("riscv64-linux-musl")
	f := newFakeToolchain(t, triple.MustParse("x86_64-linux-musl"), target)
	r := newFakeRunner(t, f)

	em := WrapperEmulator([]string{"docker", "run", "--rm", "--platform", "linux/riscv64", "img"})
	rep := CrossToolchain(context.Background(), r, t.TempDir(), f.prefix, target, em, WithOptLevels("-O2"))
	if !rep.OK() {
		t.Fatalf("wrapper run should verify clean:\n%s", rep)
	}

	var runs int
	want := "docker run --rm --platform linux/riscv64 img "
	for _, c := range r.cmds {
		if !strings.HasSuffix(c.Name, "-run") {
			continue
		}
		runs++
		// Some probes are launched through `sh -c exec ...`, so the wrapper
		// is not always argv[0]; what matters is that it is what launches.
		if got := strings.Join(c.Args, " "); !strings.Contains(got, want) {
			t.Fatalf("probe %s ran as %q, want %q to launch it", c.Name, got, want)
		}
	}
	if runs == 0 {
		t.Fatal("no probe was executed at all; the wrapper proved nothing")
	}
}
