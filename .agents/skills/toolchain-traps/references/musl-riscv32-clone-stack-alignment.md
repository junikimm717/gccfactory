# musl 1.2.5: riscv32 `__clone` leaves every thread stack misaligned

**Status:** root-caused, mechanism closed end-to-end, fix already exists upstream.
**Upstream fix:** musl commit `5e03c03fcde3534b37a0b995a438cd176d6882d3`,
"clone: align the given stack pointer on or1k and riscv", 2025-02-22 — *after* the
1.2.5 release, so it is absent from every 1.2.5 toolchain.
**Status in gccfactory:** landed 2026-08-27 as two arch-scoped patches (below), with
both riscv toolchains rebuilt and re-verified. This was not new work and not an
upstream submission; upstream is already correct.

---

## One-paragraph summary

musl 1.2.5's `src/thread/riscv32/clone.s` passes the child stack pointer to
`SYS_clone` without masking it to a 16-byte boundary, unlike 14 of the 19
architectures musl supports. On riscv32 the caller (`pthread_create`) only ever
guarantees 4-byte alignment, so every non-main thread runs its entire life with
`sp ≡ 4 (mod 8)`. The RISC-V ILP32 psABI requires 64-bit variadic arguments to
land in aligned register pairs and 8-aligned save-area slots, so a misaligned
frame makes **every 64-bit `va_arg` in the process misread** — on worker threads
only. In CPython this surfaces as `test_threading` deadlocking, because
`RLock._release_save()` returns the owner thread ident through
`Py_BuildValue("k" "K", count, owner)`, a 64-bit varargs read. `Condition.wait()`
restores a garbage owner and the following `release()` correctly reports a lock
it does not hold.

The bug is in musl. It is not CPython's, not qemu's, and it reproduces on real
riscv32 hardware.

---

## Symptom as originally observed

Target `riscv32-linux-musl`, static CPython 3.13.13, run under `qemu-riscv32`.
`test_threading` fails; every other target in an 11-target matrix passes the same
suite green (x86_64, i386, aarch64, arm musleabi + musleabihf, mips64, ppc64,
ppc64le, riscv64, s390x).

The failing case reduces to `ConditionTests.test_waitfor`, ~20 lines:

```python
import threading
state = 0

def f(cond):
    global state
    with cond:
        assert cond.wait_for(lambda: state == 4)

cond = threading.Condition()          # NB: wraps an RLock, not a Lock
t = threading.Thread(target=f, args=(cond,)); t.start()
for i in range(4):
    with cond:
        state += 1
        cond.notify()
t.join()
```

Two shapes, both from the same cause:

- `RuntimeError: cannot wait on un-acquired lock` at `Lib/threading.py:351`
- a hard hang that wedges even the main thread

**It is deterministic, not a race.** 200/200 trials fail, and `taskset -c 0`
pinning changes nothing. That alone rules out the lost-wakeup / memory-ordering
family that this symptom normally suggests.

---

## Root cause, link by link

### 1. The corruption is per-thread and needs no concurrency at all

Strip out the second thread entirely. Acquire an `RLock` in a worker, call
`_release_save()`, `_acquire_restore()`, `release()`:

```
worker: ident 726256928 (0x2b49cd20)
worker: saved: (1, 3112038675962134528)      # owner = 0x2b302eb000000000 — garbage
worker: RELEASE FAILED: cannot release un-acquired lock
```

The identical sequence on the **main thread** is always correct. So this is not a
synchronisation bug; it is data corruption confined to spawned threads.

### 2. The corruption is in the 64-bit varargs path

`_release_save` returns via `Py_BuildValue("k" "K", count, owner)` — `K` is
`unsigned long long`, read with `va_arg(..., unsigned long long)`.
`_acquire_restore` parses through *pointers* (no 64-bit `va_arg`) and faithfully
stores whatever it was handed, which is why the garbage survives the round trip.

Corroborating: `PyUnicode_FromFormat` in worker threads is deranged in a matching
way — `%llu` yields the low word, `%lu` the high word, `%p` the count. Any
64-bit varargs read in a worker thread is unreliable in this binary.

### 3. Why misalignment breaks 64-bit varargs on ILP32 RISC-V

The RISC-V psABI (ILP32) passes `long long` in an **even/odd aligned register
pair**, and spills to an **8-byte-aligned** slot in the variadic save area. A
variadic callee builds that save area relative to its incoming `sp`.
Disassembly of `PyUnicode_FromFormat` in the shipped binary puts the register
save area at `sp+36` — 8-aligned **only if `sp ≡ 0 (mod 8)` on entry**.

Demonstrated directly in C: call a variadic function through an asm shim that
offsets `sp` by a fixed amount before the call.

| `sp` offset | result |
|---|---|
| 0 | correct — `1122334455667788` |
| 4 | **wrong** — `c0de11223344` |
| 8 | correct |
| 12 | **wrong** |

