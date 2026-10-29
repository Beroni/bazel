<!-- review-fleet:v1
repo: klever/app
pr: 123
base: develop
head_sha: a1b2c3d4e5f67890abcdef1234567890abcdef12
scope_cmd: git diff origin/develop...HEAD
files: 12
lenses_ran: senior-code-reviewer, exploit-digger, lazy-senior-dev
lenses_skipped: none
verdict: request_changes
-->

# Review Fleet — PR #123 · Add transfer endpoint

**Verdict:** request_changes · 2 findings (1 critical, 1 minor) · 1 cut (−27 lines)
**Scope:** `git diff origin/develop...HEAD` — 12 files · base `develop` · head `a1b2c3d`
**Lenses:** senior-code-reviewer, exploit-digger, lazy-senior-dev

## Findings

### Transfer debits any account by id
<!-- finding severity=critical class=authz lenses=senior-code-reviewer+exploit-digger path=src/api/transfer.ts line=88 side=RIGHT confidence=9 ref=CWE-639 -->

**critical · authz** (senior-code-reviewer + exploit-digger)

Transfer reads `accountId` from the request body without checking ownership.

**Trigger:** `POST /api/transfer` with another user's `accountId` debits their account.

**Fix:** resolve the account from the session, or assert `account.ownerId === session.userId`
before debiting.

### Settlement job has no test for the double-run case
<!-- finding severity=minor class=test-gap lenses=senior-code-reviewer path=src/jobs/settle.test.ts line=14 side=RIGHT confidence=8 -->

**minor · test-gap** (senior-code-reviewer)

The suite covers the happy path only; a second run on the same day is never exercised.

**Fix:** add a case that invokes `settle()` twice for one date and asserts a single ledger
entry.

## Cuts

### Hand-rolled retry duplicates the client's built-in backoff
<!-- finding severity=minor class=reuse lenses=lazy-senior-dev path=src/lib/retry.ts line=12 side=RIGHT lines_saved=27 -->

**minor · reuse · −27 lines** (lazy-senior-dev)

`retryWithBackoff` reimplements what `httpClient` already does via `retry: 3`.

**Fix:** delete the helper and pass `retry: 3` at the call site.

## Needs human verification
<!-- needs-human -->

- `src/jobs/settle.ts:40` — the cron cadence assumes settlement never runs twice for the same
  day.
  **To settle:** confirm with whoever owns the scheduler.
