<!-- history-pr:v1
repo: Beroni/bazel
pr: 22
title: feat: publish step waits for you to choose what goes
state: MERGED
author: Beroni
base: master
head_sha: 6fe788fee32d4fe1d7099e1478164921e5bd83fe
created: 2026-09-25T21:50:04Z
merged: 2026-10-29T18:07:09Z
commits: 11
reviews: 1
threads: 12 (0 resolved, 12 open)
force_pushes: 1
generated_at: 2026-10-30T01:16:41Z
-->
Commits read: 11

# History — PR #22 · feat: publish step waits for you to choose what goes

## What it does

**Stated.** The body says a pipeline that ends in `publish` "no longer posts on its own. It saves the review, stops **waiting on you**, and shows only what would go to the PR, each finding with its **report** checkbox." That makes `pause` redundant, so it is removed. It lists three commits:
- **feat(store):** each item under `## Needs human verification` becomes a finding that can be unticked.
- **feat(server):** the `publish` step waits. The screen splits into *stays in Bazel* and *goes to the PR*. `pause`, the clone kept between stops, `POST /api/jobs/{id}/continue` and the `paused` state are removed.
- **feat(config):** the publishing skill (`bazel-post-report`, or whatever `post_agent.task` calls) can no longer be added as an agent.

The body also promises a config migration: `pause` is dropped, the post skill saved as an agent is removed and its steps become `publish`, `default:` is cleared if it pointed there, and a second `publish` is dropped. No linked issue and no labels. Two test-plan boxes are still unticked (running `review-fleet → publish` in the page, and loading an old config).

**Actual footprint.** 15 files, +2541 / −631.
- Server, `internal/server/{jobs,server,render,view}.go` plus `static/app.js` and `app.css`: about 600 lines of code, and `server_test.go` alone is +909 / −126.
- Config, `internal/config/config.go`: +247 / −65, with +364 / −42 in tests.
- Store, `internal/store/findings.go`: +89 / −7, with +125 / −7 in tests.
- Agent: `agent.go`, `agent_test.go`.
- Docs: `README.md`.
- Tests are present in every layer, and `render_test.go` is new (+112).

The footprint matches the stated scope. The follow-up commits add more than the body describes: refusing a publish that is in flight or already posted, a per-publish copy of the review, escaped raw HTML on screen, and a server-instance id on the page (see the decisions).

## Decisions

### 1. Make the publish step stop and wait, and drop `pause`
- Commit: 8dfbeff · description
The first attempt turns `publish` into a stop point: the review is saved and the card waits, showing only the publishable part with its checkboxes. `pause` is removed in the same commit, together with `/continue`, the `paused` state and the clone kept between stops. The split of the report into `publish_html` and `context_html` is part of this commit (`TestPipelineEsperaParaPublicar` covers the waiting state).
- Rejected: `pause` as a separate step — "redundant" once `publish` waits (description).
- Rejected: keeping the clone between stops and `POST /api/jobs/{id}/continue` — removed with the state they served.

### 2. Make each doubt under *Needs human verification* a finding that can be unticked
- Commit: 211ccd1 · commit message
Each list item in that section becomes a finding, "so a doubt already settled does not reach the PR". The existing fixture in `findings_test.go` was changed from that section to `## Coverage`. Reviewer thread T9 later points out that this left the new behaviour without an assertion.

### 3. Keep the publishing skill out of the agent list, and migrate old configs
- Commit: 1ebf73b · commit message
Run as an ordinary step, the skill has neither the review nor the prompt that authorizes posting. Config load drops `pause` and the post agent. A step that named the post agent becomes `publish`. The commit tags the skill "runs as publish" on the skills page. This is the head the reviewer inspected.
- Rejected: leaving the post skill as an ordinary agent — it would run without the review in hand.

