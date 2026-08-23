# GCC Build Farms

Your job is to build a factory for building GCC stacks on linux-musl.
Your build system must be able to execute canadian-cross compilation for
arbitrary architectures to arbitrary architectures.

The final toolchain you output must be as feature complete as the set of tools
shipped by musl-cross-make (github.com/richfelker/musl-cross-make). You should
also make sure to include a copy of the `make` binary.

musl-cross-make only partially supports canadian cross compilation and in a
risky two-stage process with no support. Our job is to bridge this gap and get
full compilation. Feel free to get ideas from it. You need to make sure that
your toolchain binaries

The host architectures you must *prove* support for:
- x86_64
- aarch64

Target triples you must *prove* support for:

1. aarch64-linux-musl
2. arm-linux-musleabi
3. arm-linux-musleabihf
4. i386-linux-musl
5. mips64-linux-musl
6. powerpc64-linux-musl
7. powerpc64le-linux-musl
8. riscv32-linux-musl
9. riscv64-linux-musl
10. s390x-linux-musl
11. x86_64-linux-musl

## Platform specifics

This system only needs to support linux. If you are a mac, you should expect
docker/orbstack to be there. Feel free to spawn up a container with a volume
mount to this directory and run everything there with docker exec. Be cognizant
of the limited resources you have.

If you are on a linux system, you are welcome to use the host gcc.

## The build system

Your programs should be written in ./src, and all build trees and artifacts
must be stored in ./dist. ./dist will be kept with a .gitkeep but will otherwise
be gitignored.

This build system should be built in go and driven by a shell script shim that
sets the correct paths for it to look at. The build system resides at
./src/gccfactory and the orchestration scripts (which are for your convenience
too) may reside in ./src. The go-based build system is allowed to require the following:

1. a working compiler toolchain for native compilation
2. a dist directory
3. the path to a qemu binary for running host-system targeted binaries
4. the path to a qemu binary for running target-system targeted binaries.
5. a matrix of host and target architectures (which will all be linux-musl). If
   not supplied, you may spawn a TUI asking the user to select.

It must have the following:

1. It is idempotent (duh), preserves invariants, and can resume and recover from
   broken states. This must be rigorously tested.
2. It must be able to work with other versions of itself working in the same
   directory; races are not allowed! races must be rigorously tested.
3. It has the ability to check that the given compiler, the cross compiler, and
   the final compiler+toolchains are not broken. You should have a very
   extensive module in ./src/gccfactory/internal/ensure to ensure this.
4. Maximum visibility and log preservation! We want to iterate very rapidly on
   why things are failing. Make it as easy as possible for you to understand why
   things are failing! What is helpful for you will be helpful to the user too!
5. Checksum verification; it is fine to embed this data into the binary as a
   JSON. It should just be easily upgradeable via a script that is inside
   ./src/gccfactory.

## Principles for testing:

When you write tests:

- Never test for coverage! This is one of the worst things AI is known to do for
  tests. It is fine to have some untested code if it is trivial.
- If I report behavioral bugs, you should construct a test that attempts to
  replicate the conditions of the bug. Then, you should write code that
  simulates a user using the system, rather than you carefully manipulating
  code. In this case, it is expected that you call a subagent to roleplay as a
  user who knows nothing and then write the test like that.

When you're driving test runs:

- Recompiles are extremely expensive! Testing small sections or inspecting code
  yourself (or with a subagent) may be less expensive! Only attempt to run the
  full recompile pipeline when you have confidence everything will work end to
  end!

When you've found a fix: please do not put excessive comments around that fix!
one line max.

## Principles for UI

You need to design your system in a way that it is self-documenting and requires
as few lines of documentation for a user to peruse through as possible. This
should be a defining feature of your design plan.

## Principles for development

When you are doing implementation, if you are the top agent, your job is to
supervise subagents. Spawn as many as you need to get the task done as quickly
as possible.

## Skills

Accumulated knowledge for working on this system lives in `.agents/skills/`
(`.claude/skills` is a symlink to it, so both conventions resolve). Read the
relevant one before starting — they exist to stop you rediscovering expensive
failures.

- `gccfactory` — architecture, the build/verify/debug loop, and the invariants
  that must not be broken. Start here.
- `toolchain-traps` — symptom-to-cause catalogue for broken toolchains. Read
  before debugging anything that builds but misbehaves.
- `gccfactory-add-target` — adding a target triple, or promoting one to proven.
- `comment-hygiene` — how to strip comments without losing knowledge or
  silently changing code. Read before any comment sweep.

If you learn something that cost you real time to discover, add it to the
matching skill.
