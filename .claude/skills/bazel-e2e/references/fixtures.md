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
| `gh api --method POST repos/O/R/pulls/N/reviews --input -` | fake review URL; stdin recorded; unknown PR → 404, bad payload → 422 |
| `gh pr list --repo R …` | PRs of R, newest number first; `--state` (open by default), `--author`, `--limit` |
| `gh pr view N --repo R --json …` | one object; unknown PR → GraphQL-style error |
| `gh pr diff N --repo R` | `diff` from the fixture, or one generated from `files` |
| `gh pr comment N --repo R --body-file -` | prints a fake comment URL; stdin recorded; unknown PR or empty body fails |
| `gh repo clone R DIR -- ARGS` | `git clone ARGS file://<state>/repos/R.git DIR` |
| `gh pr checkout N [--repo R]` (in a clone) | the PR's branch; a PR of another repo comes by fetch |
| anything else, any other `gh api` included | exit 1, `fake gh: unsupported: gh …` |

`gh pr list` and `gh pr view` check `--json` against the field list of
`gh pr list --help` (gh 2.87.3), in `prFields`.

The origin repo is built once per server (atomic rename, one `git fast-import`
with fixed dates, so the same fixture gives the same SHAs): `main` with a README,
plus one branch per PR (`headRefName`) adding the PR's `files` (or `pr-<n>.txt`).
The `headRefOid` gh lists is the tip of that branch.

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

PR page fields (`gh pr view --json headRefOid,createdAt,body,commits,files,reviewRequests,latestReviews,labels,statusCheckRollup`,
the redesign's Overview) take gh's shape in the fixture: `createdAt`, `commits`
(`oid`, `messageHeadline`, `committedDate`, `authors`), `reviewRequests`,
`latestReviews` (`author.login`, `state`), `labels` (`name`), `statusCheckRollup`
(`name`, `status`, `conclusion`). Left out, they are synthesized: one commit at
`headRefOid`, empty lists. `files` in the fixture is the branch content (a map);
asked with `--json files` it comes back as gh's list (`path`, `additions`,
`deletions`).

`symlinks` (path → target) commits links in the PR branch, for hostile-PR tests.

`url` defaults to `https://github.com/<repo>/pull/<n>`; `updatedAt` (ISO) or
`updatedAgo` (Go duration); `baseRefName` defaults to `main`.
`changedFiles: 0` makes Bazel refuse the review ("no changed files").

`state` defaults to `OPEN`. Leave `headRefOid` out: gh answers the tip of the
branch, the commit a clone gets. A value stands for a head that no clone will
reach, for PRs that never get reviewed.

To simulate "changed since review": review the PR, `bazel.movePR(key)` (a new
commit on its branch), then `GET /api/prs?refresh=1`. Each test has its own copy
of the fixtures, read on every gh call; `updatePR` and `addPR` change them.

### gh.json

```json
{ "user": "e2e-user",
  "fail": { "auth": "…", "user": "…", "list:acme/web-app": "HTTP 404",
            "comment": "HTTP 403", "clone:acme/api-core": "repository not found", "checkout": "…",
            "view:acme/api-core#101": "…", "diff:acme/api-core#101": "…", "api": "…" },
  "forks": { "acme/fork": "acme/api-core" } }
```

`forks` maps a fork to its parent: in a clone of the fork, `gh pr checkout N`
without `--repo` or `GH_REPO` resolves N in the parent, as gh does.

Each `fail` key makes that call print the message to stderr and exit 1 —
for repo errors in the list, publish failures, clone failures.

## Fake agent (`e2e/fakes/agent`)

Reads the whole prompt from stdin, appends one record to
`$FAKE_AGENT_STATE/calls.jsonl`, waits for `$FAKE_AGENT_STATE/$FAKE_AGENT_GATE`
if that env is set, then plays `$FAKE_AGENT_SCENARIO[.jsonl|.txt|.md]` from
the first directory of `$FAKE_AGENT_SCENARIOS` (`fixtures/agent`, then the
synthesized `.out/scenarios`) that has it, line by line.

The record (`bazel.agentCalls()`):

- `scenario`, `argv`, `cwd`, `prompt`, `at`.
- `skills`: what the agent could load from Bazel: the `--plugin-dir` skills, or
  `<cwd>/.claude/skills` on a base that still writes them into the clone.
  `builtin_skill` and `builtin_skill_source` (`plugin`, `clone` or empty) say
  whether `bazel-post-report` was there and where from.
- `plugin_dir`, `plugin_name`: `--plugin-dir` and the name in its
  `.claude-plugin/plugin.json`.
