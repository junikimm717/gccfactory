---
name: gccfactory
description: Work on the gccfactory canadian-cross GCC build farm in this repo — its architecture, the build/verify/debug loop, how the ensure verification suite proves a toolchain works, the measured --workers/-j parallelism and memory budget, where logs and artifacts live, how to iterate on a failing build without paying for a full recompile, and which invariants must never be broken. Start here for ANY question about this repo — it has a lookup table that answers most of them without scanning the codebase. Use whenever touching src/gccfactory, running ./src/gccf, adding a recipe or job type, or investigating why a toolchain build failed.
---

# gccfactory

Builds **canadian-cross** toolchains: compilers that RUN on HOST and EMIT code
for TARGET, built on BUILD. `./src/gccf help` is the user-facing documentation
and is kept authoritative — read it before writing new docs.

## Orientation

```
src/gccf                 bash shim; the ONLY entry point. Linux-only by
                         design — it never launches a container itself.
docker/Dockerfile        debian bookworm arm64 + qemu-user-static + go
docker/run               opt-in escape hatch: run a command in that image
                         with the repo bind-mounted at /w. Use this when
                         working from macOS.
src/gccfactory/          the go module
  internal/triple/       the 11 supported triples; ELF identity, loader paths,
                         per-arch gcc flags. Shared vocabulary — edit carefully.
  internal/core/         job graph, Merkle keys, flock leases, atomic publish,
                         Runner (every command is logged)
  internal/sources/      pinned tarballs + sha256 + embedded patches
  internal/recipe/       srctree / cross / hostmake / canadian jobs
  internal/ensure/       proves a toolchain actually works
  internal/cli/          commands + the dependency-free picker TUI
dist/                    everything generated; gitignored
```

## Answer questions from here, not from a scan

Grepping the tree to answer a basic question is slow and expensive. Almost
everything is one of these:

| question | where the answer is |
|---|---|
| what commands exist, what a flag does | `./src/gccf help`, `./src/gccf help <cmd>` — **authoritative**, kept current |
| what's built / stale / building right now | `./src/gccf status` |
| how many cores, how much RAM a job needs | *Parallelism and resources*, below |
| what "verified" actually means | *How verification works*, below |
| why a built toolchain misbehaves | `toolchain-traps` skill — symptom-first |
| how to add or prove a triple | `gccfactory-add-target` skill |
| why a specific configure flag is there | `internal/recipe/config.go` — one file, all flags |
| the supported triples and their ELF identity | `internal/triple/triple.go` |
| what a job did, in order | `dist/logs/jobs/<slug>/latest/commands.sh` |

The `Long:` field of each command in `internal/cli/*.go` is the real user
documentation — it explains rationale, not just syntax. Read that before
reading implementation, and update it when behaviour changes.

## The DAG

```
srctree_<pkg>-<ver>      fetch -> verify sha256 -> extract -> patch -> config.sub
  cross_<T>              BUILD->T toolchain (binutils, gcc, musl, kernel headers)
  hostmake_<H>           static GNU make for H            (needs cross_<H>)
  canadian_<H>__<T>      the deliverable  (needs cross_<H>, cross_<T>, hostmake_<H>)
```

`cross_<X>` is shared: it serves as the host compiler when X is a host and as
the target compiler when X is a target.

**Why `canadian` works when musl-cross-make's doesn't:** it seeds its output
with the *target-side* files copied from `cross_<T>` (musl sysroot, libgcc,
libstdc++, crt objects — all host-independent), then builds only host code via
gcc's top-level `make all-host` / `install-host`. The canadian build therefore
never tries to run a HOST-arch `xgcc` on the BUILD machine, which is the
classic canadian trap. Target libraries are built once per target instead of
once per (host, target) pair.

## Parallelism and resources

Two independent knobs, and the interesting part is that the defaults are
**measured, not guessed** — on the 8-core / 7.8 GB builder container.

