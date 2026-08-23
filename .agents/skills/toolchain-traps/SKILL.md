---
name: toolchain-traps
description: Symptom-to-cause catalogue for broken or subtly-wrong GCC/binutils/musl cross and canadian-cross toolchains — wrong-arch assembler, non-relocatable prefixes, missing kernel headers, target libs built by the wrong compiler, static-linking flags leaking into target code, and the specific places musl-cross-make gets canadian cross wrong. Use when a toolchain build fails, when a built toolchain produces wrong or unrunnable output, or before changing configure flags in internal/recipe.
---

# Toolchain traps

Each entry is **symptom → cause → fix**. All of these were hit or verified in
this project; none are hypothetical. Check here before theorising.

## `as: unrecognized option '--64'` (or the assembler rejects valid flags)

**Cause.** gcc did not find its own assembler and fell through to the build
machine's native `as` on `$PATH`.

gcc locates `as`/`ld` via `<prefix>/<T>/bin/` — the binutils **tooldir**, where
binutils installs a second, *unprefixed* copy of `as`, `ld`, `ar`, `nm`,
`objdump`, `ranlib`, `strip`. Confirm with:

```sh
<T>-gcc -print-search-dirs | tr ':' '\n' | grep bin
<T>-gcc -print-prog-name=as     # MUST be an absolute path, never bare "as"
```

`<prefix>/bin/<T>-as` is **not** on the programs path — it exists for humans.

**Fix.** Ensure binutils actually installed. In a canadian toolchain those
tooldir binaries must be **HOST** ELFs; in a cross toolchain, BUILD ELFs.
`internal/ensure` checks this (`tooldir-elf`, `tooldir-as-ld`, `gcc-finds-as`).

**Related trap:** when seeding a canadian output from a cross toolchain,
**exclude `<crossT>/<T>/bin/`** — copying it plants BUILD-arch binaries into a
HOST toolchain. Everything else under `<T>/` is host-independent and safe.

## Toolchain works in place, breaks when moved

**Cause.** An absolute `--prefix`. gcc and ld only define
`TARGET_SYSTEM_ROOT_RELOCATABLE` when the sysroot is under an **empty** prefix
(see `gcc/configure.ac` and `ld/configure.ac`).

**Fix.** `--prefix=` (empty), `--libdir=/lib`, `--with-sysroot=/<T>`, install
with `DESTDIR=<real location>`. Never substitute an absolute prefix.

## Target libraries silently built by the wrong compiler

**Cause.** In a canadian configuration gcc's `GCC_TARGET_TOOL` (`config/acx.m4`)
takes the `build != host` path and resolves `<T>-gcc` from **`$PATH`**, with no
pinning. Whatever cross compiler happens to be first wins.

**Fix.** Don't build target libraries in the canadian stage at all. Use
`make all-host` / `make install-host` and copy the target side from
`cross_<T>`. If you must build them, pass every `*_FOR_TARGET` explicitly.

## Static-linking flags contaminate target code

**Cause.** `CFLAGS` propagates into `CFLAGS_FOR_TARGET`; `CC` does not.
Putting `-static` in `CFLAGS` statically links the *target* libraries too.

**Fix.** Put it in `CC`/`CXX`: `CC="<H>-gcc -static --static"`. Both spellings
are needed — `-static` for gcc, `--static` because libtool eats `-static`.

## Kernel headers silently missing for some architectures

**Cause.** The sabotage `linux-headers-*` tarball's **top-level** directories
are the ARCH namespace (`aarch64`, `x86_64`, `i386`, `riscv32`, `riscv64`,
`ppc64le`, …). Its `arch/` directory is an internal implementation detail and
does **not** contain every arch — notably no `arch/riscv`.

musl-cross-make discovers architectures by globbing `arch/*`, so for riscv it
finds nothing, `LINUX_ARCH` comes out empty, and the entire kernel-headers step
is skipped **without an error**.

**Fix.** Use a static arch map against the top-level names, never a glob. Ours
is `linuxArch` in `internal/recipe/config.go`. Install with
`make ARCH=<arch> INSTALL_HDR_PATH=<staged> install` (the tarball aliases
`headers_install` to `install`).

**Generalise:** a missing-headers bug of this shape is silent by construction.
When adding a target, assert the headers landed rather than assuming.

