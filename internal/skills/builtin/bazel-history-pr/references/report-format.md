# History report format

What `/bazel-history-pr` prints as its final output. Bazel saves it like any agent's output
and shows it apart from reviews: it is not a review, and it is never published.

This report makes no claim about code correctness and must never reach a PR as if it were a
review. It never uses the review section names (`## Findings`, `## Cuts`,
`## Needs human verification`).

Written in English, always, regardless of the language of the request.

## Header block

First thing in the file, before any prose. One key per line:

```markdown
<!-- history-pr:v1
repo: owner/name
pr: 123
title: Add refresh-token rotation
state: MERGED
author: someuser
base: develop
head_sha: a1b2c3d4e5f67890abcdef1234567890abcdef12
created: 2026-03-02T09:14:00Z
merged: 2026-03-08T16:02:00Z
commits: 48
reviews: 9
threads: 11 (7 resolved, 4 open)
force_pushes: 2
generated_at: 2026-03-11T12:00:00Z
-->
```

It is the answer to "is this report still current?" — compare `head_sha` against
`gh pr view <N> --json headRefOid`. Different means commits landed after this reading; rerun
rather than trusting the timeline's tail.

## The page reads this report

Bazel's History tab parses the report (`store.ParseHistory`) and draws three sections from it:
`## What it does`, `## Decisions` (one card per `###`) and `## Still open`. Every other `##`
is shown under "More from the history", whole. So the three parsed sections carry the story,
and their shape is exact:

- the heading names below, verbatim (case does not matter to the parser, but keep them as
  written);
- each decision is a `###`, numbered `1.`, `2.`…;
- its first line is `- Commit: <short sha> · <source>`, when a commit carries it;
- any number of `- Rejected: <alternative> — <why>` lines;
- `## Still open` is a list of `- ` items.

## Sections, in this order

Right after the header block, one line `Commits read: N` (the commits you actually listed),
then the sections. No H1: the page already shows the PR's title.

### 1. `## What it does`

What the PR is for, in two parts kept apart because they are two different claims:

- **Stated** — the title, the distilled body, the linked issue, the labels. Quote the body
  where it is specific; do not paraphrase a requirement into something narrower.
- **Actual footprint** — directories and layers touched, `+/-`, file count, tests present or
  not.

If the stated intent and the footprint diverge — a body describing a bugfix over a diff that
adds a subsystem — say so in one line. Do not judge it; a reader who knows about the gap
reads the diff differently, and that is the whole point.

### 2. `## Decisions`

The arc of how the author got to the current diff, one `###` per decision, in the order they
were taken. A decision is a choice the record shows: the first approach, each pivot (a
revert, a file added then deleted, the same file rewritten, a retitle, draft→ready), and each
change a review asked for and got.

```markdown
### 2. Move the refresh token server-side
- Commit: 9de41b2 · thread with @reviewer1 (T2)
The first attempt kept the token in memory (`4f2a10c`, inferred from the thread on
`src/auth/refresh.ts:88`); the reviewer replied "still client-side", and this commit moved it
behind an httpOnly cookie. Confirmed: the message cites the thread.
- Rejected: token in localStorage — readable from JS, raised in T1.
- Rejected: token in memory — lost on reload, raised in T2.
```

- **`- Commit:`** — **one** short sha (7 hex), the commit that carries the decision, then ` · `,
  then where the reason comes from: `description`, `commit message`, `thread with @login
  (T<n>)`, `review by @login`, or `diff` when nothing written explains it. Leave the line out
  when no commit carries the decision (agreed in a thread, never pushed). A decision spread over
  several commits names the main one here and the others in the body; a list of shas on this
  line is not read.
- **The body** says what was decided and why, and spells out the evidence level of every
  causal link (**confirmed** / **inferred** / **unanswered**, see the SKILL). Never write "fixed
  in `abc123`" for an inferred link.
- **`- Rejected:`** — an alternative the record shows was tried or proposed and dropped, and
  why. Only what the commits or comments show; never invent an alternative to have one.
- Pressure points (files with the most commits) and what force-pushes erased go in the body
  of the decision they belong to.

Describe, do not appraise. This report gets read by the people it describes.

### 3. `## Still open`

A **flat** list, one `- ` item per open thing — no paragraph before it, no nested items (the
page reads each line as an item). What a reader still has to deal with: unresolved threads
(with ids), failing checks, questions asked and never answered, review requests never fulfilled. If the
PR is merged, this lists what was open **when it landed** — merging does not resolve anything.
Nothing open → `- Nothing open.`

### 4. `## Review ↔ fix loop`

One block per inline thread, open threads first:

```markdown
#### T2 — `src/auth/refresh.ts:88` — open · not outdated
**reviewer1, 03-03 08:02:** "This keeps the refresh token readable from JS. It needs to be
httpOnly or it is not a rotation, it is a rename."
**Answered by:** `4f2a10c` (03-03 14:55) — *inferred*: touches `src/auth/refresh.ts` after
the thread; the commit message says only "address review".
**Follow-up:** reviewer1 replied 03-04 "still client-side"; second attempt `9de41b2` moved it
server-side. Thread never resolved.
```

Required in every block: the anchor (`path:line`), the ask in the reviewer's own words, the
evidence level spelled out, and the GitHub flags (`resolved by X`, `outdated`) kept separate
from your own evidence. No threads → leave the section out.

### 5. `## Timeline`

One table, every event, ascending:

```markdown
| UTC | Δ | Who | Event | Detail |
|---|---|---|---|---|
| 03-02 09:14 | — | someuser | opened | draft, base `develop` |
| 03-03 08:02 | 20h22m | reviewer1 | review: CHANGES_REQUESTED | 3 threads, all on token storage |
| 03-05 10:30 | 1d19h | someuser | force-push | 12 commits rewritten; T1 anchored pre-push |
```

`Δ` is the gap from the previous row — stalls are usually where the story is. Thread ids are
the ones from section 4. Bot batches collapse into one row. A force-push always gets its own
row, with what it rewrote.

### 6. `## Stats`

Small table: time to first review, number of review rounds, commits after the first review,
longest gap, cycle time open→merge, top three most-churned files, human vs bot comment count.

### 7. `## Gaps`

Anything you could not read: force-pushed commits, a 403, a deleted fork, a truncated list
you could not paginate. One line each, naming the section it affects. Nothing missing → leave
the section out.

## Rules

- Quote reviewers verbatim for the ask; summarize everything else.
- One timestamp format, UTC, throughout.
- Commits sorted by committer date; flag any commit whose author date is more than a day
  earlier (rebased).
- Every causal claim carries its evidence level.
- No findings, no severities, no verdict. This report does not review code — if it starts
  making correctness claims, that is `bazel-review-fleet`'s job and this one is doing it without
  the lenses.
