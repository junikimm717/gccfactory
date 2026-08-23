# gccfactory

A factory for **canadian-cross GCC toolchains on linux-musl**: compilers that
run on an arbitrary HOST triple and emit code for an arbitrary TARGET triple,
built on whatever machine you have. Output is feature-equivalent to
[musl-cross-make](https://github.com/richfelker/musl-cross-make), plus `make`.

Thanks to Claude for very substantial contributions to making this monster work.
It's crazy how good modern LLM's are.

## Quickstart

```sh
./src/gccf                                                   # help = the docs
./src/gccf build --host x86_64-linux-musl --target aarch64-linux-musl
./src/gccf verify                                            # prove it works
```

`./src/gccf build` with no flags opens a two-column picker (hosts | targets).

gccf is a Linux tool and assumes it is on Linux: it needs go, a native
toolchain, and qemu-user-static. It never launches a container for you. If you
are not on Linux, bring your own — `docker/run <cmd>` runs a command inside the
builder image with the repo bind-mounted, which is one way:

```sh
docker/run ./src/gccf build --host proven --target proven
```

Toolchains land in `dist/toolchains/out/<HOST>/<TARGET>/`.

Defaults are sized for a small builder and do not auto-scale, so on a large
machine pass `--workers`/`-j` explicitly or most of it will sit idle. Memory,
not cores, is the limit — see `./src/gccf help build`.

## dist/

Everything the build writes is under `dist/` (gitignored, safe to delete):

```
dist/
  toolchains/out/<HOST>/<TARGET>/   the deliverable
  toolchains/cross/<TARGET>/       build->target toolchain, shared between matrices
  toolchains/hostmake/<HOST>/      static GNU make for that host
  src/                             checksum-verified upstream tarballs
  srctrees/<pkg>-<ver>/            extracted + patched sources, built once
  logs/jobs/<slug>/                every command a job ran, with cwd + env
  logs/runs/<stamp>-<pid>/         structured event stream per invocation
  work/ .staging/ .trash/ locks/ state/   scratch and coordination
```

Jobs are content-addressed: rebuilding with unchanged inputs is a no-op, and
changing one configure flag rebuilds exactly what depends on it. Two `gccf`
processes may share one `dist/` safely.

## Learning more

There is no other documentation on purpose. The CLI describes itself:

```sh
./src/gccf help            # overview, the dist layout, supported triples
./src/gccf help build      # every flag, and why each default is what it is
./src/gccf status          # what exists, what's stale, what's building now
./src/gccf logs <slug>     # the transcript of a job, step by step
./src/gccf shell <slug>    # a shell with that job's exact env and cwd
```

## Layout of this repo

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
