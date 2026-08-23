package ensure

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// execRunner is a minimal real Runner. It is also the exact shape of the
// adapter internal/cli must provide around *core.Runner.
type execRunner struct{ t *testing.T }

func (e execRunner) Output(ctx context.Context, c Cmd) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.Args[0], c.Args[1:]...)
	cmd.Dir = c.Dir
	cmd.Env = os.Environ()
	for k, v := range c.EnvAdd {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return cmd.CombinedOutput()
}

func (e execRunner) Run(ctx context.Context, c Cmd) error {
	out, err := e.Output(ctx, c)
	if err != nil {
		e.t.Logf("%s: %v\n%s", c.Name, err, out)
	}
	return err
}

func TestProbesNamed(t *testing.T) {
	got, err := ProbesNamed([]string{"tls", "hello"})
	if err != nil || len(got) != 2 || got[0].Name != "tls" {
		t.Fatalf("got %v, %v", got, err)
	}
	if all, err := ProbesNamed(nil); err != nil || len(all) != len(Probes()) {
		t.Fatalf("nil selector must mean everything")
	}
	_, err = ProbesNamed([]string{"nope"})
	if err == nil || !strings.Contains(err.Error(), "unknown probe \"nope\"") {
		t.Fatalf("got %v", err)
	}
}

// TestProbeSourcesParse type-checks every probe source with the native
// compiler. It is the cheap way to catch a typo without a cross toolchain.
func TestProbeSourcesParse(t *testing.T) {
	cc, ccErr := exec.LookPath("cc")
	cxx, cxxErr := exec.LookPath("c++")
	if ccErr != nil || cxxErr != nil {
		t.Skip("no native cc/c++")
	}
	dir := t.TempDir()
	for _, p := range Probes() {
		if err := p.Write(dir); err != nil {
			t.Fatal(err)
		}
		for _, f := range p.Files {
			args := []string{"-fsyntax-only", "-Wall", "-Wextra"}
			tool := cc
			if p.Lang == "c++" {
				tool, args = cxx, append(args, "-std=c++17")
			}
			out, err := exec.Command(tool, append(args, filepath.Join(dir, f))...).CombinedOutput()
			if err != nil {
				t.Errorf("%s: %v\n%s", f, err, out)
			} else if len(out) > 0 {
				t.Logf("%s warnings:\n%s", f, out)
			}
		}
	}
}

// TestNativeToolchainRunsProbes drives the real entry point with a real
// compiler: it proves the harness wiring and that every Want string is what
// the programs actually print.
func TestNativeToolchainRunsProbes(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles and runs the whole probe suite")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no native cc")
	}
	if _, err := exec.LookPath("c++"); err != nil {
		t.Skip("no native c++")
	}
	probes := Probes()
	if runtime.GOOS != "linux" {
		// -ldl only exists on linux; everything else is portable.
		probes = nil
		for _, p := range Probes() {
			if !p.Shared {
				probes = append(probes, p)
			}
		}
	}
	rep := NativeToolchain(context.Background(), execRunner{t}, t.TempDir(), "cc", "c++",
		WithOptLevels("-O2"), WithProbes(probes...))
	if !rep.OK() {
		t.Fatalf("native probe suite failed:\n%s", rep)
	}
	t.Logf("\n%s", rep)
}
