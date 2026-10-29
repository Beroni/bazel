---
name: bazel-lazy-senior-dev
description: Runs the SUBTRACTION lens over one PR inside Bazel — hunts over-engineering, code
  that should not exist, that reinvents what the repo/stdlib/platform already has, or that fits
  in a fraction of the lines — and prints a report of concrete cuts with the lines they save.
  Ships inside Bazel and runs headless in the PR's clone. Use it as
  /bazel-lazy-senior-dev <PR number>. For bugs and security use bazel-exploit-digger; for all
  three lenses with dedup use bazel-review-fleet.
allowed-tools: Read Write Grep Glob Bash Agent
model: inherit
effort: high
---

# Bazel Lazy Senior Dev

One lens only: **subtraction**. The lens hunts one class of problem, over-engineering, and
returns cuts: what goes, what takes its place, how many lines that saves. It does not look at
correctness, security, validation, performance or accessibility.

Read, in this order, and follow them:

1. `.claude/skills/bazel-review-fleet/references/headless.md` — how this runs inside Bazel.
2. `.claude/skills/bazel-review-fleet/references/single-lens.md` — the three steps: scope,
   lens, report.

Lens specifics:

- Agent: `bazel-lazy-senior-dev`; agent file `.claude/agents/bazel-lazy-senior-dev.md`.
- H1: `# Lazy Senior Dev — …`; `lenses_ran: lazy-senior-dev`.
- Every cut goes in `## Cuts` with its `lines_saved`, biggest first. The ladder `tag` **is**
  the `class`. A cut is never `critical`.
- A bug it tripped over goes in `## Findings` with `class=out-of-scope`. It is a note left in
  passing, not a security review.
- **Arbitrate before printing.** Alone, this lens has no counterweight. Drop any cut that is
  load-bearing: validation at a trust boundary, error handling that prevents data loss, a
  security measure, accessibility, the only executable check. Anything else stays for the
  author to decide.
