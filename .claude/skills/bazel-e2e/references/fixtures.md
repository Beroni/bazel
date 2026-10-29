# Fixtures

## Fake gh (`e2e/fakes/gh`)

Reads `$FAKE_GH_FIXTURES/prs.json` and optional `gh.json`, writes
`$FAKE_GH_STATE/calls.jsonl` (one line per call, under flock) and builds the
fixture git repos under `$FAKE_GH_STATE/repos/<owner>/<repo>.git`.

| Call (what Bazel runs) | Answer |
| --- | --- |
| `gh --version` | `gh version 0.0.0-fake (e2e)` |
| `gh auth status` | ok, logged in as `gh.json.user` |
| `gh api user --jq .login` | `gh.json.user` (default `e2e-user`) |
| `gh api …` (anything else) | `{}`-ish JSON; stdin recorded if `--input -` |
| `gh search prs … -- "repo:a repo:b"` | PRs of those repos, `--json` fields only, `--limit` |
| `gh pr list --repo R …` | PRs of R |
| `gh pr view N --repo R --json …` | one object; unknown PR → GraphQL-style error |
| `gh pr diff N --repo R` | `diff` from the fixture, or one generated from `files` |
| `gh pr comment N --repo R --body-file -` | prints a fake comment URL; stdin recorded |
| `gh repo clone R DIR -- ARGS` | `git clone ARGS file://<state>/repos/R.git DIR` |
| `gh pr checkout N` (in a clone) | `git checkout -B <headRefName> origin/<headRefName>` |
| anything else | exit 1, `fake gh: unsupported: gh …` |

The origin repo is built once per server (atomic rename): `main` with a
README, plus one branch per PR (`headRefName`) adding the PR's `files` (or
`pr-<n>.txt`). Git SHAs are real but do not match `headRefOid`; Bazel only
reads `headRefOid` from gh metadata.

### prs.json

An array. Each entry is a gh PR plus `repo`:

```json
{
  "repo": "acme/api-core", "number": 101, "title": "…", "body": "markdown",
  "isDraft": false, "additions": 120, "deletions": 8, "changedFiles": 3,
  "updatedAgo": "2h",
  "headRefName": "feat/rate-limit", "headRefOid": "aaaa101", "baseRefName": "main",
  "author": {"login": "alice"}, "reviewDecision": "REVIEW_REQUIRED",
  "files": {"internal/limit/bucket.go": "package limit"},
  "diff": "optional unified diff for gh pr diff"
}
```

`url` defaults to `https://github.com/<repo>/pull/<n>`; `updatedAt` (ISO) or
`updatedAgo` (Go duration); `baseRefName` defaults to `main`.
`changedFiles: 0` makes Bazel refuse the review ("no changed files").

To simulate "changed since review": point the test at its own copy of the
fixture dir (`FAKE_GH_FIXTURES` is read on every gh call), review the PR,
change its `headRefOid` in the copy, then `GET /api/prs?refresh=1`. A
`fixturesDir` option on `Bazel` for temp copies is planned for phase 2.

### gh.json

```json
{ "user": "e2e-user",
  "fail": { "search": "HTTP 502", "list:acme/web-app": "HTTP 404", "comment": "HTTP 403",
            "clone:acme/api-core": "repository not found", "checkout": "…",
            "view:acme/api-core#101": "…", "api": "…" } }
```

Each `fail` key makes that call print the message to stderr and exit 1 —
for repo errors in the list, publish failures, clone failures.

## Fake agent (`e2e/fakes/agent`)

Reads the whole prompt from stdin, appends `{scenario, argv, cwd, prompt,
builtin_skill, at}` to `$FAKE_AGENT_STATE/calls.jsonl`, then plays
`$FAKE_AGENT_SCENARIOS/$FAKE_AGENT_SCENARIO[.jsonl|.txt|.md]` line by line.
`builtin_skill` is true when `<cwd>/.claude/skills/bazel-post-report/SKILL.md`
exists (Bazel materializes it in the clone for the post agent).

Lines go to stdout verbatim, except directives:

| Directive | Effect |
| --- | --- |
| `#!sleep 200` | pause 200 ms |
| `#!stderr text` | one stderr line (shows in the log in another color; last 5 lines become the error message on a non-zero exit) |
| `#!exit 2` | exit now with code 2 |
| `#!wait name` | block until `$FAKE_AGENT_STATE/name` exists (the test calls `bazel.release('name')`); 120 s cap |
| `#!block` | sleep 120 s (to be killed: cancel, timeout) |
| `#!gh-review` | read the review file from `/bazel-post-report <path>` in the prompt and the PR from "The repository R is cloned … PR #N"; run `gh api --method POST repos/R/pulls/N/reviews --input -` with `{"event":"COMMENT","body":<file>}` |
| `#!# text` | comment |

The format of the rest is whatever the agent's `format` expects:

- `claude-stream`: one JSON event per line — `system/init`,
  `rate_limit_event`, `assistant`/`user` with `message.content` blocks
  (`text`, `tool_use`, `tool_result`) and `parent_tool_use_id` for sub-agent
  lines, `result` with `result`, `usage`, `modelUsage`, `total_cost_usd`.
  See `internal/agent/stream.go` for what Bazel reads.
- `codex-json`, `grok-stream`: see `internal/agent/codex.go`, `grok.go` and
  their tests for the event shapes.
- `plain`: the Markdown report itself.

### Recording a new stream

Take a real run's stdout **on your own machine, outside the test suite**
(e.g. `claude -p --output-format stream-json --verbose < prompt.txt > raw.jsonl`
on a public toy repo), then scrub it: drop session ids, paths, emails and
anything private; shorten tool results; replace the report with a small one
that carries unique markers (`E2E-…`) the test can assert on. Keep files under
~50 lines. Never commit a stream from a private repo.

### Markers

Scenario text uses unique tokens (`E2E-FINDING-ONE`, `E2E-LENS-LINE`,
`E2E-SLOW-READ`, …) so assertions never depend on prose that a redesign or a
copy edit could change.
