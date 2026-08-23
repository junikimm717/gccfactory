---
name: comment-hygiene
description: How to run a comment-elimination sweep across this codebase without destroying knowledge or silently changing code — the delete/keep taxonomy, the AST-equivalence harness that proves only comments moved, how to fan the work out over subagents, and the failure modes that have actually bitten. Use when asked to strip, prune, or clean up comments, when reviewing an AI-authored diff for narration, or before adding a comment to a fix.
---

# Comment hygiene

Two failure modes, opposite directions, both expensive:

- **Narration.** Models emit a comment per line restating the line. It is noise,
  it goes stale, and it buries the comments that matter.
- **Knowledge loss.** This repo's comments encode weeks of toolchain
  archaeology. Deleting `// musl only gives -fPIC to the .lo objects` costs a
  future agent a day of rediscovery.

"When unsure, keep" is the tiebreaker — but it is only for genuine ties, and
it is **not** a licence to keep everything. A sweep run on that instruction
alone removed 1 comment from 13k lines and the repo owner rejected the result.
Apply the test below first; reach for the tiebreaker only when it comes out
genuinely ambiguous.

## The test

Cover the comment. Read only the signature and the body. **Could a competent
Go reader write down the same information from what's left?** If yes, delete
it. A one-line function's body *is* its documentation.

**Go's "every exported identifier gets a doc comment starting with its name"
convention is explicitly overridden in this repo's `internal/` packages.**
These are single-consumer packages and the project principle is to be
self-documenting in as few lines as possible. "It's exported" is never a
reason to keep a comment here, and it is not a tie. Do not cite golint or
godoc, and do not run a linter that complains about missing doc comments.

So `// Pass records a successful check.` on `func (r *Report) Pass(...)` goes.
`// LockPath is the flock file for a job slug. Lock files are never deleted:
removing one would break flock identity for anyone holding it open.` stays —
sentence two is unrecoverable from the code.

### Trim dead leading sentences

The most common shape in this codebase is a doc comment that opens with a
restatement and only then says something real. Delete the opener, keep the
rest, reword minimally:

    // exec runs argv and returns its combined output. A non-zero exit is a
    // normal outcome here (it becomes a failed Check), so Runner.Output is used.
    ->
    // A non-zero exit is a normal outcome here (it becomes a failed Check), so
    // Runner.Output is used.

## Before you start: build the harness

Never sweep without a way to prove you only moved comments. Reviewing a
200-line comment diff by eye does not catch a subagent that "improved" a
variable name along the way.

`stripcmt/main.go` in this skill directory prints every Go file's AST with
comments discarded. Comment-only edits produce byte-identical output.

```
cp -r .agents/skills/comment-hygiene/stripcmt /tmp/sc   # build outside the repo
(cd /tmp/sc && go mod init stripcmt >/dev/null 2>&1; go build -o /tmp/stripcmt .)
/tmp/stripcmt ./src > /tmp/before.txt          # BEFORE any edits
# ... run the sweep ...
/tmp/stripcmt ./src > /tmp/after.txt
diff /tmp/before.txt /tmp/after.txt
```

The diff should be empty, or contain **only blank lines**. `go/printer`
preserves the vertical gap a comment occupied, so deleting a comment that sat
alone between two statements shows up as one removed blank line. That is the
only acceptable difference. Anything with a token in it means a subagent
touched code — find it and revert it.

Then `go vet ./...` and `go test ./...` on the affected packages. Cheap, and it
catches a deleted `//go:` directive.

## The taxonomy

**Delete:**

- Narration restating the next line — `// loop over hosts`, `// return the
  result`, `// set the flag`.
- Step numbering — `// Step 3: build gcc`.
- Section-divider banners and decorative separators.
- Tautological doc comments — `// Build builds.` on `func Build`.
- Explanations of stdlib or Go idioms — `// defer closes the file`.
- Commented-out code.
- Changelog and process narration — `// added to fix X`, `// previously we did
  Y`, `// NEW:`.
