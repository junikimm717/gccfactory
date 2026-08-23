package recipe

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

const testBuild = "aarch64-unknown-linux-gnu"

func allTriples(t *testing.T) []triple.Triple {
	t.Helper()
	out := make([]triple.Triple, 0, len(triple.Known))
	for _, name := range triple.Known {
		tr, err := triple.Parse(name)
		if err != nil {
			t.Fatalf("triple.Parse(%q): %v", name, err)
		}
		out = append(out, tr)
	}
	return out
}

func crossCfg(target triple.Triple) buildCfg {
	return buildCfg{Work: "/w", Stage: "/s", Build: testBuild, Target: target}
}

func canadianCfg(host, target triple.Triple) buildCfg {
	return buildCfg{
		Work: "/w", Stage: "/s", Build: testBuild,
		Host: host, Target: target, Canadian: true,
		CrossH: "/a/crossH", CrossT: "/a/crossT", HostMake: "/a/hostmake",
	}
}

func flagValue(flags []string, name string) (string, bool) {
	prefix := name + "="
	for _, f := range flags {
		if strings.HasPrefix(f, prefix) {
			return strings.TrimPrefix(f, prefix), true
		}
	}
	return "", false
}

func has(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

// The relocatable layout is the single most important invariant in this
// package: an absolute --prefix anywhere bakes the build machine's paths into
// the shipped binaries and the toolchain stops working when it is moved.
func TestPrefixIsEmptyAndLayoutIsRelocatable(t *testing.T) {
	check := func(t *testing.T, what string, flags []string, target triple.Triple) {
		t.Helper()
		v, ok := flagValue(flags, "--prefix")
		if !ok {
			t.Fatalf("%s: no --prefix flag at all", what)
		}
		if v != "" {
			t.Errorf("%s: --prefix must be empty, got %q", what, v)
		}
		if v, ok := flagValue(flags, "--libdir"); !ok || v != "/lib" {
			t.Errorf("%s: --libdir must be /lib, got %q (present=%v)", what, v, ok)
		}
		if v, ok := flagValue(flags, "--with-sysroot"); !ok || v != "/"+target.Raw {
			t.Errorf("%s: --with-sysroot must be /%s, got %q (present=%v)", what, target.Raw, v, ok)
		}
		for _, f := range flags {
			for _, bad := range []string{"--exec-prefix=", "--program-prefix="} {
				if strings.HasPrefix(f, bad) {
					t.Errorf("%s: %s would defeat the relocatable layout", what, f)
				}
			}
		}
	}

	for _, tr := range allTriples(t) {
		t.Run(tr.Raw, func(t *testing.T) {
			c := crossCfg(tr)
			check(t, "cross binutils", binutilsConfig(c), tr)
			check(t, "cross gcc", gccConfig(c), tr)

			host := triple.MustParse("x86_64-linux-musl")
			cc := canadianCfg(host, tr)
			check(t, "canadian binutils", binutilsConfig(cc), tr)
			check(t, "canadian gcc", gccConfig(cc), tr)

			if v, ok := flagValue(muslConfig(c), "--prefix"); !ok || v != "" {
				t.Errorf("musl: --prefix must be empty, got %q (present=%v)", v, ok)
			}
			if v, ok := flagValue(hostMakeConfig(canadianCfg(host, tr)), "--prefix"); !ok || v != "" {
				t.Errorf("hostmake: --prefix must be empty, got %q (present=%v)", v, ok)
			}
		})
	}
}

// A cross build is build == host; only the target differs. Getting this wrong
// makes gcc believe it is doing a canadian build and configure the wrong tools.
func TestCrossConfigHostEqualsBuild(t *testing.T) {
	for _, tr := range allTriples(t) {
		c := crossCfg(tr)
		for what, flags := range map[string][]string{"binutils": binutilsConfig(c), "gcc": gccConfig(c)} {
			b, _ := flagValue(flags, "--build")
			h, _ := flagValue(flags, "--host")
			g, _ := flagValue(flags, "--target")
			if b != testBuild || h != testBuild {
				t.Errorf("%s %s: want --build=--host=%s, got build=%q host=%q", tr, what, testBuild, b, h)
			}
			if g != tr.Raw {
				t.Errorf("%s %s: want --target=%s, got %q", tr, what, tr.Raw, g)
			}
		}
	}
}

// A canadian build is three distinct triples, and all three must be stated
// explicitly: gcc infers the wrong thing from any two of them.
func TestCanadianConfigCarriesAllThreeTriples(t *testing.T) {
	for _, host := range allTriples(t) {
		for _, target := range allTriples(t) {
			c := canadianCfg(host, target)
			for what, flags := range map[string][]string{"binutils": binutilsConfig(c), "gcc": gccConfig(c)} {
				b, okB := flagValue(flags, "--build")
				h, okH := flagValue(flags, "--host")
				g, okG := flagValue(flags, "--target")
				if !okB || !okH || !okG {
					t.Fatalf("%s->%s %s: missing one of --build/--host/--target: %v", host, target, what, flags)
				}
				if b != testBuild || h != host.Raw || g != target.Raw {
					t.Errorf("%s->%s %s: got build=%q host=%q target=%q", host, target, what, b, h, g)
				}
			}
		}
	}
}

// The canadian gcc must never be pointed at the cross build's object-tree
// binutils: those do not exist in this work dir.
func TestCanadianGCCUsesInstalledTargetTools(t *testing.T) {
	c := canadianCfg(triple.MustParse("x86_64-linux-musl"), triple.MustParse("aarch64-linux-musl"))
	flags := gccConfig(c)
	for _, f := range flags {
		if strings.Contains(f, "obj_binutils") {
			t.Errorf("canadian gcc config refers to the cross build's object tree: %s", f)
		}
	}
	// It must point at the per-job staging dir, never straight at cross:<T>:
	// an installed sysroot has no `usr`, and gcc looks for
	// <build-sysroot>/usr/include when it runs fixincludes.
	v, ok := flagValue(flags, "--with-build-sysroot")
	if !ok || v != c.BuildSysroot() {
		t.Errorf("--with-build-sysroot = %q (present=%v), want the staging dir %q", v, ok, c.BuildSysroot())
	}
	if !strings.HasPrefix(v, c.Work+"/") {
		t.Errorf("--with-build-sysroot %q must live inside the job work dir", v)
	}
	if v == filepath.Join(c.CrossT, c.Target.Raw) {
		t.Error("--with-build-sysroot must not be cross:<T>'s installed sysroot: it has no usr/include")
	}
	// Only the last fallback rung names the target tools explicitly.
	if _, ok := flagValue(flags, "CC_FOR_TARGET"); ok {
		t.Errorf("the default canadian rung must not set CC_FOR_TARGET")
	}
	c.ForTargetTools = true
	if v, ok := flagValue(gccConfig(c), "CC_FOR_TARGET"); !ok || v != "/a/crossT/bin/aarch64-linux-musl-gcc" {
		t.Errorf("fallback rung CC_FOR_TARGET = %q (present=%v)", v, ok)
	}
}

// The cross gcc, by contrast, has nothing installed yet and must reach into
// obj_binutils.
func TestCrossGCCUsesObjectTreeBinutils(t *testing.T) {
	c := crossCfg(triple.MustParse("riscv64-linux-musl"))
	flags := gccConfig(c)
	for _, want := range []string{
		"AR_FOR_TARGET=/w/obj_binutils/binutils/ar",
		"AS_FOR_TARGET=/w/obj_binutils/gas/as-new",
		"LD_FOR_TARGET=/w/obj_binutils/ld/ld-new",
		"STRIP_FOR_TARGET=/w/obj_binutils/binutils/strip-new",
		"--with-build-sysroot=/w/obj_sysroot",
	} {
		if !has(flags, want) {
			t.Errorf("cross gcc config is missing %s", want)
		}
	}
}

// The arch-specific decisions live in internal/triple; this proves they
// actually reach the configure line.
func TestGCCConfigIncludesArchFlags(t *testing.T) {
	cases := map[string][]string{
		"riscv64-linux-musl":     {"--with-arch=rv64gc", "--with-abi=lp64d"},
		"powerpc64le-linux-musl": {"--with-abi=elfv2", "--enable-secureplt"},
		"arm-linux-musleabihf":   {"--with-float=hard", "--with-fpu=vfpv3-d16"},
		"s390x-linux-musl":       {"--with-long-double-128"},
	}
	for name, want := range cases {
		tr := triple.MustParse(name)
		flags := gccConfig(crossCfg(tr))
		for _, w := range want {
			if !has(flags, w) {
				t.Errorf("%s: gcc config missing %s\ngot: %s", name, w, strings.Join(flags, " "))
			}
		}
		// The same flags must survive into the canadian build, or the two
		// halves of the toolchain would disagree about the ABI.
		cflags := gccConfig(canadianCfg(triple.MustParse("x86_64-linux-musl"), tr))
		for _, w := range want {
			if !has(cflags, w) {
				t.Errorf("%s: canadian gcc config missing %s", name, w)
			}
		}
	}
}

func TestBinutilsAlwaysDisablesZstd(t *testing.T) {
	// Autodetection would silently differ between the cross build (libzstd-dev
	// present for BUILD) and the canadian build (absent for HOST).
	for _, tr := range allTriples(t) {
		if !has(binutilsConfig(crossCfg(tr)), "--without-zstd") {
			t.Errorf("%s: cross binutils config must pass --without-zstd", tr)
		}
		if !has(binutilsConfig(canadianCfg(triple.MustParse("aarch64-linux-musl"), tr)), "--without-zstd") {
			t.Errorf("%s: canadian binutils config must pass --without-zstd", tr)
		}
	}
}

func TestHostBinariesAreStatic(t *testing.T) {
	h := triple.MustParse("x86_64-linux-musl")
	if got, want := hostCC(h), "x86_64-linux-musl-gcc -static --static"; got != want {
		t.Errorf("hostCC = %q, want %q", got, want)
	}
	if got, want := hostCXX(h), "x86_64-linux-musl-g++ -static --static"; got != want {
		t.Errorf("hostCXX = %q, want %q", got, want)
	}
	cc, ok := flagValue(hostMakeConfig(canadianCfg(h, h)), "CC")
	if !ok || !strings.Contains(cc, "-static --static") {
		t.Errorf("hostmake CC = %q, want it to carry -static --static", cc)
	}

	// binutils and gcc get it as a configure argument, which autoconf treats as
	// a precious variable and forwards to every sub-configure.
	c := canadianCfg(h, triple.MustParse("mips64-linux-musl"))
	for what, flags := range map[string][]string{"binutils": binutilsConfig(c), "gcc": gccConfig(c)} {
		if v, ok := flagValue(flags, "CC"); !ok || v != hostCC(h) {
			t.Errorf("canadian %s: CC = %q (present=%v), want %q", what, v, ok, hostCC(h))
		}
		if v, ok := flagValue(flags, "CXX"); !ok || v != hostCXX(h) {
			t.Errorf("canadian %s: CXX = %q (present=%v), want %q", what, v, ok, hostCXX(h))
		}
	}
	// The cross build compiles with the container's native gcc; forcing a
	// static musl CC there would be wrong.
	if _, ok := flagValue(gccConfig(crossCfg(h)), "CC"); ok {
		t.Error("cross gcc config must not pin CC")
	}
}

// Every make invocation must carry mcm's overrides, and they must also reach
// recursive sub-makes through the MAKE variable.
// Regression: muslVars once landed only inside the MAKE= redefinition, so the
// top-level make never saw AR=/RANLIB= and musl fell back to
// $(CROSS_COMPILE)ar, dying with "x86_64-linux-musl-ar: No such file or
// directory" at lib/libm.a. Per-invocation vars must appear in BOTH places:
// argv for this make, MAKE= for the sub-makes it spawns.
func TestMakeArgvPutsVarsOnThisMakesCommandLine(t *testing.T) {
	c := crossCfg(triple.MustParse("x86_64-linux-musl"))
	c.Work = "/w/work"
	vars := muslVars(c)
	if len(vars) == 0 {
		t.Fatal("muslVars returned nothing; the rest of this test is vacuous")
	}
	argv := makeArgv(6, cmdVars(vars...), "install")

	inner, ok := flagValue(argv, "MAKE")
	if !ok {
		t.Fatalf("no MAKE= in %v", argv)
	}
	for _, v := range vars {
		if !has(argv, v) {
			t.Errorf("%q is only inside MAKE=, not on the command line; musl will "+
				"ignore it and shell out to $(CROSS_COMPILE)ar.\nargv: %v", v, argv)
		}
		if !strings.Contains(inner, v) {
			t.Errorf("MAKE=%q does not carry %q, so sub-makes lose it", inner, v)
		}
	}
	// AR specifically must resolve to the object tree we just built, never a
	// bare name that would be looked up on $PATH.
	ar, ok := flagValue(argv, "AR")
	if !ok || !strings.HasPrefix(ar, "/") {
		t.Errorf("AR must be an absolute path into obj_binutils, got %q", ar)
	}
}

func TestMakeArgvCarriesSharedFlags(t *testing.T) {
	argv := makeArgv(4, makeVars{}, "all-gcc")
	for _, f := range makeFlags {
		if !has(argv, f) {
			t.Errorf("makeArgv is missing shared flag %q: %v", f, argv)
		}
	}
	if !has(argv, "-j4") {
		t.Errorf("makeArgv did not honor the job count: %v", argv)
	}
	if argv[len(argv)-1] != "all-gcc" {
		t.Errorf("makeArgv put the target somewhere odd: %v", argv)
	}
	inner, ok := flagValue(argv, "MAKE")
	if !ok {
		t.Fatalf("makeArgv must set MAKE= so sub-makes inherit the overrides: %v", argv)
	}
	for _, f := range makeFlags {
		if !strings.Contains(inner, f) {
			t.Errorf("MAKE=%q does not carry %q", inner, f)
		}
	}
	// Extra vars belong in MAKE (sub-makes need them), not on the outer line.
	argv = makeArgv(1, innerVars("enable_shared=no"), "all-target-libgcc")
	inner, _ = flagValue(argv, "MAKE")
	if !strings.Contains(inner, "enable_shared=no") {
		t.Errorf("MAKE=%q must carry enable_shared=no", inner)
	}
	if has(argv, "enable_shared=no") {
		t.Errorf("enable_shared=no should ride in MAKE only, not the outer argv: %v", argv)
	}
}

// Passing the overrides is not enough: automake's info-recursive re-invokes
// $(MAKE), and only the MAKE= redefinition puts INFO_DEPS= on that sub-make's
// command line. Without it canadian binutils dies building doc/bfd.info.
func TestEveryMakeInvocationRedefinesMAKE(t *testing.T) {
	cases := [][]string{
		makeArgv(0, makeVars{}),
		makeArgv(1, makeVars{}, "all"),
		makeArgv(8, makeVars{}, "all-host"),
		makeArgv(4, makeVars{}, "DESTDIR=/s", "install"),
		makeArgv(4, makeVars{}, "DESTDIR=/s", "install-host"),
		makeArgv(2, innerVars("enable_shared=no"), "all-target-libgcc"),
		makeArgv(2, cmdVars(muslVars(crossCfg(triple.MustParse("x86_64-linux-musl")))...), "install"),
	}
	for _, argv := range cases {
		inner, ok := flagValue(argv, "MAKE")
		if !ok {
			t.Errorf("no MAKE= redefinition in %v", argv)
			continue
		}
		if !strings.HasPrefix(inner, "make ") {
			t.Errorf("MAKE=%q must start with the make program", inner)
		}
		for _, f := range makeFlags {
			if !strings.Contains(inner, f) {
				t.Errorf("MAKE=%q is missing %q", inner, f)
			}
			if !has(argv, f) {
				t.Errorf("outer argv %v is missing %q", argv, f)
			}
		}
	}
}

// makeArgv is the only way this package is allowed to spell `make` as a
// command, so that the guarantee above holds for every invocation rather than
// for most of them. builder.run/builder.sh take (ctx, name, dir, envAdd, ...),
// so a hand-rolled make would show up as the literal "make" from arg 4 on.
func TestNoMakeCommandBypassesMakeArgv(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "run" && sel.Sel.Name != "sh") || len(call.Args) < 5 {
					return true
				}
				for _, arg := range call.Args[4:] {
					lit, ok := arg.(*ast.BasicLit)
					if ok && lit.Kind == token.STRING && lit.Value == `"make"` {
						t.Errorf("%s:%d builds a make command by hand; use builder.make so MAKE= is set",
							filepath.Base(name), fset.Position(lit.Pos()).Line)
					}
				}
				return true
			})
		}
	}
}

