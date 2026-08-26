package pack

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/ensure"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// The suffix is what the consumer keys off, so the arch-only comparison has to
// hold for the pairs that look like exceptions.
func TestKindComparesArchitectureOnly(t *testing.T) {
	cases := []struct {
		host, target, want string
	}{
		{"x86_64-linux-musl", "x86_64-linux-musl", "native"},
		{"x86_64-linux-musl", "aarch64-linux-musl", "cross"},
		{"aarch64-linux-musl", "aarch64-linux-musl", "native"},
		{"aarch64-linux-musl", "x86_64-linux-musl", "cross"},
		// i386 is a different architecture from x86_64, however closely related.
		{"x86_64-linux-musl", "i386-linux-musl", "cross"},
		{"i386-linux-musl", "x86_64-linux-musl", "cross"},
		// The ABI suffix is not part of the architecture: both arm targets are
		// native on an arm host, and each is native to the other's host.
		{"arm-linux-musleabi", "arm-linux-musleabi", "native"},
		{"arm-linux-musleabi", "arm-linux-musleabihf", "native"},
		{"arm-linux-musleabihf", "arm-linux-musleabi", "native"},
		{"x86_64-linux-musl", "arm-linux-musleabihf", "cross"},
		// powerpc64le and powerpc64 are distinct arch strings.
		{"powerpc64-linux-musl", "powerpc64le-linux-musl", "cross"},
		{"powerpc64le-linux-musl", "powerpc64le-linux-musl", "native"},
	}
	for _, c := range cases {
		h, tg := triple.MustParse(c.host), triple.MustParse(c.target)
		if got := Kind(h, tg); got != c.want {
			t.Errorf("Kind(%s, %s) = %s, want %s", c.host, c.target, got, c.want)
		}
		wantTop := c.target + "-" + c.want
		if got := TopDir(h, tg); got != wantTop {
			t.Errorf("TopDir(%s, %s) = %s, want %s", c.host, c.target, got, wantTop)
		}
		if got := FileName(h, tg); got != wantTop+".tgz" {
			t.Errorf("FileName(%s, %s) = %s, want %s.tgz", c.host, c.target, got, wantTop)
		}
	}
}

// fakeTree stands in for a toolchain: a plain file, an executable, a symlink,
// and a nested directory. No compiler is involved.
func fakeTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mkdir := func(p string) {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p string, mode os.FileMode, body string) {
		if err := os.WriteFile(filepath.Join(root, p), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	mkdir("bin")
	mkdir("lib/gcc")
	write("bin/fake-gcc", 0o755, "#!/bin/sh\n")
	write("bin/make", 0o755, "#!/bin/sh\n")
	write("lib/gcc/libgcc.a", 0o644, "not really an archive")
	if err := os.Symlink("fake-gcc", filepath.Join(root, "bin", "fake-cc")); err != nil {
		t.Fatal(err)
	}
	return root
}

func packFake(t *testing.T, src, dstDir, top string) (string, Result) {
	t.Helper()
	dst := filepath.Join(dstDir, top+".tgz")
	res, err := Write(dst, src, top, triple.MustParse("x86_64-linux-musl"))
	if err != nil {
		t.Fatal(err)
	}
	return dst, res
}

func TestTarballShape(t *testing.T) {
	src := fakeTree(t)
	const top = "fake-linux-musl-cross"
	dst, res := packFake(t, src, t.TempDir(), top)

	a, err := Inspect(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Tops) != 1 || a.Tops[0] != top {
		t.Fatalf("want exactly one top-level dir %q, got %v", top, a.Tops)
	}
	// The consumer extracts with -C and then walks into <top>/bin, so the
	// prefix has to be on every entry.
	for name := range a.Entries {
		if name != top && !bytes.HasPrefix([]byte(name), []byte(top+"/")) {
			t.Errorf("entry %q escapes the top-level directory", name)
		}
	}

	link, ok := a.Has(top + "/bin/fake-cc")
	if !ok {
		t.Fatal("symlink bin/fake-cc missing")
	}
	if link.Type != tar.TypeSymlink || link.Link != "fake-gcc" {
		t.Errorf("bin/fake-cc: type %q -> %q, want a symlink to fake-gcc", link.Type, link.Link)
	}

	gcc, ok := a.Has(top + "/bin/fake-gcc")
	if !ok {
		t.Fatal("bin/fake-gcc missing")
	}
	if !gcc.Executable() {
		t.Errorf("bin/fake-gcc lost its executable bit: mode %04o", gcc.Mode)
	}
	lib, ok := a.Has(top + "/lib/gcc/libgcc.a")
	if !ok {
		t.Fatal("lib/gcc/libgcc.a missing")
	}
	if lib.Executable() {
		t.Errorf("lib/gcc/libgcc.a gained an executable bit: mode %04o", lib.Mode)
	}
	if res.Files != 3 || res.Symlinks != 1 {
		t.Errorf("counted %d files / %d symlinks, want 3 / 1", res.Files, res.Symlinks)
	}
}

func TestHardlinksStayLinks(t *testing.T) {
	src := fakeTree(t)
	if err := os.Link(filepath.Join(src, "bin", "fake-gcc"), filepath.Join(src, "bin", "fake-gcc-1.2.3")); err != nil {
		t.Skipf("hardlinks unavailable here: %v", err)
	}
	const top = "fake-linux-musl-cross"
	dst, res := packFake(t, src, t.TempDir(), top)
	if res.Hardlinks != 1 {
		t.Fatalf("got %d hardlink entries, want 1", res.Hardlinks)
	}
	a, err := Inspect(dst)
	if err != nil {
		t.Fatal(err)
	}
	// Sorted order makes "fake-gcc" the stored copy and the suffixed name the link.
	e, ok := a.Has(top + "/bin/fake-gcc-1.2.3")
	if !ok {
		t.Fatal("bin/fake-gcc-1.2.3 missing")
	}
	if e.Type != tar.TypeLink || e.Link != top+"/bin/fake-gcc" {
		t.Errorf("got type %q -> %q, want a hardlink to %s/bin/fake-gcc", e.Type, e.Link, top)
	}
}

func TestDeterministicBytes(t *testing.T) {
	src := fakeTree(t)
	const top = "fake-linux-musl-native"
	outA, outB := t.TempDir(), t.TempDir()

	pathA, resA := packFake(t, src, outA, top)
	pathB, resB := packFake(t, src, outB, top)

	a, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("same tree packed twice differs: %d vs %d bytes", len(a), len(b))
	}
	if resA.SHA256 != resB.SHA256 {
		t.Fatalf("sha256 differs: %s vs %s", resA.SHA256, resB.SHA256)
	}
	// Repacking over an existing tarball must also be a no-op in content.
	if _, err := Write(pathA, src, top, triple.MustParse("x86_64-linux-musl")); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, again) {
		t.Fatal("repacking in place changed the bytes")
	}
}

