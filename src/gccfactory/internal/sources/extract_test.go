package sources

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeTar builds <dir>/probe.tar.gz containing probe-1/{README,sub/x.c}.
func makeTar(t *testing.T, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	root := filepath.Join(dir, "stage", "probe-1")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"README":  "hello\n",
		"sub/x.c": "int main(void){return 0;}\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	arch := filepath.Join(dir, "probe-1.tar.gz")
	cmd := exec.Command("tar", "-czf", arch, "-C", filepath.Join(dir, "stage"), "probe-1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tar: %v\n%s", err, out)
	}
	return arch
}

func probeSource() Source {
	return Source{Name: "probe", Version: "1", File: "probe-1.tar.gz",
		URLs:   []string{"https://example.invalid/probe-1.tar.gz"},
		SHA256: strings.Repeat("a", 64), TopDir: "probe-1"}
}

func TestExtractStripsTopDir(t *testing.T) {
	tmp := t.TempDir()
	arch := makeTar(t, tmp)
	dst := filepath.Join(tmp, "out", "probe")

	if err := Extract(context.Background(), arch, dst, probeSource()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"README", "sub/x.c"} {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(want))); err != nil {
			t.Errorf("%s missing after extract: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "probe-1")); err == nil {
		t.Error("top-level dir was not stripped")
	}
}

func TestExtractRefusesExistingDst(t *testing.T) {
	tmp := t.TempDir()
	arch := makeTar(t, tmp)
	dst := filepath.Join(tmp, "out")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Extract(context.Background(), arch, dst, probeSource()); err == nil {
		t.Fatal("Extract clobbered an existing directory")
	}
}

// A failed extraction must not leave a half-built tree that a later run would
// mistake for a good source tree.
func TestExtractCleansUpOnFailure(t *testing.T) {
	tmp := t.TempDir()
	bad := filepath.Join(tmp, "not-a-tarball.tar.gz")
	if err := os.WriteFile(bad, []byte("definitely not a tar archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(tmp, "out")
	if err := Extract(context.Background(), bad, dst, probeSource()); err == nil {
		t.Fatal("expected an error extracting garbage")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("%s was left behind after a failed extract", dst)
	}
}

func TestExtractRejectsRawSource(t *testing.T) {
	s := probeSource()
	s.Raw, s.TopDir = true, ""
	err := Extract(context.Background(), "/dev/null", filepath.Join(t.TempDir(), "x"), s)
	if err == nil || !strings.Contains(err.Error(), "raw file") {
		t.Fatalf("want a raw-file error, got %v", err)
	}
}