// gcc reads <build-sysroot>/usr/include, and mcm only ever creates that `usr`
// link inside its throwaway obj_sysroot. Our staging dir must reproduce all
// three conventions or `make all-host` dies in stmp-fixinc.
func TestBuildSysrootStagingHasTheUsrConventions(t *testing.T) {
	script := buildSysrootScript("/a/crossT/aarch64-linux-musl") + scratchSysrootScript
	for _, want := range []string{
		"ln -sfn '/a/crossT/aarch64-linux-musl/include' include",
		"ln -sfn '/a/crossT/aarch64-linux-musl/lib' lib",
		"ln -sfn . usr",
		"ln -sfn lib lib32",
		"ln -sfn lib lib64",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("build sysroot staging is missing %q:\n%s", want, script)
		}
	}
}

// musl installs the loader as an absolute symlink to /lib/libc.so, which
// dangles inside a relocatable sysroot: qemu -L cannot follow it and every
// dynamic target binary fails to start.
func TestFinalizeSysrootFixesTheLoaderAndAddsUsr(t *testing.T) {
	if !strings.Contains(finalizeSysrootScript, "ln -sfn . usr") {
		t.Error("published sysroots must carry the usr -> . convention")
	}
	if !strings.Contains(finalizeSysrootScript, "ln -sf libc.so") {
		t.Error("the musl loader symlink must be rewritten relative")
	}
	if strings.Contains(finalizeSysrootScript, "/lib/libc.so") {
		t.Error("the replacement loader target must be relative, not /lib/libc.so")
	}
	if !strings.Contains(finalizeSysrootScript, "ld-musl-*.so.1") {
		t.Error("the loader glob must cover every arch's ld-musl name")
	}
}

