package recipe

// Everything in this file is a pure function of a buildCfg. No I/O, no
// filesystem, no process execution: that is what makes the configure lines
// testable and what makes the job keys honest.
//
// The layout these flags encode is musl-cross-make's, and it is the reason the
// toolchains are relocatable:
//
//	--prefix=            empty, so nothing absolute is baked in
//	--libdir=/lib        relative to that empty prefix
//	--with-sysroot=/<T>  ditto; the real sysroot is <root>/<T>
//	DESTDIR=<root>       supplies the location, at install time only
//
// gcc and ld then resolve everything relative to argv[0].

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// buildCfg is the complete description of one build: which triples, which
// directories. Every path a recipe needs is derived here rather than being
// concatenated at the point of use.
type buildCfg struct {
	Work  string // per-job scratch dir; holds src_* and obj_*
	Stage string // per-job DESTDIR; becomes the published artifact
	Build string // config.guess output, e.g. "aarch64-unknown-linux-gnu"

	Target triple.Triple
	Host   triple.Triple // zero value on cross jobs, where host == build
	// Canadian selects --host=<H> over --host=<B> and switches the recipe from
	// "build the whole toolchain" to "build only host code".
	Canadian bool

	CrossH   string // artifact dir of cross:<H>   (canadian only)
	CrossT   string // artifact dir of cross:<T>   (canadian only)
	HostMake string // artifact dir of hostmake:<H> (canadian only)

	// Fallback rung selectors for canadian gcc; see canadian.go.
	NoFixincludes  bool
	ForTargetTools bool
}

// HostTriple is what goes into --host: the real host on a canadian build, the
// build machine everywhere else.
func (c buildCfg) HostTriple() string {
	if c.Canadian {
		return c.Host.Raw
	}
	return c.Build
}

// Sysroot is the sysroot path *relative to the empty prefix*. It is never an
// absolute path on the build machine.
func (c buildCfg) Sysroot() string { return "/" + c.Target.Raw }

// NativeGxxIncludeDir is where gcc looks for C++ headers when host == target.
func (c buildCfg) NativeGxxIncludeDir() string {
	return "include/c++/" + src(pkgGCC).Version
}

// HostEqualsTarget reports whether gcc will configure this as a native compiler.
func (c buildCfg) HostEqualsTarget() bool {
	return c.Canadian && c.Host.Raw == c.Target.Raw
}

func (c buildCfg) src(pkg string) string { return filepath.Join(c.Work, "src_"+pkg) }
func (c buildCfg) obj(pkg string) string { return filepath.Join(c.Work, "obj_"+pkg) }

// ObjSysroot is the throwaway sysroot the cross build assembles as it goes:
// musl headers first, then musl itself, so libgcc and libstdc++ can be built
// against a libc that does not exist yet at the start.
func (c buildCfg) ObjSysroot() string { return filepath.Join(c.Work, "obj_sysroot") }

// BuildSysroot is what --with-build-sysroot points at on a canadian build.
//
// It cannot simply be cross:<T>'s installed sysroot. gcc computes
// BUILD_SYSTEM_HEADER_DIR as $with_build_sysroot$(NATIVE_SYSTEM_HEADER_DIR),
// i.e. "<build-sysroot>/usr/include" (gcc/configure), and an installed sysroot
// has no `usr`: mcm only ever creates that link inside its throwaway
// obj_sysroot. Without it `make all-host` dies in stmp-fixinc with
// "The directory (BUILD_SYSTEM_HEADER_DIR) ... does not exist".
//
// So we stage one: a directory of symlinks into cross:<T> carrying the same
// usr/lib32/lib64 conventions obj_sysroot has.
func (c buildCfg) BuildSysroot() string { return filepath.Join(c.Work, "obj_build_sysroot") }

// StageSysroot is where the target's headers and libraries land in the
// published artifact.
func (c buildCfg) StageSysroot() string { return filepath.Join(c.Stage, c.Target.Raw) }

// commonConfig is mcm's COMMON_CONFIG: applied to binutils and gcc alike.
// The debug-prefix-map keeps the (pid-tagged, ephemeral) work directory out of
// the debug info of every object we ship.
func commonConfig(c buildCfg) []string {
	f := []string{
		"--disable-nls",
		"--with-debug-prefix-map=" + c.Work + "=",
	}
	if c.Canadian {
		// As configure *arguments*, not just environment: autoconf caches
		// precious variables and forwards them to every sub-configure, which
		// an exported CC does not reliably reach. This is mcm's COMMON_CONFIG.
		f = append(f, "CC="+hostCC(c.Host), "CXX="+hostCXX(c.Host), "CXX_FOR_BUILD="+buildCXX())
	} else {
		f = append(f, "CXX="+buildCXX())
	}
	return f
}

