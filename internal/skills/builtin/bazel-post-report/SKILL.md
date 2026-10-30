---
name: bazel-post-report
description: Publishes to GitHub a review Bazel has already produced and a person has already
  read on screen — the markdown whose path comes in the prompt. Posts a single review with
  inline comments, reacts 👍 to what is already on the PR instead of repeating it, stands down
  on findings the team already settled in a resolved or replied-to thread, and posts an
  all-clear when there were no findings. Ships inside Bazel; not the same as a `post-report`
  you may have installed, which resolves the file on its own and asks before posting.
allowed-tools: Read Grep Glob Bash
model: inherit
effort: high
---

# Bazel Post Report

Takes to the PR a review that **already exists and a human already approved**. Bazel ran the
agent, saved the markdown, showed it on screen, and the person clicked publish. Your job starts
there: turn that file into a review on GitHub.

From `.claude/skills/bazel-review-fleet/references/headless.md`, sections 1 (nobody answers)
and 4 (English) apply. Its sections 2, 3, 5 and 6 do not: you publish a review that already
exists, you never run the validator, and what you print is what you did (Step 6), not a
report. The `gh` commands and the exact shape of every comment are in
`.claude/skills/bazel-post-report/references/publishing.md`; read it before Step 2.

## What was decided before you

None of these is yours to redo:

- **The review was read.** Do not review the code, do not look for new findings, do not
  improve the prose. What is in the file is what the person read and approved.
- **Publishing was confirmed.** Bazel asked on its page before starting you. **Do not ask for
  confirmation** — there is no terminal; a question either hangs or is swallowed.
- **The findings were filtered.** A finding the person unticked is already out of the file.
  Publish what is in it, all of it.

## The file

Its path comes in the prompt. It is Bazel's header, a `---` line, then the agent's report:

```markdown
# acme/api-core#482 — Add refresh-token rotation
- Author: @maria
- Branch: `feat/refresh`
- URL: https://github.com/acme/api-core/pull/482
- Head sha: a1b2c3d4e5f6...
- Diff: +240 −18 across 7 file(s)
- Reviewed at: 2026-09-11 14:32 (took 4m12s)
- Agent: bazel-review-fleet

---

<the agent's report>
```

The first line gives **repo and PR number**. `- Head sha:` gives the commit that was
reviewed. Only what comes after the first `---` line is the report, and only the report
becomes comments — the header is Bazel's metadata.

The report is normally in the format of
`.claude/skills/bazel-review-fleet/references/report-format.md`: a `<!-- review-fleet:v1 … -->`
header and, under each `###`, a `<!-- finding … -->` line with `path`, `line`, `side`. **Do not
run `validate-report.sh` on this file**: when the person unticks every finding of a section,
the section is left empty, which the validator rejects although the file is exactly what was
approved. Read the metadata directly instead.

## Step 1 — Gates

Each one **stops the publication**. None becomes a warning. When one stops you, write to
stdout which one and why, and publish nothing.

- **No `# owner/repo#N` on the first line** → you do not know where to publish.
- **Repo different from the clone's** (`gh repo view --json nameWithOwner --jq .nameWithOwner`)
  → you are about to comment on another project. Compare ignoring case: Bazel writes the repo
  as it was typed in its config (`beroni/bazel`), GitHub answers with its own spelling
  (`Beroni/bazel`), and both name the same repository.
- **`files: 0`** in the report's header → the review never resolved a scope. Publishing it, or
  an all-clear on top of it, would tell the team code was reviewed when none was.

Then the **sha gate**. Compare the reviewed sha with the PR's current HEAD:

```bash
gh pr view <N> --json headRefOid --jq .headRefOid
```

The reviewed sha is `- Head sha:` in Bazel's header; when the report has its own `head_sha:`,
both must match the HEAD. Any mismatch, or a header without `- Head sha:` (a review saved by an
old Bazel), means a commit may have landed after the review: lines moved, and an anchor that was
right now points at the wrong code. **Do not post inline.** Fall back to a single body comment
(Step 5) that says which sha was reviewed. Never anchor to a line you cannot prove is the same
code.

## Step 2 — Survey what is already on the PR

Before assembling any payload: **the three calls in `publishing.md`, all three.** The REST
comment list alone cannot tell a resolved thread (that exists only in GraphQL) and cannot tell
a reply from an original comment unless you select `in_reply_to_id`. Run the survey short and
you re-raise what the team already closed.

## Step 3 — Classify every finding

Every finding lands in **exactly one** destination. Classify all of them before building any
payload, checking in this order — the first row that matches wins:

| Situation | Destination |
|---|---|
| Already on the PR **and settled** (thread resolved, or somebody replied) | **Nothing written, no 👍.** Out of the verdict; listed as already settled. |
| Already on the PR | **👍 on the existing comment.** Nothing posted. |
| No `path`/`line` (`-`), sha mismatch, or a line outside the diff | the review body |
| Everything else | an inline comment |