### 4. Force-push of the first three commits (pre-review)
- Commit: 1ebf73b · timeline
Author dates are 21:46–21:47 and committer dates 21:52:03–04, five to six minutes apart (below the one-day flag). The PR opened at 21:50:04, and a `head_ref_force_pushed` event follows at 21:52:22. Everything before that push is unreachable through the API (see Gaps). No reviewer thread was written before it, so none is orphaned.

### 5. The review asks for a stricter human gate, and the author answers it in one commit
- Commit: fdec568 · thread with @Duukaa (T1–T12)
Duukaa's `CHANGES_REQUESTED` (2026-10-02) raised 12 threads. 26 days later one commit touched 13 files, +1,355 / −305 in total, almost all in `config.go`, `jobs.go`, `server_test.go`, `app.js` and `README.md`. Its message does not cite the review or the reviewer. It lists the following, and each item is linked to a thread below at the **inferred** level:
- **Migration (T2, T3, T4):** "a pause before it turns it into the publish step, or drops the pipeline with a warning"; "the post agent's step is removed wherever it was, a single publish goes last only after a publishable step"; "isPostAgent matches by name, full task or the built-in bazel-post-report skill, not by any shared skill"; `resolveSteps` rejects a posting agent before publish.
- **Waiting state (T1):** "The card only waits when there is something to choose; a failed save ends as failed instead of falling back to one-click publish. 'Keep it here' dismisses the wait."
- **Double publish and shared copy (review body, not threads):** a publish in flight or already posted is refused (409), and each publish gets its own copy, "removed once the post agent is done".
- **Lock (T8):** "Views render outside the job lock."
- **Lazy continuation (T5):** unticking a doubt "now removes all of it (lazy continuation and bold-numbered items)". The message uses the reviewer's own term, and the commit adds the explanatory comment at `findings.go:85-90`. The thread's line 91 was last modified by 211ccd1, so the exact line was not touched.
- **Raw HTML (T11):** shown "as escaped text, except the one-line finding metadata".
- **Stale caches (T10):** "The page drops cached bodies and picks when the server restarts." The code adds an `instance` id to `/api/state` and SSE (`adoptInstance` in `app.js`), which is the reviewer's first option. `app.js:421` is unchanged (last modified by 8dfbeff).
- **Tests (T6, T7, T9):** +861 lines in `server_test.go`, +75 in `findings_test.go`, +289 in `config_test.go`.
- **README (T11, T12):** rewritten to say "Bazel's own code publishes nothing on its own" and "The `publish` step is a gate in front of Bazel's publishing path, not a sandbox around the agents".
- Rejected: the "run the post agent against the reviewed commit" and "`--disallowedTools` for reviewers" suggestions in T12. A grep of the Go sources finds no `disallowed` and no head-sha pinning in `jobs.go`, `workspace` or `agent.go`. The README wording was softened instead.

None of this is marked as answered in GitHub: no reply and no resolve on any thread.

### 6. Small fixes and refactors after the big fix
- Commit: 4a2a946, 23bed06, a4de41d, 0bfb184, d717081 · commit message
These five land within 90 minutes of fdec568, none citing a thread:
- `4a2a946`: show failed steps in `ContextBody`.
- `23bed06`: test that an orphan `publish` drops its pipeline. This is inferred to relate to T2, which asked for a Load→Save→Load round trip; the commit is a test tweak (+7 / −2).
- `a4de41d`: skip empty items in the human section. It adds a `- None.` check at `findings.go:95-96` and a test. It is not tied to any thread.
- `0bfb184`: reuse `skills.TaskSkill`.
- `d717081`: share a step filter in the bodies.

`23bed06` and `a4de41d` have author dates 49s and 31s earlier than their committer date, so they were amended or rebased within the minute.

### 7. Bring `master` into the branch, twice
- Commit: a2938dd, 6fe788f · commit message
Two merge commits (two parents each), not attempts. The review noted that the branch was "behind `master`" and "merges cleanly". `a2938dd` (10-28) brings agent, pricing and gh changes. `6fe788f` (10-29, seven minutes before the merge) brings #23's MCP server, pricing changes and more. The merge commits are why `config.go`, `jobs.go` and `server.go` appear touched by changes unrelated to this PR.