- Arrange/Act/Assert scaffolding in tests.

**Keep, verbatim:**

- Anything stating **why**: a constraint, a workaround, an upstream bug, a
  filesystem quirk. Especially the toolchain knowledge — musl's `-fPIC`,
  gcc's host==target divergence, `incpath.cc` search order, binutils installing
  unprefixed tools, gcc PR numbers.
- Concurrency and correctness invariants in `internal/core`: lease/flock
  semantics, heartbeats, atomic-rename publish, what feeds a cache key. These
  are load-bearing; deleting one is how a race gets reintroduced.
- Magic numbers in test expectations, and **why** a test case exists — which
  regression it pins down, why a fake behaves a certain way. That is what gives
  a test teeth.
- Why something is skipped.
- Doc comments on exported identifiers that add information beyond the name.
- Cross-references to skills, docs, upstream issues.
- `//go:` directives, build tags, license headers.
- User-facing rationale in `internal/cli` — the project's goal is a
  self-documenting UI, so a note on why an affordance exists is worth keeping.

Help text, error strings, and test expectations live in **string literals**.
Those are code. Never edit them during a sweep.

## Running the sweep

Partition by package, one sonnet subagent per group, all spawned in a single
message so they run concurrently. Roughly: `recipe`, `ensure`, `cli`, `core` +
`logging` + `triple` + `sources` + `main.go`, and the test files split in two.

Each subagent prompt must carry:

1. Its exact file list, and "edit only these".
2. The absolute rule: only comments and the blank lines they leave behind. No
   renames, no reordering, no reformatting, no "while I'm here" fixes. Say that
   an AST diff verifies this.
3. The delete list and the keep list, with **concrete examples from its own
   files** — a `recipe` agent needs to be told the musl `-fPIC` comment is
   sacred; a `core` agent needs to be told the lock comments are.
4. "When unsure, keep."
5. **"Removing zero from a file is a perfectly good outcome. Do not manufacture
   removals to look productive."** Without this, agents invent work.
6. Use the `Edit` tool, never `sed` or a python string replacement.
7. Report the count and quote every removed comment, then state explicitly
   whether any code was touched.

Have them quote removals. It is how you spot a bad judgement call without
reading the whole diff.

## Traps

**The harness only covers `.go` files.** A sweep once came back with a changed
`go.mod` — the module path had been renamed. `stripcmt` cannot see that, and
neither can a skim of a comment diff. After any sweep, run `git status` and
account for **every** changed file, not just the ones you expected. And check
`git log` before blaming a subagent: that particular `go.mod` change turned out
to be the repo owner's own commit, and reverting it was the actual mistake.

**A big number is not automatically wrong — check what kind.** The narration
class really does get exhausted: a second sweep of this repo found one comment
across ~13k lines. But the *trivial-doc-comment* class was still fully intact
at that point, and a third sweep removed roughly a hundred. So read the
category, not the count. Agents inventing work delete WHY-comments and
rephrase things; agents doing real work delete accessor docs. Both look like
"a big number" from the outside, which is why the report must quote every
removal.

**Blind `str.replace` in a python heredoc silently no-ops** when the anchor
doesn't match, and reports success. Two edits were lost this way. `Edit` errors
when its anchor is missing; use it.

**Do not sweep without a git baseline.** The first sweep here had none, which
is why the AST harness exists at all.

**Actively-wrong comments are the real find.** The first sweep turned up three
comments describing code that no longer existed — a field renamed
`ExtraFor`→`ExtraCCFor`, a stale note about a deleted helper, and `SPEC §1`
references to a document that was never written. Fix these rather than deleting
them; a wrong comment is worse than either a right one or none.

## Writing comments in the first place

Repo rule, from `AGENTS.md`: **when you've found a fix, one line of comment
max.** The impulse after a hard debugging session is to write a paragraph
explaining the whole journey. Don't. One line at the fix, and the full story
goes in `toolchain-traps` where it is searchable by symptom.
