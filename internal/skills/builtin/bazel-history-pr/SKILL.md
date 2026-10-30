---
name: bazel-history-pr
description: Reconstructs the story of one PR inside Bazel — what it set out to do, every
  commit the author pushed, every review and inline thread, and the review→fix loop on one time
  axis — and prints it as a markdown report to read before reviewing. Read-only; it is not a
  review and is never published. Ships inside Bazel and runs headless in the PR's clone. Use it
  as /bazel-history-pr <PR number>.
allowed-tools: Read Write Grep Glob Bash
model: inherit
effort: high
---

# Bazel History PR

> `<plugin>` in the paths below is the folder two levels above this skill's base directory
> (`<plugin>/skills/<this skill>`), or the one the prompt names when the skill came inlined.
> Write it out as an absolute path in every command — the shell runs in the PR's clone.

Reads a pull request end to end and writes down its story: **what it is about**, **how the
author tried to solve it**, and **how the review↔fix loop actually went**. It looks at the
*process*, not at whether the code is right — that is `bazel-review-fleet`.

**Read `<plugin>/skills/bazel-review-fleet/references/headless.md` first** and follow its
sections 1 (nobody answers), 3 (the report is your final output — only its first two bullets:
a history is not a review report and is never validated), 4 (English) and 6 (write nothing
else). Its section 2 does not apply: this skill reads the PR through the API, not the diff.

It is **read-only**. Every `gh` call is a read. Its output is not a review — no findings, no
severities, no verdict — and it must never be published: in Bazel this agent is marked not
publishable, and its report shows up apart from the review.

## Step 1 — Identify the PR

The PR number is the task argument; the repository is the clone's
(`gh repo view --json nameWithOwner --jq .nameWithOwner`) and must match the one the prompt
names, ignoring case (the prompt carries the spelling from Bazel's config). Missing or different → print a short report that says so, in the format of
`references/report-format.md` with whatever header values you have, and stop.

## Step 2 — Collect the raw material

All the commands are in `<plugin>/skills/bazel-history-pr/references/data-collection.md`.
**Read it before running anything** — it has the pagination rules and the two endpoints people
confuse. Seven reads (`gh pr view`, commits, reviews, inline comments, conversation comments,
the timeline and the GraphQL `reviewThreads`), in parallel when you can.

Keep each response as a JSON file under a scratch directory (`$TMPDIR` or `/tmp`), not inside
the clone, and work from the files.

## Step 3 — Build the time axis

One list, every event, ascending — this merged list is the report's spine.

- **Sort commits by `committer.date`, not `author.date`.** A rebase keeps the author date and
  rewrites the committer date; sorting by author date puts a rebased fix *before* the review
  that caused it. Flag any commit whose two dates differ by more than a day.
- **Everything in UTC**, one format.
- **Collapse bots** (`dependabot`, `github-actions`, `coderabbit`, `sonarcloud`…) into one row
  per batch; never let them push a human thread out of the report.
- Record the **gap** between consecutive events.

## Step 4 — Link reviews to fixes

For every inline thread: what was asked, and what answered it? A candidate answer is a commit
that lands after the thread's first comment **and** touches its `path` — verify with
`gh api repos/{owner}/{repo}/commits/{sha} --jq '.files[].filename'`.

| Level | What earns it |
|---|---|
| **confirmed** | the commit message references the thread or reviewer, **or** the commit touches the exact lines of the thread |
| **inferred** | the commit touches the same file after the thread, nothing more specific |
| **unanswered** | no commit touched that path after the thread |

Never write "fixed in `abc123`" for an inferred link. **Resolved does not mean fixed**, and
outdated means even less: report the GitHub flags as what they are, apart from your evidence
level.

## Step 5 — Read the attempt arc as decisions

The first approach; each pivot (revert, file created then deleted, the same file rewritten,
retitle, draft→ready); each change a review asked for and got; the pressure points (files with
the most commits); and what every force-push rewrote. Each of those is a **decision** in the
report: a `###` under `## Decisions`, with the commit that carries it, where its reason comes
from, and the alternatives the record shows were dropped. Describe what the commits show;
never judge the author.

## Step 6 — Print the report

Format and section order: `<plugin>/skills/bazel-history-pr/references/report-format.md`.
Bazel's History tab parses `## What it does`, `## Decisions` and `## Still open` from it, so
those three follow the spec to the letter. Write
it to a scratch file if that helps, then print the **whole report** as your final output, with
nothing around it. If something could not be collected (a deleted fork, force-pushed commits,
a 403), it goes in the report's Gaps section — a named gap is context, a hidden one makes the
rest unverifiable.

Never use the review section names (`## Findings`, `## Cuts`, `## Needs human verification`):
Bazel would read them as review findings.

## Traps

1. **`gh pr view --json comments` is not the review comments.** Conversation comments live in
   `/issues/{n}/comments`; inline review comments live in `/pulls/{n}/comments`. Stop at the
   first and the whole review→fix loop disappears while the report still reads fine.
2. **Pagination and force-pushes** — `data-collection.md` has both: `--paginate` on every list,
   and threads anchored to commits a force-push erased, which are never dropped.
3. **A merged PR is not a finished conversation.** Threads stay open after merge; report what
   was open when it landed.

## Never

- Never post, comment, review or react. Never commit, push or check out another branch.
- Never ask a question.
- Never state an inferred link as fact, or report `isResolved` as "fixed".
- Never drop an event you could not fetch — name the gap.
- Never print anything after the report.