## Still open

All 12 threads were open (unresolved) at merge, with no reply on any of them. `reviewDecision` at the time of reading is `REVIEW_REQUIRED`. The only review is `CHANGES_REQUESTED`, and no later review or dismissal appears.
- T1 `internal/server/jobs.go:391` — waiting state ignores whether there is anything to choose (outdated). A later commit message describes related changes (inferred).
- T2 `internal/config/config.go:1035` — migration corrupts pipelines where the post agent wasn't last (outdated). Message addresses it (inferred).
- T3 `internal/config/config.go:1032` — dropping `pause` removes the checkpoint in front of self-posting agents (outdated). Message addresses it (inferred).
- T4 `internal/config/config.go:1006` — post agent matched by skill alone (outdated). Message addresses it (inferred).
- T5 `internal/store/findings.go:91` — lazy-continuation line split from its item. Message addresses it (inferred).
- T6 `internal/server/server_test.go:1269` — new transitions untested. Tests were added in the same commit (inferred).
- T7 `internal/server/server_test.go:1322` — step-body unwrapping assertion lost. Not verified.
- T8 `internal/server/view.go:197` — extra renders under `m.mu` (outdated). Message says "Views render outside the job lock" (inferred).
- T9 `internal/store/findings_test.go:114` — bold-numbered items under *Needs human verification* lost their assertion. Not verified.
- T10 `internal/server/static/app.js:421` — stale caches after a restart. Server `instance` id and `adoptInstance` added (inferred). The reviewer's `bodies`/`splits`/`picks` clearing is not verified.
- T11 `README.md:474` — "shows exactly what goes" and raw HTML (outdated). Message and README address it (inferred).
- T12 `README.md:468` — "does not publish on its own" and the post agent cloning the current head (outdated). README softened. The two suggested mitigations do not appear in the code (see Decision 5).
- Review-body items outside the diff hunks. I could not verify these were handled:
  - `Manager.keep` dead: `NewManager` still takes `keep bool`, and I found no `m.keep` read.
  - Stale "Pausado nada escreve…" comment in `Log()`: `grep` finds no `Pausado` in `jobs.go`.
  - `render.go` comment: still reads "Sem goldmark.WithUnsafe(): HTML cru no…" (checked only the first line).
  - Saved-tab publish sending non-publishable steps: `PublishSaved` still calls `publishInputFor(path, body, skip, false)`.
- The two unticked test-plan boxes in the description (the manual run in the page, and the old-config load).
- PR #26, "chore: translate code comments to English", cross-references this PR after the merge. Many comments quoted above are in Portuguese at this head.

## Review ↔ fix loop

All 12 threads come from one `CHANGES_REQUESTED` review by @Duukaa (2026-10-02 20:59:52Z, against `1ebf73b`). None has a reply, a resolver or a follow-up. The "Answered by" line below is **inferred** in every case: all threads share one candidate, `fdec568`, which touches each path after the thread but does not cite the thread or the reviewer. `outdated` and `resolved` are GitHub's flags, kept apart from that.

#### T1 — `internal/server/jobs.go:391` — open · outdated
**Duukaa, 10-02 20:59:** "Waiting state ignores whether there is anything to choose, or whether something already went." Three failure shapes: a self-posting agent before `publish`, an agent later set to "stays here", and every publishable step failed. Also: `job.publish` read after the in-place branch set it to nil, and a failed save falls back to the one-click buttons.
**Answered by:** `fdec568` (10-28 21:49) — *inferred*: `jobs.go` +179 / −35, and the message says "The card only waits when there is something to choose; a failed save ends as failed…".