// cross:<T>'s <T>/bin holds BUILD-arch binutils and lib/gcc holds BUILD-arch
// glibc plugins. Neither may reach a HOST toolchain, and install-host does not
// overwrite either.
func TestSeedExcludesBuildArchBinaries(t *testing.T) {
	for _, bad := range []string{"bin", "libexec", "."} {
		for _, d := range seedSysrootDirs {
			if d == bad {
				t.Errorf("seeding %q from cross:<T> would copy BUILD-arch binaries", d)
			}
		}
	}
	if len(seedSysrootDirs) != 2 || seedSysrootDirs[0] != "include" || seedSysrootDirs[1] != "lib" {
		t.Errorf("seedSysrootDirs = %v, want exactly [include lib]", seedSysrootDirs)
	}

	script := libgccSeedScript("/s/lib/gcc")
	for _, want := range []string{"-name '*.o'", "-name '*.a'", "--parents", "-type f"} {
		if !strings.Contains(script, want) {
			t.Errorf("lib/gcc seed is missing %q:\n%s", want, script)
		}
	}
	// A blanket copy is what leaked libcc1plugin.so into both deliverables.
	if strings.Contains(script, "cp -a "+"/") || strings.Contains(script, "/.") {
		t.Errorf("lib/gcc must not be seeded wholesale:\n%s", script)
	}
	for _, bad := range []string{"*.so", "*.la", "plugin"} {
		if strings.Contains(script, bad) {
			t.Errorf("lib/gcc seed must not mention %q:\n%s", bad, script)
		}
	}
}

