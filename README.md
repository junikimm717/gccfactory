# gccfactory

A factory for
**[Canadian Cross](https://en.wikipedia.org/wiki/Cross_compiler#Canadian_Cross)
GCC toolchains on linux-musl**.

From any Linux system (or container), providing a HOST and TARGET triple hands
you a GCC that runs on HOST and emits code for TARGET. For instance, you can be
on an x86 machine and obtain a toolchain with an aarch64 gcc binary that outputs
code for riscv64.

Output is feature-equivalent to
[musl-cross-make](https://github.com/richfelker/musl-cross-make) plus a static
`make` binary for convenience.

musl-cross-make does a lot of heavy lifting, but its Canadian Cross support is a
risky two-stage manual process that it explicitly does not support. Bridging
that gap was a rabbit hole of build-vs-host-vs- target confusion, sysroots that
don't actually encode where the build system looks, and architecture-specific
musl bugs that only surface in programs the new compiler emits.

I have verified 2 hosts (aarch64 and x86_64) x 11 targets; additional support
for other hosts or targets may come in the future, but I think this covers most
of the systems people will have (the additional arches are for the love of the
game)

Thanks to Claude for very substantial contributions to making this monster work
(and running while I'm asleep to fix all the broken build bugs). It's crazy how
good modern LLM's are and how much they allow one person to do.

## Usage

You probably want the tarballs. Prebuilt toolchains for every proven cell live at
[dev.mit.junic.kim/cross](https://dev.mit.junic.kim/cross/):

```sh
# a riscv64 compiler that runs on your x86_64 box
curl -LO https://dev.mit.junic.kim/cross/x86_64/riscv64-linux-musl-cross.tgz
tar -xzf riscv64-linux-musl-cross.tgz
./riscv64-linux-musl-cross/bin/riscv64-linux-musl-gcc hello.c -o hello
```

One directory per host arch (`x86_64`, `aarch64`), one tarball per target, plus
`SHA256SUMS`. They are relocatable; you do not need to install them to a
specific path.

## Quickstart

```sh
./src/gccf                                                   # help = the docs
./src/gccf doctor                                            # can this box run what it builds?
./src/gccf build --host x86_64-linux-musl --target aarch64-linux-musl
./src/gccf verify                                            # prove it works
./src/gccf pack                                              # tarballs to hand out
```

`./src/gccf build` with no flags opens a two-column picker (hosts | targets).
`--host proven --target proven` builds the whole matrix.

gccf is a Linux tool and assumes it: it needs go, a native toolchain, and
qemu-user-static. It never launches a container for you. If you are not on
Linux, bring your own — `docker/run <cmd>` runs a command inside the builder
image with the repo bind-mounted, which is one way:

```sh
docker/run ./src/gccf build --host proven --target proven
```

Defaults are sized for a small builder and do not auto-scale, so on a large
machine pass `--workers`/`-j` explicitly or most of it will sit idle. Memory,
not cores, is the limit — see `./src/gccf help build`.

## What "proven" means

`verify` is not a smoke test. Every binary in the toolchain is ELF-checked for
the host arch, then the compilers are _executed_ — natively, through
binfmt_misc, or under qemu — to build a probe suite for the target at `-O0` and
`-O2`: libm, pthreads, TLS, atomics, C++ iostreams, exceptions, `std::regex`,
`dlopen` of a `-fPIC` object, `-static`. Each resulting binary is then ELF-
checked for the target and _run_. A cell is only proven if all of that passes.

Pinned upstreams, checksummed and embedded in the binary (`./src/gccf sources`):
gcc 16.2.0, binutils 2.44, musl 1.2.5, make 4.4.1, plus gmp/mpfr/mpc/isl and
kernel headers.

## The build system

`src/gccfactory` is a Go build system over a content-addressed job graph: each
job is keyed by its sources, flags, triples and its dependencies' keys.
Rebuilding with identical inputs is a no-op, changing one configure flag
rebuilds exactly what depends on it, Ctrl-C never leaves a half-built artifact,
and two `gccf` processes may share one `dist/` safely.

Everything the build writes is under `dist/` (gitignored, safe to delete):
toolchains in `toolchains/out/<HOST>/<TARGET>/`, tarballs in
`tarballs/<HOST-ARCH>/`, and a full command-by-command transcript of every job
in `logs/`.

## Learning more

The CLI describes itself with a lot of help messages (I still need to de-slop it
oops):

```sh
./src/gccf help            # overview, the dist layout, supported triples
./src/gccf help build      # every flag, and why each default is what it is
./src/gccf status          # what exists, what's stale, what's building now
./src/gccf logs <slug>     # the transcript of a job, step by step
./src/gccf shell <slug>    # a shell with that job's exact env and cwd
```

Repo layout:

```
src/gccf                   the shim you run
src/gccfactory/            the Go build system
src/gccfactory/internal/   core (DAG+locking) recipe sources ensure triple cli
docker/                    builder image + `docker/run`, for non-Linux hosts
.agents/skills/            hard-won knowledge for whoever works on this next
```

If you are extending this system, read `.agents/skills/toolchain-traps` first —
it is a symptom-to-cause catalogue of the failures this project already hit, and
every entry cost real time to find.
