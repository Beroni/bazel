# Harness architecture

```
e2e/
  playwright.config.ts     6 workers, fully parallel, traces kept on failure
  harness/
    paths.ts               BIN, FIXTURES, SAFE_PATH, safeEnv()
    global-setup.ts        run lock; go build bazel + fakes; preflight; ~/.bazel guard
    lock.ts                one run per output directory (E2E_OUT for another)
    bazel.ts               class Bazel (one isolated server), defaultConfig()
    fixtures.ts            Playwright fixtures: bazelOptions, bazel, app
    ui.ts                  every UI selector, by testid only
  fakes/gh/main.go         fake GitHub CLI
  fakes/agent/main.go      fake agent CLI (scenario player)
  fixtures/default/        prs.json, gh.json
  fixtures/agent/          hand-written scenarios
  fixtures/agent/specs/    scenario specs → .out/scenarios/*.jsonl
  tools/stream.mjs         synth (spec → claude-stream) and scrub (real stream → fixture)
  tests/                   p0-*.spec.ts, p1-*.spec.ts
  .out/                    bin/, scenarios/, runs/, results/, report/ (git-ignored)
```

## One test, one server

`new Bazel(opts).start()`:

1. `mkdtemp e2e/.out/runs/bz-XXXX` with `home/`, `bazel-home/`, `tmp/`,
   `gh/`, `agent/`, and `fixtures/` (this test's copy of the gh fixtures,
   plus `gh.json` from `opts.gh`).
2. Writes `bazel-home/config.yaml` as JSON (yaml.v3 reads it) from
   `defaultConfig()` + `opts.config`, and fake skills under `home/skills/`
   (`skills_dir` points there).
3. Picks a free port, spawns `e2e/.out/bin/bazel --addr 127.0.0.1:<port>
   --no-splash` with `safeEnv()` plus `HOME`, `BAZEL_HOME`, `TMPDIR`,
   `FAKE_GH_*`, `FAKE_AGENT_*`. stdout/stderr → `server.log`.
4. Polls `GET /api/state` until 200 (15s budget).

The fixture stops it with SIGTERM (SIGKILL after 7s), deletes the root on
success and keeps it (plus attachments) on failure.

### Environment (safeEnv)

| Var | Value | Why |
| --- | --- | --- |
| `PATH` | `.out/bin:/usr/bin:/bin:/usr/sbin:/sbin` | only the fakes and git |
| `HOME` | `<root>/home` | `~` expansions, `~/.claude/skills` |
| `BAZEL_HOME` | `<root>/bazel-home` | config + reviews |
| `TMPDIR` | `<root>/tmp` | PR clones land here (`bazel-<slug>-<n>-*`) |
| `GH_CONFIG_DIR` | `/nonexistent/gh-config` | a real gh would have no login |
| `GIT_CONFIG_GLOBAL` / `GIT_CONFIG_NOSYSTEM` | `/dev/null` / `1` | no user git config |
| `GIT_ALLOW_PROTOCOL` | `file` | git refuses https/ssh |
| `HTTP(S)_PROXY`, `ALL_PROXY` | `http://127.0.0.1:9` | stray HTTP dies fast |
| `TZ` | `UTC` | stable timestamps |
| `BAZEL_NO_SPLASH`, `NO_COLOR`, `CI` | `1` | no animation |

Nothing else from the developer's shell is inherited.

## The `bazel` fixture API

| Member | Does |
| --- | --- |
| `url`, `port`, `root`, `bazelHome`, `reviewsDir`, `configPath` | where things are |
| `api(method, path, body?)` | same-origin fetch → `{ status, json }` |
| `enqueue(ref, agent)` | `POST /api/reviews`, returns the job id (throws unless 202) |
| `job(id)` / `waitJob(id, pred, ms?)` | `GET /api/jobs/{id}` (with body/html), polled until `pred` |
| `mcp(method, params?, id?)` | one JSON-RPC call to `/mcp` |
| `ghCalls()` | `[{ argv, stdin, cwd, at }]` from the fake gh |
| `agentCalls()` | `[{ scenario, pid, branch, argv, cwd, prompt, builtin_skill, at }]` |
| `release(name)` | creates `agent/<name>`; unblocks `#!wait <name>` and `FAKE_AGENT_GATE=<name>` |
| `agentAlive(pid)` | whether an agent process (`agentCalls()[n].pid`) is still there |
| `movePR(key)` | a new commit on the PR's branch: gh lists it, a clone gets it, Bazel notices on its next refresh |
| `tool(name, args)` | MCP `tools/call` → `{ isError, text, out }` |
| `events()` | opens `/api/events`: `{ list, next(pred), close() }` (first event is `hello`) |
| `restart()` | new process, same BAZEL_HOME (config, reviews, index survive; queue does not) |
| `updatePR(key, patch)` / `setGh(settings)` | mutate this test's gh fixtures mid-test |
| `rawRequest(method, path, headers)` | raw HTTP with a chosen `Host` header |
| `configText()` | config.yaml as Bazel wrote it |
| `savedReviews()` | `*.md` in `reviewsDir` |
| `runtimes()` | per-review plugin folders left in `<BAZEL_HOME>/runtime` (should be `[]` after a run) |
| `clones()` | `bazel-*` dirs left in `TMPDIR` (should be `[]` without `--keep`) |
| `serverLog()` | the server's stdout+stderr |
| `extraOrigins` | origins this test's pages may also visit (a proxy in front of Bazel) |
| `blocked` | URLs the context route aborted |

`app` is `page` after `goto(bazel.url)`, with `dialog → accept()`. The `context`
fixture routes every page: `bazel.mayVisit(url)` compares the parsed origin, so
`http://127.0.0.1:7777@attacker.invalid/` is not local. To assert a confirm's text, add your
own `page.on('dialog', …)` listener — both run.

`bazelOptions.config` may be a function of the server (`(bz) => ({…})`) when a
value needs a path in its temp root.

## Default config

Repos `acme/api-core`, `acme/web-app`; `include_drafts: true`; base agent
`fake-agent -p --output-format stream-json --verbose`, `format: claude-stream`,
`checkout: true`, `timeout_seconds: 60`. Agents:

| name | scenario | notes |
| --- | --- | --- |
| `review-fleet` | `claude-findings` | init, quota event, sub-agent `exploit-digger`, 2 findings, $2.41 / 1.8M tokens |
| `slow-lens` | `claude-slow` | one tool call, then `#!wait release-slow` |
| `plain-lens` | `plain-report` | `format: plain`, 1 finding |

`post_agent` = `bazel-post-report` with scenario `post-claude`, which runs
`#!gh-review` (→ `gh api --method POST repos/<repo>/pulls/<n>/reviews --input -`).

## Determinism

- Never sleep for "long enough". Hold an agent with `#!wait <name>` and call
  `bazel.release(name)` when the test has seen what it needs. A negative gets a
  barrier: `settled(page)` after a dismissed confirm, `agentAlive(pid)` for a kill.
- Poll the API (`waitJob`) or use `expect.poll` / web-first assertions; do
  not read the DOM in a loop.
- `updatedAgo` in prs.json keeps relative ages stable (`2h` stays `2h`).
- Never assert the relative order of stdout and stderr lines: Bazel reads
  them in two goroutines.
- Things that depend on wall-clock seconds (saved file names) need the
  release to happen right after a second boundary — see
  `tests/p1-saved-collision.spec.ts`.
