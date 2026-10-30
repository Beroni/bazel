---
name: bazel-review-fleet
description: Runs three code-review lenses over one PR inside Bazel — precision
  (senior-code-reviewer), adversarial recall (exploit-digger) and over-engineering
  (lazy-senior-dev) — over the same diff, deduplicates their findings and prints a single
  report with one verdict. Ships inside Bazel and runs headless in the PR's clone. Use it as
  /bazel-review-fleet <PR number>. For one lens alone use bazel-senior-code-reviewer,
  bazel-exploit-digger or bazel-lazy-senior-dev; to publish a review use bazel-post-report.
allowed-tools: Read Write Grep Glob Bash Agent
model: inherit
effort: high
---

# Bazel Review Fleet

Three review lenses, one diff, one verdict. Each lens has a different objective function, and
the point of the fleet is that they **disagree productively**:

| Lens | Optimizes for | Hunts |
|---|---|---|
| `senior-code-reviewer` | precision | 11 dimensions under a confidence 8+ gate |
| `exploit-digger` | recall | adversarial bug classes, assuming one exists |
| `lazy-senior-dev` | subtraction | code that should not exist |

**Read `.claude/skills/bazel-review-fleet/references/headless.md` first.** It says how this
runs inside Bazel: nobody answers, the scope comes from the prompt, the report is your final
output, failures are reports too. Everything below assumes it.

## Step 1 — Resolve the scope once

Follow section 2 of `headless.md`: repository, PR number, base, head sha, the
`git diff origin/<base>...HEAD` command and its file list — every path in it, `.claude/` and
other agent configuration included. Empty → failure report, stop.

**All three lenses read this exact scope.** If each lens resolved it on its own and one landed
somewhere else, they would have reviewed different code, and deduplicating or comparing their
findings would mean nothing.

## Step 2 — Choose the lenses

Default: all three. Skip a lens the diff does not justify, and record it in `lenses_skipped`
with the reason: a docs-only diff does not need `exploit-digger`; a one-file, 8-line diff
needs `senior-code-reviewer` alone.

## Step 3 — Run the lenses

The lens instructions are the agent files Bazel wrote into the clone:

| Lens | Agent | File |
|---|---|---|
| `senior-code-reviewer` | `bazel-senior-code-reviewer` | `.claude/agents/bazel-senior-code-reviewer.md` |
| `exploit-digger` | `bazel-exploit-digger` | `.claude/agents/bazel-exploit-digger.md` |
| `lazy-senior-dev` | `bazel-lazy-senior-dev` | `.claude/agents/bazel-lazy-senior-dev.md` |

Every lens gets **the same** scope block, verbatim:

```
Scope for this review (resolved by the orchestrator — use exactly this, do NOT resolve it
again, skip the "Scope resolution" section of your prompt):

  Command: git diff origin/develop...HEAD
  Files (N): src/api/users.ts, src/lib/auth.ts, ...
  PR #123 — "<title>" — base: develop
  PR description: <body, or "no description">

Review that scope with your lens and return ONLY the JSON of your output contract.

Write every prose field of that JSON in English, regardless of the language of this prompt,
of the repo, or of the user's request.
```

The language line is mandatory and goes in literally.

**With the `Agent` tool** (Claude Code): one message, one `Agent` call per lens with its
`subagent_type` and **`run_in_background: false`**, so the three run in parallel and you wait
for all of them in the same turn. Do not let them run in the background and end your turn
with "still waiting": every turn you end is a result Bazel records, and if the run stops there
the saved review is that sentence.

**Without it** (another CLI, or the agent type is unknown): run the lenses yourself as
**sequential passes**. For each lens, read its agent file, apply its instructions to the scope
block above, and write its JSON down before starting the next. Start every pass from the diff,
not from the previous pass's findings: the lenses are only worth combining if each one looked
on its own. Never replace a lens with a generic pass of your own; a lens you could not run goes
to `lenses_skipped` with the reason.

If a lens returns prose or malformed JSON, ask for the object again (or redo that pass). Do not
guess the content.

## Step 4 — Merge and deduplicate

**Same finding** = same file **and** lines within 5 of each other **and** same root cause.
`bazel-post-report` uses the same predicate against the PR's comments; keep them identical.

When two lenses report the same finding, keep one:

- `exploit-digger` vs `senior-code-reviewer` on the same security issue → keep the version with
  the concrete `trigger`, fold in the other's `ref` and `fix`.
- Any lens vs a `lazy-senior-dev` `out-of-scope` item → drop the `out-of-scope`.
- Different severities → **the higher one wins**, and `lenses` lists both. Convergence is
  signal.

A real conflict (one lens wants to cut what another says is load-bearing):

- `lazy-senior-dev` wants to cut validation at a trust boundary, error handling that prevents
  data loss, a security measure, accessibility, or the only executable check → **keep the
  code, drop the cut.**
- Anything else → keep both, each in its section, and let the author decide.

`needs_human_verification` from `exploit-digger` **never** becomes a finding. It goes to
`## Needs human verification`, intact.

## Step 5 — Verdict

The verdict follows what is **left in the report after the merge**, not what each lens voted —
a lens that asked for changes over a cut the merge dropped has nothing left to back it:

- `request_changes` — a `critical` or `major` finding in `## Findings`;
- `comment` — otherwise, any finding, cut or `## Needs human verification` item;
- `approve` — nothing at all, over a scope with files.

Cuts never drive `request_changes`: a simplification does not block a merge.

## Step 6 — Print the report

The format is `.claude/skills/bazel-review-fleet/references/report-format.md`, and only that.
Read it before writing: the header, the title-and-summary block, the three sections, the
closed vocabularies, the level line, `**Trigger:**` and `**Fix:**`. Nothing that is not in the
spec goes into the report — no coverage section, no summary, no notes. `exploit-digger`'s
`checked_ok` and `trust_boundaries` stay out.

Write it to a scratch file and validate it (section 3 of `headless.md`), fix until it
passes, and print the file's content as your final output, with nothing around it.

## Never

- Never ask a question. Never publish, comment, react, commit, push or edit code.
- Never approve on an empty scope.
- Never let one lens's findings into another lens's pass.
- Never turn a `needs_human_verification` item into a finding.
- Never print anything after the report.