// gcc 14.2's libcody predates char8_t, so a build g++ defaulting to C++20 or
// later (gcc 15+) fails on its u8 literals. CXX, never CXXFLAGS: CXXFLAGS
// propagates into CXXFLAGS_FOR_TARGET and would pin the target libraries too.
func buildCXX() string { return "g++ -std=gnu++17" }

// binutilsConfig is mcm's FULL_BINUTILS_CONFIG plus the explicit feature
// switches we need for build/host parity (see RECIPES "Notes / gotchas").
func binutilsConfig(c buildCfg) []string {
	f := []string{"--disable-separate-code"}
	f = append(f, commonConfig(c)...)
	f = append(f,
		"--disable-werror",
		"--target="+c.Target.Raw,
		"--prefix=",
		"--libdir=/lib",
		"--disable-multilib",
		"--with-sysroot="+c.Sysroot(),
		"--enable-deterministic-archives",
		// zstd exists for BUILD but not for HOST in the container; letting
		// configure autodetect would make cross: and canadian: disagree.
		"--without-zstd",
		"--disable-gdb",
		"--disable-gdbserver",
		"--disable-sim",
		"--disable-readline",
		"--disable-libdecnumber",
		"--build="+c.Build,
		"--host="+c.HostTriple(),
	)
	return f
}

// gccConfig is mcm's FULL_GCC_CONFIG. The arch-specific part comes from
// triple.GCCConfig (mcm's GCC_CONFIG_FOR_TARGET), so ABI/float/arch decisions
// live in exactly one place.
func gccConfig(c buildCfg) []string {
	f := []string{"--enable-languages=c,c++"}
	f = append(f, c.Target.GCCConfig()...)
	f = append(f, commonConfig(c)...)
	f = append(f,
		"--disable-bootstrap",
		// a gmp flag, passed through the gcc toplevel to the in-tree gmp
		"--disable-assembly",
		"--disable-werror",
		"--target="+c.Target.Raw,
		"--prefix=",
		"--libdir=/lib",
		"--disable-multilib",
		"--with-sysroot="+c.Sysroot(),
		"--enable-tls",
		"--disable-libmudflap",
		"--disable-libsanitizer",
		"--disable-gnu-indirect-function",
		"--enable-initfini-array",
		"--enable-libstdcxx-time=rt",
		"--build="+c.Build,
		"--host="+c.HostTriple(),
	)
	if c.NoFixincludes {
		f = append(f, "--disable-fixincludes")
	}
	if c.Canadian {
		// Target code is not built here; it is copied from cross:<T>. The
		// build sysroot only has to be good enough for configure's probes and
		// for fixincludes -- but it does have to have a `usr`. See BuildSysroot.
		f = append(f, "--with-build-sysroot="+c.BuildSysroot())
		if c.ForTargetTools {
			f = append(f, canadianForTarget(c)...)
		}
	} else {
		f = append(f, "--with-build-sysroot="+c.ObjSysroot())
		f = append(f, crossForTarget(c)...)
	}
	return f
}

// crossForTarget points gcc at the binutils we just built *in its object tree*,
// because they are not installed anywhere yet when gcc configures.
func crossForTarget(c buildCfg) []string {
	ob := c.obj("binutils")
	return []string{
		"AR_FOR_TARGET=" + filepath.Join(ob, "binutils", "ar"),
		"AS_FOR_TARGET=" + filepath.Join(ob, "gas", "as-new"),
		"LD_FOR_TARGET=" + filepath.Join(ob, "ld", "ld-new"),
		"NM_FOR_TARGET=" + filepath.Join(ob, "binutils", "nm-new"),
		"OBJCOPY_FOR_TARGET=" + filepath.Join(ob, "binutils", "objcopy"),
		"OBJDUMP_FOR_TARGET=" + filepath.Join(ob, "binutils", "objdump"),
		"RANLIB_FOR_TARGET=" + filepath.Join(ob, "binutils", "ranlib"),
		"READELF_FOR_TARGET=" + filepath.Join(ob, "binutils", "readelf"),
		"STRIP_FOR_TARGET=" + filepath.Join(ob, "binutils", "strip-new"),
	}
}

