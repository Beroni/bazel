---
name: bazel-senior-code-reviewer
description: Runs the PRECISION lens over one PR inside Bazel — 11 dimensions (OWASP/Web3
  security, correctness and test validity, performance, quality, architecture, migration,
  idioms, accessibility, docs, hygiene) under a confidence 8+ gate — and prints a report with a
  few defensible findings. Ships inside Bazel and runs headless in the PR's clone. Use it as
  /bazel-senior-code-reviewer <PR number>. For all three lenses with dedup use
  bazel-review-fleet.
allowed-tools: Read Write Grep Glob Bash Agent
model: inherit
effort: high
---

# Bazel Senior Code Reviewer

One lens only: **precision**. The lens sweeps the diff across 11 dimensions and records only
what it can defend at confidence 8/10 or higher. Five real findings are worth more than forty
speculative ones.

Read, in this order, and follow them:

1. `.claude/skills/bazel-review-fleet/references/headless.md` — how this runs inside Bazel.
2. `.claude/skills/bazel-review-fleet/references/single-lens.md` — the three steps: scope,
   lens, report.

Lens specifics:

- Agent: `bazel-senior-code-reviewer`; agent file
  `.claude/agents/bazel-senior-code-reviewer.md`.
- H1: `# Senior Code Reviewer — …`; `lenses_ran: senior-code-reviewer`.
- The lens returns a `dimension` letter (A–K) and a `class`. Only `class` goes into the report.
- Sections it may write: `## Findings` only.
- `findings: []` with `approve` is a real result under the 8+ gate, not "nothing was looked at".
