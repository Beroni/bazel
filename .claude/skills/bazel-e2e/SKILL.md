---
name: bazel-e2e
description: Runs, debugs and extends Bazel's end-to-end suite (e2e/) — a fake gh, a fake agent
  that replays recorded claude-stream/codex-json/grok-stream/plain output, an isolated server per
  test and Playwright on the cached Chromium. Use for "run the e2e tests", "make e2e is failing",
  "add an e2e scenario for X", "record a new agent stream", "why did this e2e test flake", or
  before calling a UI or server change done. Never touches ~/.bazel, the real gh, GitHub or a model.
allowed-tools: Read Grep Glob Bash Edit Write
---

# bazel-e2e

The suite lives in `e2e/` and runs with `make e2e`. Every test boots its own
`bazel` binary with its own `HOME`, `BAZEL_HOME`, `TMPDIR`, `PATH` and port,
talks to it through the browser and the HTTP API, and checks what would have
reached GitHub by reading the fake gh's call log. No network, no real reviews.

## Run

```sh
make e2e                                   # whole suite, ~10s, 4 workers
make e2e E2E_ARGS="tests/p0-publish.spec.ts"
make e2e E2E_ARGS="-g 'cancel' --headed"   # watch it
make e2e E2E_ARGS="--repeat-each 5 --workers 8"   # hunt flakes
cd e2e && npx playwright show-report .out/report
```

First run installs `@playwright/test` (pinned to 1.58.2, which matches the
Chromium build 1208 already in `~/Library/Caches/ms-playwright`) from the npm
cache with `--prefer-offline`; no browser download happens. `globalSetup`
builds `bazel`, `gh` and `fake-agent` into `e2e/.out/bin` every run.

Expected output: all tests pass. Tests marked `test.fail(true, 'BUG-QA-…')`
show as `✘` in the list reporter but count as passed — they reproduce known
bugs and flip to a real failure the day the bug is fixed (then delete the
`test.fail` line).

## Hard rules

- The server env is built from scratch by `safeEnv()` (`e2e/harness/paths.ts`):
  `PATH` is `e2e/.out/bin` + system dirs only. The real `gh` and `claude` live
  in Homebrew and must stay unreachable; `globalSetup` refuses to start if
  they resolve, and its teardown fails the run if `~/.bazel` changed.
- Never add `/opt/homebrew/bin`, `~/.local/bin` or the developer's `PATH` to
  the server env. Need a new tool? Symlink it into `BIN` in `global-setup.ts`.
- Never point a test at a real repo, a real skill or a real model.
- The page is routed: any request not to `http://127.0.0.1:*` is aborted.

## Debug a failure

1. Read the failure in the list output. A failed test keeps its temp root and
   attaches `server.log`, `gh-calls.json`, `agent-calls.json` and the root path.
2. `npx playwright show-trace e2e/.out/results/<test>/trace.zip` — DOM,
   network and console at each step.
3. In the kept root (`e2e/.out/runs/bz-*`): `bazel-home/config.yaml` (what the
   server ran with), `bazel-home/reviews/` (saved reviews, `.index.json`,
   `publish/` copies), `gh/calls.jsonl`, `agent/calls.jsonl` (each agent run
   with its full prompt and cwd), `tmp/` (clones that were not cleaned up).
4. Decide: product bug, test bug or flake. See
   [references/debugging.md](references/debugging.md).

Product bug → do not fix production code from here. Write the failing test,
mark it `test.fail(true, 'BUG-QA-NN: …')` with a comment saying what is
expected, and report it with steps, expected vs. actual and the test name.

## Add a scenario

1. Pick the area file in `e2e/tests/` (`p0-*.spec.ts`, `p1-*.spec.ts` …) or
   add one. Import `test`, `expect` from `../harness/fixtures` and `ui` from
   `../harness/ui`.
2. Use the fixtures: `bazel` (a started server; API helpers, call logs) and
   `app` (a page on the dashboard that accepts every `confirm()`).
3. Need a different config, fixtures or flags?
   `test.use({ bazelOptions: { config: {…}, fixtures: 'name', flags: ['--jobs', '1'] } })`.
   `config` replaces top-level keys of `defaultConfig()` in `e2e/harness/bazel.ts`.
4. Need different agent output? Add a scenario file to `e2e/fixtures/agent/`
   and point an agent at it with `env: { FAKE_AGENT_SCENARIO: 'name' }`.
5. Selectors go through `e2e/harness/ui.ts` only — role/text today, the
   `data-testid` from [references/selectors.md](references/selectors.md) once
   the UI ships it. Never select by CSS class: the redesign renames them.
6. Assert outcomes at three levels when they exist: what the page shows, what
   the API returns, and what reached the fakes (`bazel.ghCalls()`,
   `bazel.agentCalls()`, files under `bazel.reviewsDir`).
7. Run it 5× in parallel (`--repeat-each 5 --workers 8`) before committing.

Details: [references/harness.md](references/harness.md) (architecture, env,
API helpers), [references/fixtures.md](references/fixtures.md) (prs.json,
gh.json, scenario format and directives, how to record a new stream),
[references/selectors.md](references/selectors.md) (the data-testid contract),
[references/debugging.md](references/debugging.md) (classifying failures).