func TestKernelArchCoversEverySupportedTriple(t *testing.T) {
	for _, tr := range allTriples(t) {
		if _, err := kernelArch(tr); err != nil {
			t.Errorf("%s: %v", tr, err)
		}
	}
	if a, _ := kernelArch(triple.MustParse("i386-linux-musl")); a != "x86" {
		t.Errorf("i386 must map to the kernel's x86 arch, got %q", a)
	}
	if a, _ := kernelArch(triple.MustParse("aarch64-linux-musl")); a != "arm64" {
		t.Errorf("aarch64 must map to arm64, got %q", a)
	}
}

func TestMuslIsBuiltWithTheHalfFinishedCompiler(t *testing.T) {
	c := crossCfg(triple.MustParse("mips64-linux-musl"))
	flags := muslConfig(c)
	cc, ok := flagValue(flags, "CC")
	if !ok || cc != "/w/obj_gcc/gcc/xgcc -B /w/obj_gcc/gcc" {
		t.Errorf("musl CC = %q (present=%v)", cc, ok)
	}
	if v, _ := flagValue(flags, "LIBCC"); v != "/w/obj_gcc/mips64-linux-musl/libgcc/libgcc.a" {
		t.Errorf("musl LIBCC = %q", v)
	}
	if v, _ := flagValue(flags, "--host"); v != "mips64-linux-musl" {
		t.Errorf("musl --host = %q", v)
	}
	if !has(muslVars(c), "AR=/w/obj_binutils/binutils/ar") {
		t.Errorf("musl make vars must name the freshly built ar: %v", muslVars(c))
	}
}

