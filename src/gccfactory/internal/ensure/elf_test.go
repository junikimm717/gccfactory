package ensure

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// synthELF writes a minimal but structurally valid ELF file for tr. It is not
// runnable; it exists so the header/program-header reader can be exercised for
// every class/endianness combination without a cross toolchain.
func synthELF(t *testing.T, path string, tr triple.Triple, interp string) {
	t.Helper()
	machine, class, data, ok := tr.ELF()
	if !ok {
		t.Fatalf("no ELF identity for %s", tr)
	}
	var bo binary.ByteOrder = binary.LittleEndian
	if data == elfDataBE {
		bo = binary.BigEndian
	}
	ehsize, phentsize := 52, 32
	if class == elfClass64 {
		ehsize, phentsize = 64, 56
	}
	phnum := 0
	if interp != "" {
		phnum = 1
	}
	interpOff := ehsize + phentsize*phnum
	buf := make([]byte, interpOff)
	copy(buf, "\x7fELF")
	buf[4], buf[5], buf[6], buf[7] = class, data, 1, 0
	bo.PutUint16(buf[16:], 2) // ET_EXEC
	bo.PutUint16(buf[18:], machine)
	if class == elfClass64 {
		bo.PutUint64(buf[32:], uint64(ehsize)) // e_phoff
		bo.PutUint16(buf[52:], uint16(ehsize))
		bo.PutUint16(buf[54:], uint16(phentsize))
		bo.PutUint16(buf[56:], uint16(phnum))
	} else {
		bo.PutUint32(buf[28:], uint32(ehsize))
		bo.PutUint16(buf[40:], uint16(ehsize))
		bo.PutUint16(buf[42:], uint16(phentsize))
		bo.PutUint16(buf[44:], uint16(phnum))
	}
	if phnum == 1 {
		ph := buf[ehsize:]
		bo.PutUint32(ph, ptInterp)
		if class == elfClass64 {
			bo.PutUint32(ph[4:], 4) // p_flags = R
			bo.PutUint64(ph[8:], uint64(interpOff))
			bo.PutUint64(ph[32:], uint64(len(interp)+1))
		} else {
			bo.PutUint32(ph[4:], uint32(interpOff))
			bo.PutUint32(ph[16:], uint32(len(interp)+1))
		}
		buf = append(buf, []byte(interp)...)
		buf = append(buf, 0)
	}
	if err := os.WriteFile(path, buf, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestReadELFSynthetic(t *testing.T) {
	dir := t.TempDir()
	for _, name := range triple.Known {
		tr := triple.MustParse(name)
		for _, interp := range []string{"", tr.DynamicLinker()} {
			kind := "-static"
			if interp != "" {
				kind = "-dyn"
			}
			p := filepath.Join(dir, name+kind)
			synthELF(t, p, tr, interp)
			info, err := ReadELF(p)
			if err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			m, c, d, _ := tr.ELF()
			if info.Machine != m || info.Class != c || info.Data != d {
				t.Errorf("%s: got %s want %s", name, info, ELFID(c, d, m))
			}
			if info.Static != (interp == "") {
				t.Errorf("%s: Static=%v with interp %q", name, info.Static, interp)
			}
			if info.Interp != interp {
				t.Errorf("%s: Interp=%q want %q", name, info.Interp, interp)
			}
			wantStatic := interp == ""
			if err := ExpectELF(p, tr, &wantStatic); err != nil {
				t.Errorf("ExpectELF(%s): %v", name, err)
			}
		}
	}
}

func TestExpectELFMismatch(t *testing.T) {
	dir := t.TempDir()
	x86 := triple.MustParse("x86_64-linux-musl")
	arm := triple.MustParse("aarch64-linux-musl")
	rv32 := triple.MustParse("riscv32-linux-musl")
	rv64 := triple.MustParse("riscv64-linux-musl")

	armBin := filepath.Join(dir, "aarch64.exe")
	synthELF(t, armBin, arm, "")
	rv32Bin := filepath.Join(dir, "riscv32.exe")
	synthELF(t, rv32Bin, rv32, "")
	dynBin := filepath.Join(dir, "x86_64.dyn")
	synthELF(t, dynBin, x86, "/lib/ld-linux-x86-64.so.2")
	text := filepath.Join(dir, "script.sh")
	if err := os.WriteFile(text, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	yes, no := true, false
	tests := []struct {
		name       string
		path       string
		tr         triple.Triple
		wantStatic *bool
		want       string
	}{
		{"machine", armBin, x86, nil,
			armBin + ": expected ELF64/LE/EM_X86_64(62), got ELF64/LE/EM_AARCH64(183)"},
		{"class", rv32Bin, rv64, nil,
			rv32Bin + ": expected ELF64/LE/EM_RISCV(243), got ELF32/LE/EM_RISCV(243)"},
		{"wrong-libc", dynBin, x86, nil,
			dynBin + ": expected interpreter /lib/ld-musl-x86_64.so.1 (musl for x86_64-linux-musl), got /lib/ld-linux-x86-64.so.2"},
		{"want-static", dynBin, x86, &yes,
			dynBin + ": expected a static binary (no PT_INTERP), got dynamic with interpreter /lib/ld-linux-x86-64.so.2"},
		{"want-dynamic", armBin, arm, &no,
			armBin + ": expected a dynamic binary (PT_INTERP present), got static"},
		{"not-elf", text, x86, nil,
			"read elf " + text + ": not an ELF file"},
		{"missing", filepath.Join(dir, "nope"), x86, nil,
			"read elf " + filepath.Join(dir, "nope") + ": stat"},
		{"directory", dir, x86, nil,
			"read elf " + dir + ": is a directory"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ExpectELF(tc.path, tc.tr, tc.wantStatic)
			if err == nil {
				t.Fatalf("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error text:\n  got:  %s\n  want: contains %s", err, tc.want)
			}
		})
	}
}

// TestReadELFRealBinaries cross-compiles a tiny Go program for a few real
// architectures; these are genuine linker output, not synthetic headers.
func TestReadELFRealBinaries(t *testing.T) {
	if testing.Short() {
		t.Skip("cross builds are slow")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\nfunc main() { println(\"hi\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probe\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for goarch, name := range map[string]string{
		"amd64": "x86_64-linux-musl",
		"arm64": "aarch64-linux-musl",
		"386":   "i386-linux-musl",
		"s390x": "s390x-linux-musl",
	} {
		tr := triple.MustParse(name)
		out := filepath.Join(dir, goarch+".bin")
		cmd := exec.Command(goBin, "build", "-o", out, ".")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+goarch, "CGO_ENABLED=0", "GOFLAGS=")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("cannot cross build for %s: %v\n%s", goarch, err, b)
		}
		static := true
		if err := ExpectELF(out, tr, &static); err != nil {
			t.Errorf("%s: %v", goarch, err)
		}
		info, err := ReadELF(out)
		if err != nil {
			t.Fatal(err)
		}
		if info.Type != 2 { // ET_EXEC
			t.Errorf("%s: e_type = %d, want ET_EXEC", goarch, info.Type)
		}
		// A binary for one arch must not validate as another.
		other := triple.MustParse("powerpc64-linux-musl")
		if err := ExpectELF(out, other, nil); err == nil {
			t.Errorf("%s validated as %s", goarch, other)
		}
	}
}

func TestExpectELFLike(t *testing.T) {
	dir := t.TempDir()
	x86 := triple.MustParse("x86_64-linux-musl")
	arm := triple.MustParse("aarch64-linux-musl")
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	synthELF(t, a, x86, "")
	synthELF(t, b, arm, "")
	ref, err := ReadELF(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := ExpectELFLike(a, ref, nil); err != nil {
		t.Fatalf("self: %v", err)
	}
	err = ExpectELFLike(b, ref, nil)
	if err == nil || !strings.Contains(err.Error(), "expected ELF64/LE/EM_X86_64(62) (same as "+a+")") {
		t.Fatalf("got %v", err)
	}
}