| knob | default | meaning |
|---|---|---|
| `--workers N` | 1 | whole jobs built concurrently |
| `-j N` | 6 | `make` parallelism *inside* one job |

A `cross_<T>` job peaks at **4.0 GB** (during `all-target-libstdc++-v3`) and a
`canadian_<H>__<T>` job at **~3.0 GB**. Two concurrent gcc builds therefore
swap on a 7.8 GB box, which is far slower than running them in series. One job
at `-j6` is the fastest setting that fits. A `cross_` toolchain takes ~8 min;
the full 8-cell matrix took 1h17m.

**With more RAM, raise `--workers` before `-j`.** Job-level parallelism scales
better than make-level here: gcc's build has long serial stretches where `-j`
buys nothing, so a second worker fills the cores that `-j6` leaves idle.
`-j` above the core count only adds memory pressure.

`Env.Workers()` clamps to ≥1 and `Env.MakeJobs()` falls back to the CPU count,
so neither can be set to something degenerate.

## How verification works

`internal/ensure` is the answer to "did we actually build a working compiler,
or just a directory that looks like one?" It never trusts exit codes — it
compiles code, checks what came out is the right kind of binary, then **runs
it** and compares stdout byte for byte.

Three levels, increasing in strength:

| level | proves |
|---|---|
| `NativeToolchain` | the BUILD machine's own gcc/g++ can compile and run C and C++. Output is not ELF-asserted (BUILD is whatever the container is). |
| `CrossToolchain` | a BUILD→T toolchain: its binaries are BUILD ELFs, the tool surface is complete, probes compile for T and run under `qemu-<T>`. |
| `CanadianToolchain` | the real proof: every binary in `<prefix>/bin` is a **HOST** ELF, those binaries **run under `qemu-<host>`**, what they emit is a **TARGET** ELF, and that runs correctly under `qemu-<target>`. |

The canadian level is the one that matters, and the nesting is the trick: a
compiler for HOST is executed under one qemu, and its output under another.
That is what catches a "canadian" build that silently produced BUILD binaries.

**The probe suite** (`internal/ensure/probes/*.c`, embedded via `go:embed`).
Each probe is a self-checking program: compile it, assert the ELF identity of
the result, run it, require its stdout to equal `Probe.Want` exactly.

    hello  math  pthread  tls  atomic  static
    hello++  except  stdcxx  lto  dlopen  static-pie

Ordered cheapest-and-most-fundamental first, so a broken toolchain fails fast
and legibly rather than in a wall of noise. Each runs at `-O0` and `-O2`, and
in every link mode it allows — 22 cells per opt level (10 probes × dynamic +
static, plus `dlopen` and `static-pie` which are one mode each).

**Beyond the probes**, and these catch the subtle failures:

- `HostBinDirReport` — every file in `<prefix>/bin` is an ELF for HOST.
- `ToolSurface` — every target-prefixed tool musl-cross-make ships is present
  and executable, plus `make` for a canadian deliverable.
- `ToolDirBinReport` — `<prefix>/<T>/bin` (the tooldir gcc *actually* searches
  for as/ld) holds binaries for the machine the compiler runs on. Missing
  `as`/`ld` is fatal: gcc would silently fall through to the build machine's
  assembler on `$PATH`.
- `progNames` — asks gcc where its assembler and linker are; a bare `as` means
  the toolchain is not self-contained.
- `ExpectELF` — a dynamically linked result's `PT_INTERP` must be musl's
  loader, which is what catches a glibc-linked binary.
- `LTOPluginReport` — records whether `liblto_plugin.so` shipped; never fails
  on its own, because only the LTO probes can prove LTO works.

Results are a `Report` of named `Check`s (pass / fail / **skip**, printed
distinctly so a skip is never mistaken for a pass). A green matrix run is 118
checks, 0 failures, 2 known lto-plugin skips.

Run it with `./src/gccf verify` — with no flags it verifies everything
currently built in `dist/`, so it is always a safe thing to type.