// An interrupted or failed pack must not leave anything that looks like a
// finished tarball.
func TestWriteLeavesNothingBehindOnFailure(t *testing.T) {
	out := t.TempDir()
	dst := filepath.Join(out, "nope.tgz")
	if _, err := Write(dst, filepath.Join(t.TempDir(), "does-not-exist"), "nope", triple.MustParse("x86_64-linux-musl")); err == nil {
		t.Fatal("packing a missing tree should fail")
	}
	ents, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("output dir should be empty, got %v", ents)
	}
}

// The unprefixed names are the whole reason mimux's cross/build has a
// makeinstall step; shipping them has to be safe on a real bin/ layout.
func TestToolAliases(t *testing.T) {
	const tri = "aarch64-linux-musl"
	tt := triple.MustParse(tri)
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range ensure.BinutilsTools {
		if err := os.WriteFile(filepath.Join(src, "bin", tri+"-"+tool), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// make is shipped unprefixed already: an alias must never shadow it.
	if err := os.WriteFile(filepath.Join(src, "bin", "make"), []byte("real make"), 0o755); err != nil {
		t.Fatal(err)
	}
	const top = tri + "-cross"
	dst := filepath.Join(t.TempDir(), top+".tgz")
	if _, err := Write(dst, src, top, tt); err != nil {
		t.Fatal(err)
	}
	a, err := Inspect(dst)
	if err != nil {
		t.Fatal(err)
	}
	for name, tool := range AliasTools() {
		e, ok := a.Has(top + "/bin/" + name)
		if !ok {
			t.Errorf("bin/%s missing", name)
			continue
		}
		want := tri + "-" + tool
		if e.Type != tar.TypeSymlink || e.Link != want {
			t.Errorf("bin/%s: type %q -> %q, want a symlink to %s", name, e.Type, e.Link, want)
		}
		if _, ok := a.Has(top + "/bin/" + want); !ok {
			t.Errorf("bin/%s points at %s, which is not in the tarball", name, want)
		}
	}
	if e, ok := a.Has(top + "/bin/make"); !ok || e.Type != tar.TypeReg || e.Size != int64(len("real make")) {
		t.Errorf("bin/make was replaced by an alias: %+v", e)
	}
}