// Regression: every H == T toolchain built clean and then died on
// `#include <iostream>`. See .agents/skills/toolchain-traps.
func TestHostEqualsTargetMovesCxxHeadersOutOfTheSysroot(t *testing.T) {
	for _, tr := range allTriples(t) {
		t.Run(tr.Raw, func(t *testing.T) {
			if !canadianCfg(tr, tr).HostEqualsTarget() {
				t.Fatal("H == T not detected; C++ headers will be left unreachable")
			}
			if canadianCfg(triple.MustParse("aarch64-linux-musl"), tr).HostEqualsTarget() != (tr.Raw == "aarch64-linux-musl") {
				t.Error("HostEqualsTarget disagrees with the triples it was given")
			}
		})
	}
	// A sysroot-relative --with-gxx-include-dir breaks #include_next.
	for _, f := range gccConfig(canadianCfg(triple.MustParse("aarch64-linux-musl"), triple.MustParse("aarch64-linux-musl"))) {
		if strings.HasPrefix(f, "--with-gxx-include-dir=") {
			t.Errorf("%s reorders the include search path; move the headers instead", f)
		}
	}
	c := canadianCfg(triple.MustParse("aarch64-linux-musl"), triple.MustParse("aarch64-linux-musl"))
	if got := c.NativeGxxIncludeDir(); strings.HasPrefix(got, "/") || !strings.HasPrefix(got, "include/c++/") {
		t.Errorf("NativeGxxIncludeDir = %q, want a prefix-relative include/c++/<ver>", got)
	}
	if s := nativeGxxSeedScript(c.Target.Raw, c.NativeGxxIncludeDir()); !strings.Contains(s, "aarch64-linux-musl/include/c++") {
		t.Errorf("seed script does not move from the sysroot: %s", s)
	}
}