// canadianForTarget is the last fallback rung: name the installed B->T tools
// explicitly so a full `make` can produce target libraries again.
func canadianForTarget(c buildCfg) []string {
	bin := filepath.Join(c.CrossT, "bin")
	p := func(tool string) string { return filepath.Join(bin, c.Target.Raw+"-"+tool) }
	return []string{
		"CC_FOR_TARGET=" + p("gcc"),
		"CXX_FOR_TARGET=" + p("g++"),
		"GCC_FOR_TARGET=" + p("gcc"),
		"AR_FOR_TARGET=" + p("ar"),
		"AS_FOR_TARGET=" + p("as"),
		"LD_FOR_TARGET=" + p("ld"),
		"NM_FOR_TARGET=" + p("nm"),
		"OBJCOPY_FOR_TARGET=" + p("objcopy"),
		"OBJDUMP_FOR_TARGET=" + p("objdump"),
		"RANLIB_FOR_TARGET=" + p("ranlib"),
		"READELF_FOR_TARGET=" + p("readelf"),
		"STRIP_FOR_TARGET=" + p("strip"),
	}
}

// muslConfig builds libc with the compiler that is sitting half-finished in
// obj_gcc: xgcc plus the libgcc.a produced by all-target-libgcc.
func muslConfig(c buildCfg) []string {
	xgccDir := filepath.Join(c.obj("gcc"), "gcc")
	return []string{
		"--prefix=",
		"--host=" + c.Target.Raw,
		"CC=" + filepath.Join(xgccDir, "xgcc") + " -B " + xgccDir,
		"LIBCC=" + filepath.Join(c.obj("gcc"), c.Target.Raw, "libgcc", "libgcc.a"),
		// musl only gives -fPIC to the .lo objects that become libc.so, so
		// libc.a cannot be linked into a PIE. See toolchain-traps.
		"CFLAGS=-fPIC",
	}
}

// muslVars are make-time overrides; musl's configure does not probe for these.
func muslVars(c buildCfg) []string {
	ob := c.obj("binutils")
	return []string{
		"AR=" + filepath.Join(ob, "binutils", "ar"),
		"RANLIB=" + filepath.Join(ob, "binutils", "ranlib"),
	}
}

// hostMakeConfig configures GNU make as a static HOST binary. Static because a
// toolchain you can drop on any machine of the right arch is the whole point,
// and because running it under qemu-user then needs no sysroot.
func hostMakeConfig(c buildCfg) []string {
	return []string{
		"--build=" + c.Build,
		"--host=" + c.Host.Raw,
		"--prefix=",
		"--disable-dependency-tracking",
		"--without-guile",
		"CC=" + c.Host.Raw + "-gcc -static --static",
	}
}

// makeFlags are mcm's `MAKE +=` lines. They go on *every* make command line:
// MULTILIB_OSDIRNAMES and INFO_DEPS/infodir defeat gcc's multilib and texinfo
// machinery, ac_cv_prog_lex_root papers over a missing flex, and MAKEINFO=false
// stops the documentation build we do not ship.
var makeFlags = []string{
	"MULTILIB_OSDIRNAMES=",
	"INFO_DEPS=",
	"infodir=",
	"ac_cv_prog_lex_root=lex.yy",
	"MAKEINFO=false",
}

// makeArgv assembles a make command line the mcm way. `vars` are extra
// overrides that must reach recursive sub-makes (mcm's MAKE="$(MAKE) ..."
// trick), `args` are targets and one-off overrides for this make only.
// makeVars separates the two kinds of variable a make invocation can carry,
// because they are NOT interchangeable and mixing them up breaks builds in
// opposite directions:
//
//   - Cmd lands on this make's own command line (and, being an override,
//     propagates onward). musl's AR=/RANLIB= must be here: musl's own Makefile
//     reads them, and without them it shells out to $(CROSS_COMPILE)ar, which
//     does not exist yet.
//   - Inner rides only inside MAKE=, so it reaches the recursive sub-make and
//     nothing else. gcc's enable_shared=no must be here: it is meant for the
//     libgcc sub-make alone. This is mcm's `MAKE="$(MAKE) enable_shared=no"`.
type makeVars struct {
	Cmd   []string
	Inner []string
}

