<!-- history-pr:v1
repo: beroni/bazel
pr: 26
title: Translate comments to English
state: OPEN
author: beroni
base: master
head_sha: f231147aa0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5
created: 2026-10-28T10:00:00Z
merged: -
commits: 4
reviews: 1
threads: 1 (0 resolved, 1 open)
force_pushes: 0
generated_at: 2026-10-29T12:00:00Z
-->

# PR #26 — Translate comments to English

## About

**Stated:** translate every Go comment to English.
**Actual footprint:** 31 files under `internal/`, comments only, no tests changed.

## Timeline

| UTC | Δ | Who | Event | Detail |
|---|---|---|---|---|
| 10-28 10:00 | — | beroni | opened | base `master` |
| 10-28 15:20 | 5h20m | reviewer | review: COMMENTED | 1 thread on `internal/store/findings.go` |

## Review ↔ fix loop

#### T1 — `internal/store/findings.go:12` — open · not outdated
**reviewer, 10-28 15:20:** "This comment was mistranslated."
**Answered by:** `5fa7311` (10-28 18:00) — *inferred*: touches the file after the thread.

## How the author approached it

One pass translating every package, then a fix-up commit for mistranslations.

## Open items

- T1 is still open.

## Stats

| Metric | Value |
|---|---|
| Review rounds | 1 |

## Gaps

- None.