#### T2 — `internal/config/config.go:1035` — open · outdated
**Duukaa:** "Converting the post agent's step to `publish` in place breaks pipelines where it wasn't the last step." Table of three saved shapes that lose steps. Asks for a Load → Save → Load round trip in `TestMigraPipelines`.
**Answered by:** `fdec568` — *inferred*: `config.go` +171 / −27 and `config_test.go` +289; message says the post agent's step is "removed wherever it was, a single publish goes last only after a publishable step, and empty pipelines and their default are cleared". Whether the round-trip test exists was not verified.

#### T3 — `internal/config/config.go:1032` — open · outdated
**Duukaa:** "Dropping every `pause` removes the human checkpoint in front of agents that post on their own." Example `[review-fleet, pause, security-audit-post]`. Asks for a `[a, pause, x-post]` migration test.
**Answered by:** `fdec568` — *inferred*: message says "a pause before it turns it into the publish step, or drops the pipeline with a warning". Test coverage not verified.

#### T4 — `internal/config/config.go:1006` — open · outdated
**Duukaa:** "Matching the post agent by **skill** alone can delete a reviewing agent." Asks for exact name or full task.
**Answered by:** `fdec568` — *inferred*: message says "isPostAgent matches by name, full task or the built-in bazel-post-report skill, not by any shared skill". `0bfb184` later changes `config.go` again (reuse of `skills.TaskSkill`).

#### T5 — `internal/store/findings.go:91` — open · not outdated
**Duukaa:** "A wrapped line inside a 'Needs human verification' item is split off as a separate part." `DropFindings(..., []int{1})` leaves the continuation glued to the first doubt.
**Answered by:** `fdec568` — *inferred*: adds the lazy-continuation comment at `findings.go:85-90` and a test (`findings_test.go` +75). Line 91 itself is unchanged since `211ccd1`. `a4de41d` also edits this function.

#### T6 — `internal/server/server_test.go:1269` — open · not outdated
**Duukaa:** "This test covers reaching the waiting state, but the new transitions aren't tested anywhere." Three cases (successful publish clears `AwaitsPublish`, failed publish keeps it, skip indices against `PublishHTML`).
**Answered by:** `fdec568` — *inferred*: `server_test.go` +861. Line 1269 itself is untouched (`8dfbeff`). Which tests cover the three transitions was not verified.

#### T7 — `internal/server/server_test.go:1322` — open · not outdated
**Duukaa:** "The removed pause/continue assertions were the only check that unwrapping happens per *step body*, not just on `Body`." Suggests a `[historia, frota (fenced), publish]` case.
**Answered by:** *inferred, weakly*: same commit adds tests to the file (`fdec568`), and `6fe788f` adds 100 lines to it via merge. Nothing shows the suggested case.

#### T8 — `internal/server/view.go:197` — open · outdated
**Duukaa:** "these are two more goldmark + bluemonday passes … done while holding `m.mu`."
**Answered by:** `fdec568` — *inferred*: `view.go` +31 / −11, message "Views render outside the job lock."

#### T9 — `internal/store/findings_test.go:114` — open · not outdated
**Duukaa:** "Changing this fixture from `## Needs human verification` to `## Coverage` means bold-numbered items under *Needs human verification* are now findings, but nothing asserts that behaviour anymore."
**Answered by:** `fdec568` — *inferred*: adds `findings_test.go` +75 including a test whose name mentions the bold-numbered doubt (`TestSplitFindingsDuvidaNegritoNumerado` appears at `findings_test.go:300` via `a4de41d`). Line 114 still last touched by `211ccd1`.

#### T10 — `internal/server/static/app.js:421` — open · not outdated
**Duukaa:** "After a server restart, a stale cache can show one review and publish another." Options: instance id, random job ids, or a hash of the publishable body sent with `skip`.
**Answered by:** `fdec568` — *inferred*: `app.js` +110 / −8, with an `instance` field in the state and `adoptInstance(...)`; `server.go` +47 and an `instance` id generated per process. This is the reviewer's first option. Line 421 is untouched.

