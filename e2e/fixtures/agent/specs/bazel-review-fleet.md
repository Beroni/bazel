<!-- review-fleet:v1
repo: acme/api-core
pr: 101
base: main
head_sha: aaaa101000000000000000000000000000000000
scope_cmd: git diff origin/main...HEAD
files: 2
lenses_ran: senior-code-reviewer, exploit-digger, lazy-senior-dev
lenses_skipped: none
verdict: request_changes
-->

# Review Fleet — PR #101 · Add rate limiter to the public API

**Verdict:** request_changes · 2 findings (1 major, 1 minor) · 1 cut (−12 lines)
**Scope:** `git diff origin/main...HEAD` — 2 files · base `main` · head `aaaa101`
**Lenses:** senior-code-reviewer, exploit-digger, lazy-senior-dev

## Findings

### E2E-BUILTIN-FINDING-ONE bucket refills past its capacity
<!-- finding severity=major class=logic lenses=senior-code-reviewer+exploit-digger path=internal/limit/bucket.go line=12 side=RIGHT confidence=9 -->

**major · logic** (senior-code-reviewer + exploit-digger)

The refill adds tokens without capping at the bucket size.

**Trigger:** a client idle for an hour gets an hour's worth of requests at once.

**Fix:** clamp the token count to the capacity after each refill.

### E2E-BUILTIN-FINDING-TWO no test for the idle refill
<!-- finding severity=minor class=test-gap lenses=senior-code-reviewer path=internal/limit/bucket_test.go line=3 side=RIGHT confidence=8 -->

**minor · test-gap** (senior-code-reviewer)

The suite never lets the bucket sit idle before a burst.

**Fix:** add a case that advances the clock past a full refill and asserts the cap.

## Cuts

### E2E-BUILTIN-CUT hand-rolled clock wrapper
<!-- finding severity=nit class=stdlib lenses=lazy-senior-dev path=internal/limit/bucket.go line=4 side=RIGHT lines_saved=12 -->

**nit · stdlib · −12 lines** (lazy-senior-dev)

`clock` wraps `time.Now` with nothing added.

**Fix:** call `time.Now` directly, or take a `func() time.Time` in tests.

## Needs human verification
<!-- needs-human -->

- `internal/limit/bucket.go:20` — E2E-BUILTIN-HUMAN the limiter key may be the client IP behind a proxy.
  **To settle:** confirm which header the load balancer sets.