The order matters: a settled finding is also a duplicate, and checking duplicates first gives it
a 👍 it should never get.

**Same finding** = same file **and** lines within 5 of each other **and** same root cause. It
counts the same whether the existing comment is someone else's or from an earlier run.
Different wording of the same bug **is** a duplicate — that is the case that fools you most.
The same file with two different bugs is not. `bazel-review-fleet` deduplicates its lenses with
this same predicate.

**Settled** means the matching comment sits in a thread that was **resolved**, or that somebody
**replied to**. That is a decision the team took in writing; you do not reopen it. A settled
finding writes nothing to GitHub, does not count toward the published verdict (two settled
`critical`s do not block a merge the team unblocked), and is listed — file, line, who decided —
in the review body and in your stdout. `isOutdated` is **not** settlement; it only means the
lines moved.

Filtering duplicates is not filtering severity: an unsettled `critical` already on the PR still
counts toward the verdict in the body. It just does not become a second comment.

### Where `path` and `line` come from

From the `<!-- finding … -->` line right below each `###`, verbatim: `path`, `line`, `side`
(`RIGHT` by default, `LEFT` for a removed line), and `start_line` when the finding spans a
range. The comment body is the finding's prose verbatim — it already opens with its level line
(`**critical · authz** (senior-code-reviewer + exploit-digger)`). Append `ref` as a trailing
line when there is one; `confidence` stays out of the PR.

A report that does not follow the format leaves the anchor in the prose
(`src/api/transfer.ts:88`). You may take it from there, but **check that the file and the line
are in the diff** before anchoring:

```bash
gh pr diff <N> --patch | grep -n "^+++ b/src/api/transfer.ts"
```

In doubt, the body. A finding in the body is read; a finding anchored to the wrong line sends the
author to unrelated code.

### What never becomes a comment

`## Needs human verification` **never** becomes a finding. Its items go into the review body as
open questions, or stay out. They exist because the lens could not confirm them.

## Step 4 — Everything that goes to the PR is in English

The body, the inline comments, the headers, the all-clear — all in English, whatever the
language of the report, of the repo, of the prompt or of the user's own instruction files. The
report is normally born in English; **sweep the payload before sending and translate whatever
is left.** The comment is read by the team and stays in the repo's history.

## Step 5 — Publish

Commands and payload shapes: `publishing.md`.

- **A single review** with all inline comments (`POST /pulls/<N>/reviews`), not N loose
  comments: one notification instead of N.
- **`event: COMMENT`, always.** Never `REQUEST_CHANGES` — that blocks the merge in the user's
  name. Never `APPROVE`, not even when the report says `approve`. The verdict goes written in
  the body.
- The body carries: the verdict (the report's, and the published one when settled findings
  changed it), the scope line from the report's header (`scope_cmd`, `files`, `lenses_ran`),
  findings that go to the body, the open questions, the already-settled list, and
  ``Reviewed at `<short sha>`.``
- If the API rejects **one** comment for an invalid line, it rejects the **whole review**.
  Remove that comment, send the rest, and say which one fell out.
- **Sha mismatch** → no review with inline comments: a single `gh pr comment` with the report,
  opening with ``> Reviewed at `a1b2c3d`; the PR has moved since, so findings are not
  line-anchored.``
- **Zero findings** (the report has none) → post the all-clear from `publishing.md`, with the
  coverage line from the report's header and its sha marker. Look for the marker first: same
  sha already cleared → 👍 it, do not post another; different sha → post a new one and never
  edit or delete the old.
- **Every finding a duplicate** → only the 👍s. No empty review, and no all-clear: there were
  findings, just already said.
- **Every finding settled** → nothing is written at all, not even a 👍.

## Step 6 — Say what happened

Your stdout goes back to the review's card in Bazel. End it with the real count, in markdown:

```
6 findings: 3 posted inline, 1 was already on the PR (👍 on @someone's comment),
2 were already settled (thread resolved by @someone on src/api/transfer.ts:88).
Published verdict: comment — the report said request_changes, and both criticals were among
the settled ones. The review body carried the verdict, the settled list and 2 open questions.
```

If a gate stopped you or the API refused something, say which and why.

## Never

- Never ask. Bazel already confirmed.
- Never `APPROVE` or `REQUEST_CHANGES` in the user's name.
- Never repost a finding already on the PR — not reworded, not expanded.
- Never re-raise or 👍 a finding whose thread was resolved or replied to.
- Never reply to, resolve or close an existing thread. The 👍 is the only write you make on
  someone else's comment.
- Never edit or delete an all-clear from an earlier sha.
- Never anchor to a line when the sha does not match.
- Never review the code yourself or add a finding that was not in the file.
- Never post in a language other than English.
- Never edit files, commit, push or open a PR.