#### T11 — `README.md:474` — open · outdated
**Duukaa:** "'The screen shows exactly what goes' doesn't fully hold for raw HTML." Steered review could place `<a href=...>` or an HTML comment that is hidden on screen but posted. Suggests visible escaping or escaping before `Post`.
**Answered by:** `fdec568` — *inferred*: `render.go` +101 / −4, `render_test.go` +112, README paragraph "Raw HTML in the review is shown as escaped text … instead of being interpreted or hidden". Does not say whether the body is also escaped before `Post` (the reviewer's second option).

#### T12 — `README.md:468` — open · outdated
**Duukaa:** "Worth softening 'it does not publish on its own'." The guarantee holds for Bazel's code, not the agents; the wait is a window during which the author can push a hostile `CLAUDE.md`. Suggests running the post agent against the reviewed commit and `--disallowedTools` for reviewers.
**Answered by:** `fdec568` — *inferred*, partly: README softened ("The `publish` step is a gate in front of Bazel's publishing path, not a sandbox around the agents"). The two code mitigations do not appear in the code (see Decision 5).

## Timeline

| UTC | Δ | Who | Event | Detail |
|---|---|---|---|---|
| 09-25 21:46–21:47 | — | Beroni | authored 3 commits | `211ccd1`, `8dfbeff`, `1ebf73b` (author dates) |
| 09-25 21:50 | 3m | Beroni | opened | base `master`, not a draft |
| 09-25 21:52 | 2m | Beroni | rewrote commits | committer dates 21:52:03–04 on the three commits |
| 09-25 21:52 | 18s | Beroni | force-push | head → `1ebf73b`; pre-push history not recoverable |
| 10-02 20:59 | 6d23h7m | Duukaa | review: CHANGES_REQUESTED | 12 inline threads (T1–T12), body with 3 headline asks and extras |
| 10-28 21:11 | 26d0h11m | Beroni | merge `a2938dd` | `master` into the branch |
| 10-28 21:49 | 38m | Beroni | commit `fdec568` | the large fix, 13 files |
| 10-28 22:22 | 33m | Beroni | commit `4a2a946` | ContextBody |
| 10-28 22:23 | 1m | Beroni | commits `23bed06`, `a4de41d` | test, store fix (same committer second) |
| 10-28 22:23 | 6s | Beroni | commit `0bfb184` | refactor |
| 10-28 22:23 | 15s | Beroni | commit `d717081` | refactor |
| 10-29 17:59 | 19h36m | Beroni | merge `6fe788f` | `master` into the branch |
| 10-29 18:07 | 7m | Beroni | merged | merge commit `3ca50d7` |
| 10-29 18:07 | 1s | Beroni | closed | |
| 10-29 18:37 | 30m | Beroni | cross-referenced | PR #26 "chore: translate code comments to English" |

## Stats

| Metric | Value |
|---|---|
| Time to first review | 6d 23h 10m (from open) |
| Review rounds | 1 |
| Commits after the first review | 8 (6 non-merge, 2 merges) |
| Longest gap | 26d 0h 11m (review → first commit after it) |
| Cycle time open → merge | 33d 20h 17m |
| Most-touched files (by commits) | `config.go` 4, `config_test.go` 4, `README.md` 4 (then `findings.go`, `findings_test.go`, `app.js` 3 each) |
| Comments | 12 human inline, 0 conversation comments, 0 bot; 1 review body |

## Gaps

- The three commits before the force-push (`head_ref_force_pushed`, 09-25 21:52:22) can't be read. Decisions 1–4 rest on the post-push commits only, so how the first approach looked before then is unknown.
- Per-commit file lists for `fdec568` and the other post-review commits come from the API. Hunk-level mapping to thread lines was only checked with `git blame` on the final head for T5, T6, T7, T9 and T10. For the outdated threads (T1–T4, T8, T11, T12) the anchored lines no longer exist, so only file-level evidence is possible.
- Check runs and CI history were not collected.
- Whether the post-review tests assert specific behaviours (T2, T3, T6, T7, T9) was not verified.