## The debug loop

Recompiles are expensive. Never rerun a full build to test a hypothesis.

```sh
./src/gccf status                  # what exists / is stale / is building now
./src/gccf logs <slug> --failed    # the failing step, with cwd + env + tail
./src/gccf logs <slug> --step gcc-configure
./src/gccf shell <slug>            # a shell in that job's env and build dir
```

Every job attempt writes `dist/logs/jobs/<slug>/latest/`:
- `NNN-<step>.log` — one per command, each with a `# cwd:` / `# env:` / `# cmd:`
  header, then output, then exit code and duration
- **`commands.sh`** — an executable replay of every command the job ran, in
  order. This is the fastest way to reproduce a failure by hand: copy the one
  failing command out of it and iterate in `./src/gccf shell <slug>`.

Failed jobs keep their work tree. Successful ones delete it unless you pass
`--keep-work` (or set `GCCFACTORY_KEEP_WORK=1`).

Escape hatches, both for bisecting only — never leave them on:
- `GCCF_SKIP_VERIFY=1` — publish an artifact without proving it works
- `GCCFACTORY_KEEP_WORK=1` — retain work trees

## Invariants — do not break these

1. **`--prefix=` is the empty string**, `--libdir=/lib`, `--with-sysroot=/<T>`,
   install via `DESTDIR`. The empty prefix is what makes gcc and ld define
   `TARGET_SYSTEM_ROOT_RELOCATABLE`; an absolute prefix silently produces a
   toolchain that only works at its build path. Never "fix" this.
2. **Jobs write only into `work` and `stage`.** Core stamps `.gccfactory.json`
   itself — a recipe must never write it. A directory carrying a manifest is by
   construction complete.
3. **Every command goes through `r.Run(ctx, core.Cmd{...})`.** Anything exec'd
   outside the Runner is invisible in logs and missing from `commands.sh`.
4. **`KeyInputs()` must be deterministic and complete** — recipe version, every
   source sha256, every configure flag, the triples. Absolute build-machine
   paths must NOT leak in (the recipe uses `@BUILD@`/`@WORK@`/`@STAGE@`
   sentinels for this; there is a test asserting it).
5. **Bump `recipe.Version` by hand** when a recipe changes in a way the flags
   don't capture. That invalidates every downstream key.
6. Locks in `dist/locks/` are never deleted — deleting one breaks flock
   identity and the mutual exclusion that goes with it.

## Concurrency model

Content-addressed Merkle keys + flock leases + atomic rename publish. Two
processes may share one `dist/` safely; this is tested with real subprocesses,
not just goroutines.

- build lease = `LOCK_EX` on `dist/locks/<slug>.lock`, **re-check validity after
  acquiring** (another process may have just built it)
- a job holds `LOCK_SH` on every dependency for its whole build
- publish = write manifest last, then rename old aside and rename staging in
- crash recovery is free: the OS drops the flock, and a partial artifact can
  never become visible because publish is a rename

If you add a job type, implement `core.Job` and nothing else — the engine
handles keying, locking, publishing, logging and GC.

## Testing rules (from CLAUDE.md, restated because they are often violated)

- **Never test for coverage.** Trivial code may go untested.
- Concurrency and recovery are tested with real subprocesses and real
  `SIGKILL`, with fake jobs — never a real compiler.
- Recipe tests exercise the **pure config functions** (`binutilsConfig`,
  `gccConfig`), asserting flag shape and triple wiring. They must not compile
  anything.
- For a reported behavioral bug, reproduce the user's conditions — spawn a
  subagent to roleplay a naive user and write the test from that, rather than
  hand-crafting internal state.

## Related

- `toolchain-traps` — symptom-to-cause catalogue for broken toolchains. Read it
  before debugging any "the compiler builds but produces wrong output" problem.
- `gccfactory-add-target` — adding and proving a new target triple.