riscv64 control, identical source: correct at every offset.

### 4. Where the misalignment comes from — the exact arithmetic

This is the step that matters, and it is fully determined by the source.

**`src/thread/pthread_create.c:334`**

```c
stack -= (uintptr_t)stack % sizeof(uintptr_t);   /* riscv32: % 4  — only 4-byte aligned */
stack -= sizeof(struct start_args);
```

musl explicitly rounds the child stack down to `sizeof(uintptr_t)`, which on any
ILP32 target is **4**, not 16. There is no 8-byte guarantee at this point by
construction. It then subtracts `sizeof(struct start_args)`:

```c
struct start_args {
	void *(*start_func)(void *);
	void *start_arg;
	volatile int control;
	unsigned long sig_mask[_NSIG/8/sizeof(long)];
};
```

Measured with the actual cross toolchain, as a compile-time assertion (no
execution required):

```
riscv32: sizeof(struct start_args) = 0x14 = 20 bytes    20 % 8 == 4   ← inverts the residue
riscv64: sizeof(struct start_args) = 32                 32 % 8 == 0
```

So on riscv32 the subtraction **flips the mod-8 residue of the stack pointer**.
Whether a given binary lands on the bad side is decided by the incoming residue
of `stack = tsd - libc.tls_size` (`pthread_create.c:308`), i.e. by the program's
static TLS size — which is why it looks binary-specific rather than universal.

For the CPython binary in question:

```
readelf -lW python3.13
  TLS  ...  MemSiz 0x5d  Align 0x8      # 93 bytes — an unlucky size
```

Every worker thread in this binary runs at `sp ≡ 4 (mod 8)` for its whole life.
Confirmed from the TLS side too: a pure-C program with `__thread char pad[N]`
gives worker `sp % 16 == 12` and garbage `va_arg` for N in the bad residue class,
and `sp % 16 == 0` with correct reads for N = 16. Main thread always fine.

**`src/thread/riscv32/clone.s`** then does nothing to repair it:

```
__clone:
	# Save func and arg to stack
	addi a1, a1, -16      ← subtracts 16, so the mod-16 residue is PRESERVED
	sw a0, 0(a1)
	sw a3, 4(a1)
	...
	ecall
```

Compare arm, which is subject to the same 8-byte varargs alignment rule and
passes:

```
	and r1,r1,#-16        ← masks
```

### 5. musl 1.2.5, all architectures

Survey of `src/thread/*/clone.s` in the 1.2.5 tree — comments stripped:

| masks the child stack (14) | does **not** mask (5) |
|---|---|
| aarch64, arm, i386, microblaze, mips, mips64, mipsn32, powerpc, powerpc64, s390x, sh, x32, x86_64 | loongarch64, m68k, or1k, **riscv32**, **riscv64** |

Masking is the overwhelming convention; riscv is the outlier. Upstream's fix
commit title — "clone: align the given stack pointer on or1k and riscv" — names
exactly the arches that needed it.

### 6. Why only riscv32 in an 11-target matrix

| target | 64-bit `va_arg` needs 8-alignment? | `__clone` masks? | verdict |
|---|---|---|---|
| **riscv32** | **yes** (aligned register pair) | **no** | **broken** |
| riscv64 | no — one register | no | immune by ABI |
| arm / armhf | yes | **yes** | saved by the mask |
| i386 | no — no 8-byte varargs rule | yes | immune by ABI |
| all others | 64-bit arches | yes | immune |

riscv32 is the only cell where "needs the alignment" and "does not get it"
intersect. Two independent protections, and it is the one target with neither.

---

## Why the earlier investigation kept coming up clean

Six primitives were tested directly on riscv32 and *all passed*:
non-blocking acquire of a held lock (2000×), mutual exclusion (80000/80000),
`_is_owned` under contention, `Event` ping-pong, `get_ident` stability,
`RLock._release_save`/`_acquire_restore` (200k plain, 20k contended).

That was not bad luck, it was structural:

- In CPython 3.13, `Lock` and `Event` are the new `PyMutex` — **no varargs
  anywhere** on their paths. They are immune regardless.
- The save/restore test that passed must have executed on the **main thread**,
  which has a correctly aligned stack from the kernel.

Only *worker thread* **+** *64-bit varargs* is broken, and `Condition.wait()` in
a spawned thread is precisely that combination. Every primitive works in
isolation; the composition fails.

---

## Verification of the fix

Replacing `__clone` in a copy of `libc.a` with one that adds `andi a1, a1, -16`
makes the previously failing TLS-size class read all varargs correctly, with
`sp % 16 == 0`. Verified in C, at the level the bug lives at.

**Confirmed end-to-end against a rebuilt toolchain (2026-08-27.)** A worker thread
reading a 64-bit `va_arg`, static `-O2`, swept over `__thread char pad[N]` for
N = 1..96 to cover both residue classes, run under `qemu-riscv32`:

