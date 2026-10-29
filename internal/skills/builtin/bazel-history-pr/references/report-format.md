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

## Sections, in this order

### 1. About

What the PR is for, in two parts kept apart because they are two different claims:

- **Stated** — the title, the distilled body, the linked issue, the labels. Quote the body
  where it is specific; do not paraphrase a requirement into something narrower.
- **Actual footprint** — directories and layers touched, `+/-`, file count, tests present or
  not.

If the stated intent and the footprint diverge — a body describing a bugfix over a diff that
adds a subsystem — say so in one line. Do not judge it; a reader who knows about the gap
reads the diff differently, and that is the whole point.

### 2. Timeline

One table, every event, ascending. This is the spine.

```markdown
| UTC | Δ | Who | Event | Detail |
|---|---|---|---|---|
| 03-02 09:14 | — | someuser | opened | draft, base `develop` |
| 03-02 11:40 | 2h26m | someuser | 4 commits | first pass: store token in localStorage |
| 03-03 08:02 | 20h22m | reviewer1 | review: CHANGES_REQUESTED | 3 threads, all on token storage |
| 03-03 14:55 | 6h53m | someuser | commit `4f2a10c` | moves token to memory — T2 (inferred) |
| 03-05 10:30 | 1d19h | someuser | force-push | 12 commits rewritten; T1 anchored pre-push |
| 03-08 16:02 | … | reviewer2 | merged | 2 threads still open |
```

- `Δ` is the gap from the previous row. It is what makes a stall visible, and stalls are
  usually where the story is.
- Thread ids (`T1`, `T2`, …) are yours, assigned in section 3 and referenced here.
- Bot batches collapse into one row (`github-actions | 14 check runs | 3 red, then green`).
- A force-push always gets its own row, with what it rewrote.

### 3. Review ↔ fix loop

One block per thread, open threads first:

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
evidence level (**confirmed** / **inferred** / **unanswered**) spelled out, and the GitHub
flags (`resolved by X`, `outdated`) kept separate from your own evidence.

Never write "fixed in `abc123`" for an inferred link. The reader is deciding what to trust,
and a confident wrong link costs more than an honest gap.

### 4. How the author approached it

Prose, four to eight sentences. The arc the commits show:

- the first approach, before any review touched it
- each pivot — a revert, a file added then deleted, the same file rewritten three times, a
  draft→ready flip, a retitle
- the pressure points — files with the most commits touching them; that is where the problem
  was actually hard, and where a reviewer should start
- what force-pushes removed from the record

Describe, do not appraise. This report gets read by the people it describes.

### 5. Open items

What a reader still has to deal with: unresolved threads (with ids), failing checks,
questions asked and never answered, review requests never fulfilled. If the PR is merged,
this section lists what was open **when it landed** — merging does not resolve anything.

### 6. Stats

Small table: time to first review, number of review rounds, commits after the first review,
longest gap, cycle time open→merge, top three most-churned files, human vs bot comment count.

### 7. Gaps

Anything you could not read: force-pushed commits, a 403, a deleted fork, a truncated list
you could not paginate. One line each, naming the section it affects.

A named gap is context. An unnamed one makes every other section unverifiable.

## Rules

- Quote reviewers verbatim for the ask; summarize everything else.
- One timestamp format, UTC, throughout.
- Commits sorted by committer date; flag any commit whose author date is more than a day
  earlier (rebased).
- Every causal claim carries its evidence level.
- No findings, no severities, no verdict. This report does not review code — if it starts
  making correctness claims, that is `bazel-review-fleet`'s job and this one is doing it without
  the lenses.