- `setting_sources_user`, `strict_mcp_config`: whether the flags that confine
  Claude Code arrived.
- `agent_config`: what is left of `.claude`, `.codex`, `.grok` and `.mcp.json`
  (Lstat, so a dangling symlink counts). Must be `[]` when an agent starts.
- `config_readable`: the config files that open under the lower-case names a
  CLI uses (`.claude/settings.json`, `.mcp.json`, …). This is the real check on
  a case-insensitive disk: a PR's `.CLAUDE/` answers to `.claude/`.
  `config_anywhere`: the same names at any depth and in any case.
- `git_config`: what `git config --get` answers in the clone for
  `safe.bareRepository`, `core.fsmonitor`, `core.hooksPath` and `e2e.user`.
- `clone_skills`, `git_status`, `git_exclude`: the clone as the agent found it.
- `grok_env`, `grok_home_config`: the `GROK_*` env Bazel set for a `grok-stream`
  step and whether `$GROK_HOME/config.toml` existed.

Lines go to stdout verbatim, except directives:

| Directive | Effect |
| --- | --- |
| `#!sleep 200` | pause 200 ms |
| `#!stderr text` | one stderr line (shows in the log in another color; last 5 lines become the error message on a non-zero exit) |
| `#!exit 2` | exit now with code 2 |
| `#!wait name` | block until `$FAKE_AGENT_STATE/name` exists (the test calls `bazel.release('name')`); 120 s cap |
| `#!block` | sleep 120 s (to be killed: cancel, timeout) |
| `#!sh <command>` | run a shell command in the working directory, e.g. a `git checkout` that restores files |
| `#!repeat 650 line {i}` | print the line 650 times, `{i}` = 1..650 |
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

### Scenario specs

The easy way to a claude-stream scenario: describe it, do not record it.
`fixtures/agent/specs/<name>.json` is synthesized before every run into
`.out/scenarios/<name>.jsonl` and used as `FAKE_AGENT_SCENARIO=<name>`.

```json
{
  "model": "claude-opus-5-5",
  "tools": ["Read", "Grep", "Glob", "Bash", "Agent"],
  "rateLimit": { "session": 0.42, "week": 0.10 },
  "steps": [
    { "tool": "Bash", "input": { "command": "git diff main...HEAD --stat" } },
    { "agent": "senior-code-reviewer", "description": "precision review", "prompt": "optional first line",
      "steps": [ { "tool": "Read", "input": { "file_path": "a.go" } }, { "text": "E2E-… lens line" } ] },
    { "toolError": "Read", "input": { "file_path": "gone.go" }, "error": "E2E-… file does not exist" },
    { "text": "E2E-… main agent line" },
    { "directive": "#!wait release-me" }
  ],
  "reportFile": "<name>.md",
  "usage": { "input": 2000, "output": 500, "cacheRead": 10000, "cacheWrite": 0, "costUSD": 0.3 },
  "durationMs": 2000,
  "error": "error_max_turns"
}
```

- `report` (inline string) or `reportFile` (relative to the spec) is required.
- `agent` steps become an `Agent` tool call plus the lens's own events
  (`parent_tool_use_id`), so the log shows one terminal per lens.
- `usage` becomes both the plain `usage` and a `modelUsage` entry for `model`
  (that is what Bazel tallies); `costUSD` becomes `total_cost_usd`.
- `error` ends with `is_error: true` and that subtype instead of `success`.
- Examples: `specs/example-skill.json`, `specs/dup-lenses.json`.

### Recording a real stream

Only when the exact shape of a real CLI matters (a new event type, a new
version). Capture **by hand, on your own machine, outside the test suite**,
against a public toy repo — tests never run a model:

```sh
claude -p --output-format stream-json --verbose < prompt.txt > /tmp/raw.jsonl
node e2e/tools/stream.mjs scrub /tmp/raw.jsonl > e2e/fixtures/agent/<name>.jsonl
```

`scrub` refuses a line starting with `#!` (it would run as a directive), and
drops session ids, uuids, cwd, MCP/plugin lists and `mcp__*` tools,
rewrites home and temp paths, emails and token-looking strings, and replaces
tool results with `(scrubbed)` (errors keep 200 chars). Then read the file:
shorten it, replace the report with a small one carrying `E2E-…` markers, keep
it under ~50 lines. Never commit a stream from a private repo.

### Markers

Scenario text uses unique tokens (`E2E-FINDING-ONE`, `E2E-LENS-LINE`,
`E2E-SLOW-READ`, …) so assertions never depend on prose that a redesign or a
copy edit could change.