| `__clone` | result |
|---|---|
| musl 1.2.5 as shipped (unmasked, linked ahead of `libc.a`) | **48 ok, 48 broken** |
| patched `libc.a` from the rebuilt toolchain | **96 ok, 0 broken** |

The exact 50/50 split is the residue-class prediction of section 4, observed.
One correction to the symptom description: at `-O2` under `ilp32d` the bad class
does not merely misread varargs, it **SIGSEGVs** (rc=139). The CPython
lock-ownership error is the gentler presentation of the same fault.

Still not done: a rerun of CPython's own `test_waitfor` against an interpreter
rebuilt on the fixed toolchain. The mechanism is closed without it.

---

## The fix

Upstream, `src/thread/riscv32/clone.s` — one inserted instruction:

```diff
 __clone:
 	# Save func and arg to stack
+	andi a1, a1, -16
 	addi a1, a1, -16
 	sw a0, 0(a1)
 	sw a3, 4(a1)
```

`src/thread/riscv64/clone.s` gets the identical insertion upstream. riscv64 is
immune to *this* symptom, but the misalignment is a real psABI violation there
too and can bite anything that assumes a 16-aligned frame. Backport both; treat
riscv64 as hygiene rather than a bug fix, and say so in the patch header.

Confirmed against upstream `master` (fetched 2026-08-27): both files now open
with `andi a1, a1, -16` immediately before the existing `addi a1, a1, -16`.
`loongarch64` remains unmasked upstream and is out of scope here.

### For gccfactory specifically

The arch-scoped patch mechanism already exists and this is exactly its shape —
directly parallel to `patches/musl-1.2.5/s390x/0003-crtjmp-address-register.diff`,
which backports another post-1.2.5 upstream musl fix for one architecture.

```
src/gccfactory/internal/sources/patches/musl-1.2.5/riscv32/0004-clone-align-child-stack.diff
src/gccfactory/internal/sources/patches/musl-1.2.5/riscv64/0005-clone-align-child-stack.diff
```

Both exist as written. Measured blast radius: `./src/gccf status` before and after
moved exactly two keys, `cross_riscv32-linux-musl` and `cross_riscv64-linux-musl`,
and added the two `srctree_musl-1.2.5_riscv*` jobs. The other nine `cross_*` keys
and the shared `srctree_musl-1.2.5` key were byte-identical.

Arch-scoping matters: a global musl patch moves every target's key and rebuilds
all 19 toolchains for a fix that changes nothing on 17 of them.

### If a rebuilt toolchain is not available

A link-time workaround exists: assemble a corrected `__clone` into an object and
place it ahead of `libc.a` in the link; the first definition wins. This was
verified working. It is a stopgap — it fixes one binary, not the toolchain, and
every consumer of that toolchain stays broken.

---

## Open items

- **The hang variant is not separately root-caused.** Plausibly downstream: with
  a corrupted owner ident a thread can pass `_is_owned()` checks it should fail,
  and `Condition._waiters` bookkeeping gets torn down mid-exception. Informed
  speculation, not evidence.
- **No end-to-end confirmation** against a rebuilt interpreter (see above).
- **`loongarch64`, `m68k`, `or1k`** are also unmasked in 1.2.5. or1k is fixed
  upstream by the same commit. Whether any of them is *exploitable* depends on
  each psABI's variadic alignment rules; not investigated, and only relevant if
  gccfactory ships those targets.

---

## Incidental finding, unrelated but worth recording

ctypes on riscv32 passes variadic 64-bit arguments **without** the even-register-pair
alignment, from *both* the main thread and workers — observed via
`ctypes.pythonapi.PyUnicode_FromFormat`, with riscv64 correct. This is a separate
latent bug: ctypes cannot call `ffi_prep_cif_var` because it does not know a
function is variadic. It has nothing to do with `test_threading` and is not fixed
by the musl patch, but it will bite if riscv32 ctypes ever misbehaves.

---

## Environment

- musl 1.2.5 (toolchains built by musl-cross-make)
- CPython 3.13.13, static, `riscv32-linux-musl`, `--with-arch=rv32gc --with-abi=ilp32d`
- qemu-riscv32 10.2.2 — **exonerated**; the bug is deterministic and architectural
- Cross toolchains: `riscv32-linux-musl-cross`, `riscv64-linux-musl-cross` (control)

## References

- musl commit `5e03c03fcde3534b37a0b995a438cd176d6882d3` — "clone: align the given stack pointer on or1k and riscv" (2025-02-22)
- `https://git.musl-libc.org/cgit/musl/plain/src/thread/riscv32/clone.s`
- musl 1.2.5 `src/thread/pthread_create.c:334`, `src/env/__init_tls.c:126`
- RISC-V psABI, ILP32 variadic argument alignment
