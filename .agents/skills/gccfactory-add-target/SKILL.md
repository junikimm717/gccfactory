---
name: gccfactory-add-target
description: Add a new target triple to the gccfactory build farm, or promote an existing one from "buildable" to "proven" — the per-arch gcc flags, musl loader name, ELF identity, kernel-header arch mapping, qemu binary name, and the verification that must pass before you claim it works. Use when asked to support or prove a target like mips64, s390x, powerpc64le, riscv32, or arm-linux-musleabihf.
---

# Adding / proving a target

All 11 triples in `triple.Known` are wired and buildable. `triple.ProvenTargets`
is the smaller set exercised end-to-end. Promoting one is mostly verification,
not new code.

## 1. Describe the arch in `internal/triple/triple.go`

Add a `specs` entry. Every field matters and a wrong one fails late and
confusingly:

```go
"riscv64-linux-musl": {emRISCV, 2, 1, "riscv64", "riscv64",
    []string{"--with-arch=rv64gc", "--with-abi=lp64d"}},
//         ^machine ^class ^data  ^qemu     ^ldso    ^gcc configure flags
```

- **machine / class / data** — the ELF identity `ensure.ExpectELF` enforces.
  `class` 1 = ELF32, 2 = ELF64; `data` 1 = LE, 2 = BE.
- **qemu** — the qemu-user suffix, which is *not* always the arch:
  `powerpc64` → `ppc64`, `powerpc64le` → `ppc64le`.
- **ldso** — musl's loader name, giving `/lib/ld-musl-<ldso>.so.1`. Derive it
  from `LDSO_ARCH` in `musl/arch/<arch>/reloc.h`, and mind the ABI suffixes:
  riscv and arm append a float suffix (`-sf`, `-sp`) for soft/single float, so
  the name depends on the `--with-abi`/`--with-float` you chose. A mismatch
  here fails as a `PT_INTERP` check, not as a build error.
- **gcc flags** — `--with-arch`, `--with-abi`, `--with-float`, `--with-mode`.
  musl-cross-make sets *nothing* for riscv, so don't treat its config as
  complete; prefer the ABI the distro world actually uses (`lp64d`/`ilp32d` for
  riscv, `elfv2` for powerpc64).

## 2. Map the kernel-header arch

`linuxArch` in `internal/recipe/config.go`. The sabotage headers tarball's
**top-level** directories are the ARCH namespace — `aarch64`, `x86_64`, `i386`,
`arm`, `mips`, `powerpc`, `ppc64le`, `riscv32`, `riscv64`, `s390`. Verify the
name exists before trusting it:

```sh
tar -tf linux-headers-4.19.88-2.tar.xz | awk -F/ 'NF<=2' | sort -u
```

Do not glob `arch/*` — that is a different, incomplete namespace and is exactly
how musl-cross-make silently ships riscv toolchains with no kernel headers.

## 3. Check upstream actually supports it

Cheap, and saves a wasted 20-minute build:

```sh
# musl has the arch at all?
tar -tzf musl-1.2.5.tar.gz | sed -n 's|.*/arch/\([^/]*\)/$|\1|p' | sort -u
# what loader name does it produce?
tar -xzOf musl-1.2.5.tar.gz musl-1.2.5/arch/<arch>/reloc.h | grep LDSO_ARCH
# qemu binary present?
docker run --rm gccfactory-builder ls /usr/bin/qemu-<qemuname>-static
```

Check `internal/sources/patches/` for arch-specific fixes — binutils 2.44 ships
`riscv-pie-symbol-binding` and `s390x-pie-symbol-binding` patches for a reason.

## 4. Build the cross toolchain first, alone

```sh
./src/gccf build --target <T>            # just cross_<T>, no canadian
```

This is the cheap failure point. If the arch is going to break, it breaks here,
and you have not yet paid for 2 canadian builds on top of it.

## 5. Promote to proven

Add to `triple.ProvenTargets`. Note `proven` is **role-dependent** —
`ProvenHosts` and `ProvenTargets` are separate lists because the brief requires
2 proven hosts but 11 proven targets. `--host proven` and `--target proven`
resolve differently by design; there is a test asserting targets stay a strict
superset.

Then build and prove the full row:

```sh
./src/gccf build --host proven --target <T>
./src/gccf verify --target <T>
```

**If `<T>` is also a proven host, you have created a new diagonal (H == T)
entry — build that pair explicitly.** gcc's configure takes different paths
when host equals target, and the diagonal has its own failure mode that no
H ≠ T toolchain will ever show you (see `toolchain-traps`, the `iostream`
entry). The same warning applies in reverse when adding a *host*: it creates a
diagonal against itself.

```sh
./src/gccf build --host <T> --target <T>
```

## 6. What "proven" must actually mean

Not "it compiled". `ensure.CanadianToolchain` must pass, which means:

- every binary in `<out>/bin` **and** in the tooldir `<out>/<T>/bin` is a HOST ELF
- `<T>-gcc -print-prog-name=as` resolves to an absolute path inside the prefix
- the toolchain runs **under `qemu-<host>`** and emits TARGET binaries whose
  `PT_INTERP` is the expected musl loader
- those binaries **run under `qemu-<target>`** and produce byte-exact expected
  stdout, across the probe matrix: C, libm, pthreads, TLS, atomics, C++
  iostream/exceptions/STL/regex/std::thread, and dlopen — at `-O0` and `-O2`,
  dynamic and static

If a probe is genuinely inapplicable to an arch, give it a `Skip` **reason
string** rather than silently dropping it — skips print as `-`, never as a pass.

## Arch-specific notes

- **riscv32** is the least-trodden of the set. gcc, musl 1.2.5 and binutils
  2.44 all support it, but expect it to surface problems first.
- **32-bit targets** need `-latomic` for 64-bit atomics; the atomic probe already
  adds it conditionally via `ExtraCCFor`.
- **Big-endian** (mips64, powerpc64, s390x) — set `data: 2`. A wrong endianness
  here passes the build and fails only at `ExpectELF`.
- **arm** needs both `--with-float` and the right loader suffix
  (`arm` vs `armhf`); the two must agree or dynamic linking fails at runtime
  only.

See `toolchain-traps` for the failure catalogue and `gccfactory` for the debug
loop.
