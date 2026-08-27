package recipe

import (
	"strings"
	"testing"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/sources"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// If this fails, merging invalidates dist/ for everyone.
func TestArchScopingIsInertWithoutArchPatches(t *testing.T) {
	for _, pkg := range []string{pkgMusl, pkgLinux} {
		s := src(pkg)
		if got := sources.PatchArches(s); len(got) > 0 {
			t.Skipf("%s now ships arch patches %v; this test only pins the empty case", s.Slug(), got)
		}
	}
	for _, tt := range triple.Known {
		tr := triple.MustParse(tt)
		for _, pkg := range []string{pkgMusl, pkgLinux} {
			scoped, plain := srcTreeJobFor(pkg, tr.Arch), srcTreeJob(pkg)
			if scoped.Slug() != plain.Slug() {
				t.Errorf("%s/%s: slug %q != %q", pkg, tr.Arch, scoped.Slug(), plain.Slug())
			}
			if scoped != plain {
				t.Errorf("%s/%s: expected the shared interned job, got a separate one", pkg, tr.Arch)
			}
		}
	}
}

func TestSrcTreeJobForFallsBackWithoutPatches(t *testing.T) {
	j := srcTreeJobFor(pkgMusl, "does-not-exist")
	if j.arch != "" {
		t.Errorf("unknown arch should fall back to the shared tree, got arch=%q", j.arch)
	}
}

func TestArchPatchesRejectedOnAmbiguousPackages(t *testing.T) {
	for _, pkg := range []string{pkgGCC, pkgBinutils, pkgMake} {
		if archScopedPkgs[pkg] {
			t.Fatalf("%s must not be arch-scopable", pkg)
		}
	}
	for _, pkg := range []string{pkgMusl, pkgLinux} {
		if !archScopedPkgs[pkg] {
			t.Errorf("%s should be arch-scopable", pkg)
		}
	}
}

func TestArchPatchSetHashReportsLayoutErrors(t *testing.T) {
	s := src(pkgGCC)
	if len(sources.PatchArches(s)) == 0 {
		t.Skip("gcc ships no arch dirs, so there is no error to surface")
	}
	if h := archPatchSetHash(s, "x86_64"); !strings.HasPrefix(h, "ERROR:") {
		t.Errorf("expected an ERROR: hash for an illegal arch dir, got %q", h)
	}
}

func TestArchPatchesChangeOnlyTheirOwnArch(t *testing.T) {
	s := src(pkgMusl)
	if len(sources.PatchArches(s)) == 0 {
		t.Skip("no arch patches checked in yet")
	}
	base := patchSetHash(s)
	for _, arch := range []string{"s390x", "x86_64"} {
		if h := archPatchSetHash(s, arch); h == base {
			t.Errorf("arch hash for %s collides with the global patch hash", arch)
		}
	}
}

// Two trees whose contents differ may never collide on a key or a directory.
func TestScopedSrcTreeIsDistinct(t *testing.T) {
	s := src(pkgMusl)
	plain := &srcTree{s: s}
	scoped := &srcTree{s: s, arch: "s390x"}

	if plain.Slug() == scoped.Slug() {
		t.Errorf("scoped tree shares a slug with the shared one: %s", plain.Slug())
	}
	if !strings.HasSuffix(scoped.Slug(), "_s390x") {
		t.Errorf("scoped slug should name its arch, got %s", scoped.Slug())
	}
	pin, sin := plain.KeyInputs(), scoped.KeyInputs()
	if sin["arch"] != "s390x" {
		t.Errorf("scoped KeyInputs missing arch, got %q", sin["arch"])
	}
	if _, ok := pin["arch"]; ok {
		t.Error("shared tree must not carry an arch input, or every key today changes")
	}
	if pin["patches"] != sin["patches"] {
		t.Error("the global patch hash must be identical in both; only arch_patches may differ")
	}
}

// At build time this is a poisoned key and a failed srctree job; here it has a name.
func TestNoIllegalArchDirsCheckedIn(t *testing.T) {
	for _, pkg := range []string{
		pkgBinutils, pkgGCC, pkgMusl, pkgGMP, pkgMPFR,
		pkgMPC, pkgISL, pkgLinux, pkgMake, pkgConfigSub,
	} {
		if err := checkArchDirs(src(pkg)); err != nil {
			t.Error(err)
		}
	}
}