## musl needs libgcc, libgcc needs musl headers

**Cause.** A genuine circular dependency in any musl cross toolchain.

**Fix.** The interleaving is mandatory and order-sensitive:

```
binutils all
gcc all-gcc
musl install-headers            -> build sysroot
gcc all-target-libgcc            (enable_shared=no)
musl all
musl install                    -> build sysroot
gcc all                          (libstdc++, libatomic, shared libgcc, ...)
```

Getting this wrong is the most common way to produce a broken musl toolchain.

## Parallel `make install` corrupts the output tree

**Cause.** musl-cross-make's `install: install-musl install-gcc
install-binutils` declares **no ordering** between the three, so under `make -j`
they run concurrently into overlapping directories. Observed result: a
toolchain missing its entire tooldir `bin/`.

**Fix.** Issue install steps sequentially as separate commands. Ours does.

## musl-cross-make's canadian mode specifically

Three independent defects, all inside `litecross/Makefile`:

1. `SYSROOT = /` is selected on "HOST is non-empty", not "HOST == TARGET". A
   true canadian (`H != T`) therefore dumps the *target* libc at the toolchain
   root. `OUTPUT` is keyed on HOST only, so multi-target canadian builds
   overwrite each other.
2. The HOST branch passes no `*_FOR_TARGET` and no build sysroot.
3. **Every inter-package ordering edge lives inside `ifeq ($(HOST),)`.** With
   HOST set, `make -j` builds musl, gcc and binutils concurrently against the
   build machine's `/`.

mcm also forces `HOST := TARGET` under `NATIVE=1`, so it only ever supports
host == target. Treat its canadian path as a starting point, not a reference.

## `BUILD_SYSTEM_HEADER_DIR ... does not exist: <sysroot>/usr/include`

**Cause.** gcc computes `BUILD_SYSTEM_HEADER_DIR =
$with_build_sysroot$(NATIVE_SYSTEM_HEADER_DIR)`, i.e. `<bs>/usr/include`
(`gcc/configure:15274`). An *installed* musl sysroot has `include/ lib/ bin/`
and no `usr` symlink — mcm only creates `usr -> .` in its throwaway
`obj_sysroot`, never in the shipped tree.

**Fix.** Point `--with-build-sysroot` at a staging directory containing
`include ->`, `lib ->`, `usr -> .`, `lib32 -> lib`, `lib64 -> lib`. Also emit
`usr -> .` inside every sysroot you produce — one symlink, and it satisfies
every tool that assumes the `/usr/include` convention.

## binutils dies building `doc/bfd.info` despite `INFO_DEPS=` / `MAKEINFO=false`

**Cause.** Passing those as command-line variables is not enough. binutils
2.44's `bfd/Makefile` reaches `info-recursive` from `all`, and the automake
recursion re-invokes `$(MAKE)`; the vars don't reliably land on the sub-make's
command line. This is why litecross writes
`$(MAKE) MAKE="$(MAKE) $(LIBTOOL_ARG)" all` — the `MAKE=` redefinition is the
load-bearing part, and it is easy to drop when porting the recipe.

**Fix.** Pass `MAKE="make <all the flags>"` *in addition to* the flags
themselves, on every invocation — binutils and gcc, build and install.

## `x86_64-linux-musl-ar: No such file or directory` building musl

**Cause.** musl's Makefile defaults `AR = $(CROSS_COMPILE)ar`, so with
`--host=<T>` it looks for `<T>-ar` on `$PATH` — which does not exist yet, since
binutils has only been *built*, not installed. It gets as far as `lib/libm.a`
before failing.

**Fix.** Pass `AR=` and `RANLIB=` pointing into the binutils object tree
(`obj_binutils/binutils/{ar,ranlib}`) on musl's make **command line**.

**The subtle part — two kinds of make variable, not interchangeable:**

```make
$(MAKE) $(MUSL_VARS)                                       # command line
$(MAKE) MAKE="$(MAKE) enable_shared=no" all-target-libgcc  # inner only
```

- `AR=`/`RANLIB=` must be on *this* make's command line, because musl's own
  Makefile reads them.
- `enable_shared=no` must ride only inside `MAKE=`, because it is meant for the
  recursive libgcc sub-make alone.

Putting either in the other's place breaks a build, in opposite directions and
with unrelated-looking symptoms. `internal/recipe` models this explicitly as
`makeVars{Cmd, Inner}` rather than leaving it to convention — keep it that way.
The regression test is `TestMakeArgvPutsVarsOnThisMakesCommandLine`.

## Build-arch glibc `.so` files survive into a static toolchain

**Cause.** Seeding a canadian output with `cp -a <crossT>/lib/gcc/.` copies
`lib/gcc/<T>/<ver>/plugin/{libcc1plugin,libcp1plugin}.so*` and their `.la`.
Those are BUILD-arch **glibc** objects (`NEEDED: libc.so.6`), and
`install-host` does not overwrite them because a `-static --static` host build
emits no shared plugins. They ship silently.

**Fix.** Seed only `crt*.o` and `*.a` from `lib/gcc/`, or delete
`plugin/*.so*` and `*.la` afterwards. Generally: after any cross-tree copy,
assert no file in the output has the wrong ELF identity — `ensure` does this,
which is why the directory checks must cover *every* directory, not just `bin`.

## `qemu: Could not open '/lib/ld-musl-<arch>.so.1'` even with `-L <sysroot>`

**Cause.** musl installs `lib/ld-musl-<arch>.so.1` as an **absolute** symlink
to `/lib/libc.so`. qemu-user resolves the interpreter through the host
filesystem, so `-L` cannot redirect it. Every dynamically-linked target binary
fails to run — which looks like a broken toolchain but isn't.

**Fix.** Make it relative in the emitted sysroot:
`ln -sf libc.so <sysroot>/lib/ld-musl-<arch>.so.1`. As a fallback for sysroots
you don't control, invoke the loader directly:
`qemu-<arch>-static <sysroot>/lib/libc.so ./prog`.

## `liblto_plugin.so` missing (static host tools)

**Cause.** `CC="<H>-gcc -static --static"` makes libtool emit only
`libexec/gcc/<T>/<ver>/liblto_plugin.a`. A statically linked `ld`/`ar` cannot
`dlopen` a plugin at all, so static host tools and a shared LTO plugin are
fundamentally at odds.

**Status — measured, not assumed.** On real canadian toolchains in both
directions: all four `lto` probe cells pass, and so do `lto-archive`
(`-flto -c` → `gcc-ar rcs` → `gcc-ranlib` → link) and `gcc-nm-lto` (gcc-nm does
read symbols out of an LTO archive). LTO drives through `lto-wrapper`, which
does not need the `.so`. `ensure` reports the parity gap as a non-fatal skip
via `lto-plugin`, and `gcc-nm-lto` is a *separate* check so "plugin not
loading" stays distinguishable from "LTO broken".

The known-broken case is a third-party build system that hardcodes
`ar --plugin .../liblto_plugin.so`. If you ever need the `.so`, the host tools
cannot be fully static — that is the actual trade, so decide it deliberately.

## `fatal error: iostream: No such file or directory` — but only when H == T

C compiles fine, every C++ probe fails, at every `-O` level, static and shared.
The toolchain builds and installs without a single error. Hits **every diagonal
entry of the matrix** (`aarch64→aarch64`, `x86_64→x86_64`, ...) while every
H ≠ T toolchain built from the same recipe is perfect.

**Cause.** `gcc/configure.ac` only adds the `<target>/` component to the C++
header directory when host and target differ:

```sh
libstdcxx_incdir='include/c++/$(version)'
if test x$host != x$target; then
   libstdcxx_incdir="$target_alias/$libstdcxx_incdir"
fi
```

So a canadian build with `--host=<T> --target=<T>` compiles in
`<prefix>/include/c++/<ver>`, but `seedTarget` copies the headers from
`cross:<T>` to `<prefix>/<T>/include/c++/<ver>`. gcc never looks there.

Note `--build` is irrelevant: the build triple is the GNU one
(`aarch64-unknown-linux-gnu`), so a canadian job is still "not a cross" as far
as this test is concerned whenever H == T.

**Do NOT fix it with `--with-gxx-include-dir` pointing inside the sysroot.**
That was tried and it fails in a way that looks like progress: `<iostream>`
starts resolving, then C++ dies one level deeper on
`cstdlib:79: #include_next <stdlib.h>: No such file or directory`.

A path under the sysroot makes configure set
`gcc_gxx_include_dir_add_sysroot=1`, and `incpath.cc` **skips every
`add_sysroot` entry in its first pass**:

```c
if (sysroot && p->add_sysroot)
  continue;
```

so the C++ directories are appended *after* the libc directory instead of
before it, and `#include_next` from `cstdlib` finds nothing after itself:

```
working (H != T)            broken (sysroot-relative gxx dir)
 1 c++/<ver>                 1 <T>/include          <- libc first
 2 c++/<ver>/<T>             2 lib/gcc/<T>/<ver>/include
 3 c++/<ver>/backward        3 c++/<ver>            <- C++ last
 4 <T>/include   <- libc     4 c++/<ver>/<T>
 5 lib/gcc/<T>/<ver>/include 5 c++/<ver>/backward
```

**Fix.** Leave the configure flags alone and move the *headers* instead. When
H == T, relocate the seeded C++ headers from `<prefix>/<T>/include/c++/<ver>`
to `<prefix>/include/c++/<ver>` — the prefix-relative spot gcc's native default
(`$(libsubdir)/$(libsubdir_to_prefix)include/c++/$(version)`) already points
at. `add_sysroot` stays 0, the search order is preserved, and libc headers
still come from the tooldir. See `nativeGxxSeedScript` / `HostEqualsTarget`.

This means the diagonal deliberately has a different sysroot layout from the
rest of the matrix. That is correct: each matches what gcc natively does for
that configuration, and forcing one layout is what breaks the ordering.

Cross jobs are unaffected — their `--host` is the GNU build triple, never equal
to a `*-linux-musl` target.

**Where the libc headers actually come from.** Not `<sysroot>/usr/include`, as
you might assume — that entry is deduplicated away. They come from
`TOOL_INCLUDE_DIR` = `<prefix>/<T>/include`, the binutils tooldir. Dump the
truth instead of guessing:

```sh
qemu-<host>-static <out>/bin/<T>-g++ -v -E -x c++ /dev/null -o /dev/null
```

It also prints `ignoring nonexistent directory` and `ignoring duplicate
directory` lines, which is how you tell a missing path from a deduped one.

**Lesson for tests.** The old config tests only ever built `H ≠ T` fixtures, so
nothing caught this until a real 7-minute `gcc-all-host` finished. Any new
matrix invariant must be exercised with `H == T` as well.

## Toolchain ships `ar`/`as`/`ld` instead of `<T>-ar`/`<T>-as`/`<T>-ld`

Only on the diagonal (H == T). `ensure`'s tool-surface check reports 17 missing
prefixed tools while `bin/` visibly contains all of them unprefixed. gcc is
fine (it installs `<T>-gcc` either way); binutils is not.

**Cause.** Same family as the `iostream` trap: with host == target, binutils
configures as a **native** build and installs unprefixed tool names. For a
cross build autoconf derives `program_transform_name` from `target_alias` and
you get the `<T>-` prefix for free.

**Fix.** Don't fight the configure — a native binutils installing `ar` is
correct. Add the prefixed names as hardlink aliases after install
(`aliasNativeTools` / `nativeToolAliasScript`), so the diagonal presents the
same interface as every other cell of the matrix. Driving it off
`ensure.BinutilsTools` keeps the aliases and the check in step.

`--program-prefix=<T>-` also works and is arguably more "correct", but it
renames rather than adds, so the native `ar` a user on that machine expects
disappears. Aliasing keeps both.

**Related:** the tooldir `<prefix>/<T>/bin/` is *supposed* to hold unprefixed
tools in every configuration — do not "fix" that one.

## `-static-pie` fails: "relocation ... can not be used when making a shared object"

The errors name objects inside `libc.a` (`__libc_start_main.o`, `exit.o`,
`printf.o`), not your code.

**Cause.** It is not gcc. musl's Makefile gives `-fPIC` only to the `.lo`
objects that become `libc.so`:

```make
$(LOBJS) $(LDSO_OBJS): CFLAGS_ALL += -fPIC
```

`libc.a` is built from plain `.o` objects and therefore cannot be linked into a
PIE. musl 1.2.5 has no `--enable-static-pie` configure option.

**Fix.** Pass `CFLAGS=-fPIC` to musl's configure (`muslConfig`). Measured on
aarch64/x86_64/riscv32/riscv64, `-O0` and `-O2`: all link, run, and produce
`Type: DYN` with no `PT_INTERP`.

**gcc's `--enable-static-pie` is NOT required** and was deliberately left out —
`0004-static-pie.diff` (from musl-cross-make) already rewires the startfile
spec to use `rcrt1.o`. A stock gcc plus a PIC `libc.a` was verified working.

**But `-static-pie` does not imply `-fPIE`.** Your own objects must be compiled
PIE explicitly or you get the same relocation error pointing at *your* `.o`.
The probe passes both.

**Cost.** Every static binary now comes from a PIC `libc.a`, which is slightly
larger and slower. musl-cross-make does not do this. That is the price of
static-pie support; revisit if size regressions matter more.

**Verifying it.** A `-static-pie` binary has **no `PT_INTERP`**, so
`ensure`'s "dynamic unless built with -static" rule reports a false failure.
That is what `Probe.NoInterp` exists for. A probe that only prints a constant
proves nothing here — take the address of a function and a global in static
initialisers so the check actually depends on rcrt1 self-relocation running.

## qemu binary names don't match triples

`powerpc64-linux-musl` → `qemu-ppc64-static`, `powerpc64le-linux-musl` →
`qemu-ppc64le-static`. `triple.QemuName()` already handles this; don't derive
qemu names from the triple's arch field yourself.

## `cp: cannot stat '<srctree>/...'` on a file that plainly exists

A published srctree suddenly fails to copy, and the named path is visibly there
when you `ls` it **from macOS**. One job may have used the same tree minutes
earlier without complaint.

**Cause.** Not the recipe, and not a race. `cp -al` hardlinks every srctree into
every job's work dir, and **a failed job keeps its work dir on purpose** (that
is what makes `gccf logs`/`shell` useful). After a few failed builds the link
count on every srctree file climbs — 9 in the case that produced this — and
OrbStack/virtiofs starts losing entries: the container sees an inode's metadata
but cannot read it, while the host reads it perfectly.

It hits regular files as well as symlinks, and it moves around: repair one tree
and the next build fails on a different file in a different tree. That wandering
is the signature. Do not chase the individual file.

```sh
# from the container -- the tell
ls -la <dir>          # lrwxrwxrwx 4 root root 27 ... video   <- no "-> target"
readlink video        # exit 1: cannot read symbolic link
stat video            # cannot statx: No such file or directory
```

Note the size is still correct (27 = the length of the target string) and the
link count differs from its neighbours (4 where every sibling is 5). From the
host, `readlink` returns the right target and `test -e` passes.

**Fix — drop the link count, don't chase the file.** With no build running:

```sh
rm -rf dist/work/*      # scratch by definition; nothing published depends on it
```

That alone restored a tree that had been failing to copy, with no
re-extraction. Check `ls dist/work | wc -l` when this appears — a pile of stale
dirs from failed jobs is the tell.

Re-extracting (`rm -rf dist/srctrees/<pkg>-<ver>`) also works and is safe, but
it treats the symptom: the next build will fail on some other file. Reach for
it only if clearing `dist/work` does not help.

Never hand-repair the file or symlink. Srctrees are content-addressed published
artifacts; editing one in place silently breaks the guarantee that what is on
disk matches what its key says.

**Do not conclude the recipe corrupted the tree.** Check whether the host can
`readlink` it first — if the host is fine and the container is not, it is the
file-sharing layer, and no amount of reading the build scripts will explain it.

## binfmt_misc appears absent but isn't

`/proc/sys/fs/binfmt_misc` is not mounted in the OrbStack builder container and
`grep binfmt /proc/mounts` is empty — but the handlers are registered at the VM
level and foreign-binary exec works, including **nested** exec (a HOST-arch gcc
under `qemu-<host>` forking HOST-arch `cc1`/`as`/`ld`). Verify with a real
foreign binary; do not conclude from the missing mount that it's unavailable.

## `libcody`: `no matching function for call to 'S2C(const char8_t [2])'`

Every `cross_<T>` job dies in `gcc-all-gcc` compiling `libcody/buffer.cc` and
`cody.hh`, with dozens of near-identical errors naming `char8_t`.

**Cause.** The *build machine's* g++ is GCC 15 or newer, whose default C++
standard is C++20 or later. In C++20 `u8"..."` has type `const char8_t[]`, and
gcc 14.2's libcody only declares `S2C(const char *)`. This has nothing to do
with the target triple: every target fails identically, so whichever one is
scheduled first is the one you see in the error. Do not go looking for an
arch-specific cause.

**Fix.** Pin the standard the *build* compiler uses, in `CXX` — **not**
`CXXFLAGS`. `CXXFLAGS` propagates into `CXXFLAGS_FOR_TARGET` and would pin the
target libraries too (see the static-linking trap above; `CXX` does not
propagate). `commonConfig` in `internal/recipe/config.go` passes
`CXX=g++ -std=gnu++17` for cross jobs, and `CXX_FOR_BUILD=` likewise for
canadian ones — a canadian job's `CXX` is our own gcc 14.2, which already
defaults to gnu++17, but its build-side generator programs still use the build
machine's compiler.

**Diagnose in 5 seconds, not in a 10-minute rebuild.** The srctree is already
published, so compile the one file by hand:

```sh
cd dist/srctrees/gcc-14.2.0/tree/libcody
g++ -c buffer.cc -I. -o /tmp/x.o                  # reproduces the failure
g++ -std=gnu++17 -c buffer.cc -I. -o /tmp/x.o     # proves the fix
```

**Generalise:** gcc 14.2 was written against a gnu++17-default world. When the
host compiler is far newer than the gcc being built, suspect a default-standard
change before suspecting the recipe.

## `bin-elf` fails: `<T>-embedspu` is "not an ELF file (starts with "#! /bin/sh")"

Only on powerpc targets. A `cross_powerpc64*` toolchain builds cleanly and then
fails its own verification with `1 of 30 files in .../bin are not BUILD ELFs`.

**Cause.** Not a broken toolchain — binutils genuinely installs `embedspu` as a
`/bin/sh` script for powerpc. The old `bin-elf` check required *every* file in
`<prefix>/bin` to be an ELF for the machine the tools run on.

**Fix.** `scanDir` (`internal/ensure/toolchain.go`) classifies a file starting
with `#!` as arch-neutral: it is reported by name in the pass message but not
ELF-checked. A script cannot be a wrong-arch binary, which is the leak the
check exists to catch, so the check keeps its teeth for everything else.

**Do not weaken this to "non-ELF files are fine".** The whole point of the
directory checks is that a BUILD-arch binary must never survive into a HOST
toolchain; only the `#!` case is genuinely architecture-free.

## Two tests fail on a Fedora host and both are the environment

Neither indicates a broken build system; both pass in the debian container.

- `TestNativeToolchainRunsProbes` — the native `-static-pie` probe fails with
  `cannot find -lc`. Fedora does not install `glibc-static` by default. Only
  the *native* probe needs it; every target toolchain builds its own static
  musl, so the matrix is unaffected.
- `TestQemuPathAcceptsDirOrTemplate` — expects `/usr/bin/qemu-ppc64le-static`,
  gets `/usr/bin/qemu-ppc64le`. `ensure.QemuFor` returns whichever binary
  exists, and Fedora ships both the static and dynamic variants where debian
  ships only the static one. Verification still resolves qemu correctly.

## Verifying you actually have a canadian toolchain

Architecture checks alone are not sufficient — check the loader too, since a
right-arch binary can still be linked against the wrong libc:

```sh
file <out>/bin/<T>-gcc                       # HOST arch, statically linked
readelf -h <out>/libexec/gcc/<T>/*/cc1       # HOST arch
qemu-<host>-static <out>/bin/<T>-gcc -dumpmachine     # prints <T>
qemu-<host>-static <out>/bin/<T>-gcc -print-prog-name=as   # absolute path
qemu-<host>-static <out>/bin/<T>-g++ -O2 t.cc -o t
file t                                       # TARGET arch
readelf -l t | grep interpreter              # /lib/ld-musl-<arch>.so.1
qemu-<target>-static -L <out>/<T> ./t
```

`internal/ensure` automates all of this plus the full probe matrix
(C, C++, exceptions, threads, TLS, atomics, dlopen, LTO, static-pie; `-O0`
and `-O2`; dynamic and static). Extend it rather than writing ad-hoc checks.