func cmdVars(v ...string) makeVars   { return makeVars{Cmd: v} }
func innerVars(v ...string) makeVars { return makeVars{Inner: v} }

func makeArgv(jobs int, v makeVars, args ...string) []string {
	inner := append([]string{"make"}, makeFlags...)
	inner = append(inner, v.Cmd...)
	inner = append(inner, v.Inner...)

	argv := []string{"make"}
	if jobs > 0 {
		argv = append(argv, fmt.Sprintf("-j%d", jobs))
	}
	argv = append(argv, makeFlags...)
	argv = append(argv, v.Cmd...)
	argv = append(argv, "MAKE="+strings.Join(inner, " "))
	argv = append(argv, args...)
	return argv
}

// seedSysrootDirs are the only subdirectories of cross:<T>'s sysroot a canadian
// build may copy. Notably absent: bin/, which in a cross tree holds BUILD-arch
// copies of ar/as/ld/... that must never reach a HOST toolchain.
var seedSysrootDirs = []string{"include", "lib"}

// scratchSysrootScript makes a directory look like a sysroot gcc can build
// against: usr folds back on itself, lib32 and lib64 fold onto lib.
const scratchSysrootScript = "ln -sfn . usr\nln -sfn lib lib32\nln -sfn lib lib64\n"

// finalizeSysrootScript repairs a published sysroot: add the usr convention,
// and rewrite musl's absolute ld-musl-<arch>.so.1 -> /lib/libc.so as a relative
// link so it resolves inside the sysroot (and under `qemu-user -L`).
const finalizeSysrootScript = `
ln -sfn . usr
for l in lib/ld-musl-*.so.1; do
	[ -L "$l" ] || continue
	ln -sf libc.so "$l"
done
`

// libgccSeedScript copies the target objects and archives out of cross:<T>'s
// lib/gcc, and nothing else. --parents preserves the <T>/<ver>/ nesting gcc
// looks them up by. Everything it skips (include/, include-fixed/,
// install-tools/, and the BUILD-arch plugin/*.so) is supplied by install-host.
//
// -t is not decoration: find's -exec ... + form requires {} to be the final
// argument, so the destination cannot be written after it.
func libgccSeedScript(dst string) string {
	return "find . -type f \\( -name '*.o' -o -name '*.a' \\) -exec cp -a --parents -t " + shQuote(dst) + " {} +"
}

// nativeToolAliasScript hardlinks <T>-<tool> onto each unprefixed tool that a
// native binutils install leaves in bin/. Run from the stage's bin/.
func nativeToolAliasScript(target string, tools []string) string {
	var b strings.Builder
	for _, n := range tools {
		p := shQuote(target + "-" + n)
		b.WriteString("[ -e " + p + " ] || [ ! -e " + shQuote(n) + " ] || ln -f " + shQuote(n) + " " + p + "\n")
	}
	return b.String()
}

// nativeGxxSeedScript moves the C++ headers to where a host == target gcc looks.
func nativeGxxSeedScript(target, nativeDir string) string {
	src := filepath.Join(target, "include", "c++")
	return "mkdir -p " + shQuote(filepath.Dir(nativeDir)) + "\n" +
		"rm -rf " + shQuote(nativeDir) + "\n" +
		"mv " + shQuote(filepath.Join(src, filepath.Base(nativeDir))) + " " + shQuote(nativeDir) + "\n" +
		"rmdir " + shQuote(src) + " 2>/dev/null || true\n"
}

func buildSysrootScript(crossSysroot string) string {
	return "ln -sfn " + shQuote(filepath.Join(crossSysroot, "include")) + " include\n" +
		"ln -sfn " + shQuote(filepath.Join(crossSysroot, "lib")) + " lib\n"
}

// linuxArch maps a target arch onto the kernel's arch/ directory name, which is
// what `make headers_install ARCH=` wants.
var linuxArch = map[string]string{
	"aarch64":     "arm64",
	"arm":         "arm",
	"i386":        "x86",
	"x86_64":      "x86",
	"mips64":      "mips",
	"powerpc64":   "powerpc",
	"powerpc64le": "powerpc",
	"riscv32":     "riscv",
	"riscv64":     "riscv",
	"s390x":       "s390",
}

func kernelArch(t triple.Triple) (string, error) {
	a, ok := linuxArch[t.Arch]
	if !ok {
		return "", fmt.Errorf("recipe: no linux arch/ directory known for %q; add it to linuxArch", t.Arch)
	}
	return a, nil
}
