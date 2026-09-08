//go:build linux

package ensure

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// Fuzz input:
//
//	byte 0  flags
//	byte 1  unused (kept so mutations stay aligned)
//	byte 2  pathLen
//	rest    path-file body (and extra lines)
const (
	fuzzLdPreload = 1 << 0
	fuzzLdLibPath = 1 << 1
	fuzzPathFile  = 1 << 2
	fuzzEmptyPath = 1 << 3
)

type fuzzHijack struct {
	sysroot string
	probe   string
	poison  string
	poisonSO string
	target  triple.Triple
}

var (
	fuzzOnce   sync.Once
	fuzzArt    fuzzHijack
	fuzzArtErr error
)

func buildFuzzHijackArtifacts() error {
	fuzzOnce.Do(func() {
		muslGcc, err := exec.LookPath("musl-gcc")
		if err != nil {
			fuzzArtErr = err
			return
		}
		target := triple.MustParse("x86_64-linux-musl")
		if !isNative(target) {
			fuzzArtErr = errSkip("not same-arch x86_64")
			return
		}
		hostLibc := "/lib/" + target.Raw + "/libc.so"
		if _, err := os.Stat(hostLibc); err != nil {
			fuzzArtErr = err
			return
		}
		root, err := os.MkdirTemp("", "gccfactory-fuzz-hijack-")
		if err != nil {
			fuzzArtErr = err
			return
		}
		sys := filepath.Join(root, "sysroot")
		lib := filepath.Join(sys, "lib")
		poison := filepath.Join(root, "poison")
		if err := os.MkdirAll(lib, 0o755); err != nil {
			fuzzArtErr = err
			return
		}
		if err := os.MkdirAll(poison, 0o755); err != nil {
			fuzzArtErr = err
			return
		}
		in, err := os.ReadFile(hostLibc)
		if err != nil {
			fuzzArtErr = err
			return
		}
		if err := os.WriteFile(filepath.Join(lib, "libc.so"), in, 0o755); err != nil {
			fuzzArtErr = err
			return
		}
		if err := os.Symlink("libc.so", filepath.Join(sys, target.DynamicLinker())); err != nil {
			fuzzArtErr = err
			return
		}

		src := filepath.Join(root, "src")
		if err := os.MkdirAll(src, 0o755); err != nil {
			fuzzArtErr = err
			return
		}
		write := func(name, body string) {
			if fuzzArtErr != nil {
				return
			}
			fuzzArtErr = os.WriteFile(filepath.Join(src, name), []byte(body), 0o644)
		}
		write("foo_sys.c", "const char *foo(void) { return \"SYSROOT\"; }\n")
		write("foo_pois.c", "const char *foo(void) { return \"POISON\"; }\n")
		write("main.c", "#include <stdio.h>\nconst char *foo(void);\nint main(void) { puts(foo()); return 0; }\n")
		if fuzzArtErr != nil {
			return
		}
		sysSO := filepath.Join(lib, "libfoo.so")
		poisSO := filepath.Join(poison, "libfoo.so")
		if out, err := exec.Command(muslGcc, "-shared", "-fPIC", "-o", sysSO, filepath.Join(src, "foo_sys.c")).CombinedOutput(); err != nil {
			fuzzArtErr = errSkip("musl-gcc libfoo sys: " + string(out))
			return
		}
		if out, err := exec.Command(muslGcc, "-shared", "-fPIC", "-o", poisSO, filepath.Join(src, "foo_pois.c")).CombinedOutput(); err != nil {
			fuzzArtErr = errSkip("musl-gcc libfoo poison: " + string(out))
			return
		}
		probe := filepath.Join(root, "probe")
		if out, err := exec.Command(muslGcc, "-o", probe, filepath.Join(src, "main.c"), "-L"+lib, "-lfoo").CombinedOutput(); err != nil {
			fuzzArtErr = errSkip("musl-gcc probe: " + string(out))
			return
		}
		fuzzArt = fuzzHijack{sysroot: sys, probe: probe, poison: poison, poisonSO: poisSO, target: target}
	})
	return fuzzArtErr
}

type skipErr string

func errSkip(s string) error { return skipErr(s) }
func (s skipErr) Error() string { return string(s) }

func FuzzTargetProbeSysrootIsolation(f *testing.F) {
	if err := buildFuzzHijackArtifacts(); err != nil {
		f.Skip(err.Error())
	}
	enc := func(flags byte, path string) []byte {
		b := []byte{flags, 0, byte(len(path))}
		return append(b, path...)
	}
	f.Add(enc(0, ""))
	f.Add(enc(fuzzLdPreload, ""))
	f.Add(enc(fuzzLdLibPath, ""))
	f.Add(enc(fuzzLdPreload|fuzzLdLibPath, ""))
	f.Add(enc(fuzzPathFile, "/lib/x86_64-linux-musl\n"))
	f.Add(enc(fuzzPathFile, fuzzArt.poison+"\n"))
	f.Add(enc(fuzzEmptyPath, ""))
	f.Add(enc(fuzzPathFile|fuzzLdPreload|fuzzLdLibPath, "/lib/x86_64-linux-musl\n/lib64\n"))
	f.Add(enc(fuzzPathFile, "lib/x86_64-linux-musl\n../poison\n"))
	f.Add(enc(fuzzPathFile, "/nonexistent\n"+fuzzArt.poison+"\n"))

	f.Fuzz(func(t *testing.T, in []byte) {
		if err := buildFuzzHijackArtifacts(); err != nil {
			t.Skip(err.Error())
		}
		if len(in) < 3 {
			return
		}
		flags := in[0]
		pathLen := int(in[2])
		body := in[3:]
		if pathLen > len(body) {
			pathLen = len(body)
		}
		pathFile := string(body[:pathLen]) + string(body[pathLen:])
		if flags&fuzzEmptyPath != 0 {
			pathFile = ""
		}

		caseDir := t.TempDir()
		sys := filepath.Join(caseDir, "sysroot")
		if err := copyDir(fuzzArt.sysroot, sys); err != nil {
			t.Fatal(err)
		}
		etc := filepath.Join(sys, "etc")
		if flags&(fuzzPathFile|fuzzEmptyPath) != 0 {
			if err := os.MkdirAll(etc, 0o755); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(etc, "ld-musl-"+fuzzArt.target.LdsoArch()+".path")
			if err := os.WriteFile(name, []byte(pathFile), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		t.Setenv("LD_PRELOAD", "")
		t.Setenv("LD_LIBRARY_PATH", "")
		if flags&fuzzLdPreload != 0 {
			t.Setenv("LD_PRELOAD", fuzzArt.poisonSO)
		}
		if flags&fuzzLdLibPath != 0 {
			t.Setenv("LD_LIBRARY_PATH", fuzzArt.poison)
		}

		h := &harness{r: execRunner{t}, rep: NewReport("fuzz"), opts: newOptions(nil), work: caseDir}
		qemu, _ := exec.LookPath("qemu-" + fuzzArt.target.QemuName() + "-static")
		h.setTargetRun(context.Background(), qemu, sys, fuzzArt.target)
		if len(h.runLoader) == 0 {
			t.Fatalf("production launch forgot the sysroot loader")
		}

		stdout, _, combined, _, err := h.runBinary(context.Background(), "fuzz", caseDir, fuzzArt.probe)
		out := stdout + string(combined)
		if strings.Contains(out, "POISON") {
			t.Fatalf("loaded poison libfoo flags=%#x path=%q\n%s", flags, pathFile, out)
		}
		if err != nil {
			t.Fatalf("isolated loader failed flags=%#x: %v\n%s", flags, err, out)
		}
		if !strings.Contains(stdout, "SYSROOT") {
			t.Fatalf("probe did not use sysroot libfoo flags=%#x stdout=%q", flags, stdout)
		}
	})
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(p string, st os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		w := filepath.Join(dst, rel)
		if st.Mode()&os.ModeSymlink != 0 {
			tgt, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(w), 0o755); err != nil {
				return err
			}
			return os.Symlink(tgt, w)
		}
		if st.IsDir() {
			return os.MkdirAll(w, st.Mode())
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(w, b, st.Mode())
	})
}
