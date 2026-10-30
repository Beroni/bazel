---
name: bazel-e2e
description: Runs, debugs and extends Bazel's end-to-end suite (e2e/) — a fake gh, a fake agent
  that replays recorded claude-stream/codex-json/grok-stream/plain output, an isolated server per
  test and Playwright on the cached Chromium. Use for "run the e2e tests", "make e2e is failing",
  "add an e2e scenario for X", "record a new agent stream", "test my new built-in skill with the
  fake agent", "why did this e2e test flake", or before calling a UI or server change done. Never touches ~/.bazel, the real gh, GitHub or a model.
allowed-tools: Read Grep Glob Bash Edit Write
---

# bazel-e2e

The suite lives in `e2e/` and runs with `make e2e`. Every test boots its own
`bazel` binary with its own `HOME`, `BAZEL_HOME`, `TMPDIR`, `PATH` and port,
talks to it through the browser and the HTTP API, and checks what would have
reached GitHub by reading the fake gh's call log. No network, no real reviews.

## Run

```sh
make e2e                                   # whole suite, ~25s, 6 workers
make e2e E2E_ARGS="tests/p0-publish.spec.ts"
make e2e E2E_ARGS="-g 'cancel' --headed"   # watch it
make e2e E2E_ARGS="--repeat-each 5 --workers 8"   # hunt flakes
cd e2e && npx playwright show-report .out/report
```

### Against another base

The suite in this checkout can test any Bazel source tree — the fakes and
specs stay the same, only the binary under test changes:

```sh
git worktree add --detach ../bases/redesign <branch-or-commit>
make e2e E2E_BAZEL_SRC=../bases/redesign
make e2e E2E_BAZEL_SRC=../bases/fix E2E_FIXED=BUG-QA-12
```

Use `knownBug`, never a skip, for a security guard, so losing it turns the
suite red. `tests/p0-confine.spec.ts` (the PR's agent config never runs) has
neither any more: since #29 it simply has to pass.
`E2E_FIXED` lists the known bugs that base has fixed: their `knownBug(...)`
tests must then pass instead of fail. The selectors in `harness/ui.ts` follow
the current UI only: a base from before #30 will not pass. A UI change shows up
as a helper to adjust there, not as an edit in every test.

First run installs `@playwright/test` (pinned to 1.58.2, which matches the
Chromium build 1208 already in `~/Library/Caches/ms-playwright`) from the npm
cache with `--prefer-offline`; no browser download happens. `globalSetup`
builds `bazel`, `gh` and `fake-agent` into `e2e/.out/bin` every run (in
parallel, on Go's build cache) and synthesizes `fixtures/agent/specs/*.json`
into `e2e/.out/scenarios/`.

Expected output: all tests pass. Tests that call `knownBug('BUG-QA-…', …)`
show as `✘` in the list reporter but count as passed — they reproduce open
bugs and flip to "Expected to fail, but passed" the day the bug is fixed (then
delete the `knownBug` line, or list the id in `E2E_FIXED` for that base).

## Hard rules

- The server env is built from scratch by `safeEnv()` (`e2e/harness/paths.ts`):
  `PATH` is `e2e/.out/bin` + system dirs only. The real `gh` and `claude` live
  in Homebrew and must stay unreachable; `globalSetup` refuses to start if
  they resolve, and its teardown fails the run if `~/.bazel` changed.
- Never add `/opt/homebrew/bin`, `~/.local/bin` or the developer's `PATH` to
  the server env. Need a new tool? Symlink it into `BIN` in `global-setup.ts`.
- Never point a test at a real repo, a real skill or a real model.
- The page is routed: any request not to `http://127.0.0.1:*` is aborted.
- A gh call the fake does not implement fails the test that made it (the
  `bazel` fixture checks the call log at teardown): teach the fake, then test it.

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
call `knownBug('BUG-QA-NN', 'one line')` at its top (from `harness/fixtures`)
with a comment saying what is expected, and report it with steps, expected
vs. actual and the test name.

## Add a scenario

1. Pick the area file in `e2e/tests/` (`p0-*.spec.ts`, `p1-*.spec.ts` …) or
   add one. Import `test`, `expect` from `../harness/fixtures` and `ui` from
   `../harness/ui`.
2. Use the fixtures: `bazel` (a started server; API/MCP/SSE helpers, call
   logs, `restart()`, `updatePR()`, `setGh()`), `app` (a page on the
   dashboard) and `dialogs` (`dialogs.accept = false` to say no to the next
   `confirm()`; `dialogs.messages` has what each one asked).
3. Need a different config, fixtures or flags?
   `test.use({ bazelOptions: { config: {…}, gh: { fail: {…} }, flags: ['--jobs', '1'] } })`.
   `config` replaces top-level keys of `defaultConfig()` in `e2e/harness/bazel.ts`.
   Each test gets its own copy of the gh fixtures, so mutating them mid-test
   (`bazel.updatePR('acme/api-core#101', { headRefOid: 'new' })`) is safe.
4. Need different agent output? Add a scenario file to `e2e/fixtures/agent/`
   (or a spec, below) and point an agent at it with
   `env: { FAKE_AGENT_SCENARIO: 'name' }`. Hold it mid-run with
   `FAKE_AGENT_GATE: 'gate-name'` in the same env and `bazel.release('gate-name')`.
5. Selectors and UI actions go through `e2e/harness/ui.ts` only: the
   `data-testid` of [references/selectors.md](references/selectors.md) first,
   with no fallback, so a testid that goes away fails loudly. Use the action helpers (`openJob`,
   `cancelJob`, `filterByReview`, `selectByLabel`, `expandFindings`,
   `openFilters`, `showMeters`) where the two UIs differ —
   on the redesign, cancel/publish/keep live in the job's detail, not on the
   card. Never select by CSS class.
6. Assert outcomes at three levels when they exist: what the page shows, what
   the API returns, and what reached the fakes (`bazel.ghCalls()`,
   `bazel.agentCalls()`, files under `bazel.reviewsDir`).
7. Run it 5× in parallel (`--repeat-each 5 --workers 8`) before committing.

## Test a new skill with the fake agent

For a skill that ships inside Bazel (`internal/skills/builtin/<name>/`) or any
skill an agent calls. The real model never runs: the fake agent plays what the
skill would print, and the test checks what Bazel does with it.

1. Write the skill's output as a spec, `e2e/fixtures/agent/specs/<name>.json`
   (steps: tool calls, sub-agents/lenses, errors; the report in
   `<name>.md` next to it; usage). Format in
   [references/fixtures.md](references/fixtures.md#scenario-specs). Preview it:
   `node e2e/tools/stream.mjs synth e2e/fixtures/agent/specs/<name>.json`.
   Or capture a real run once, by hand, outside the suite, and scrub it:
   `node e2e/tools/stream.mjs scrub raw.jsonl > e2e/fixtures/agent/<name>.jsonl`.
2. Add an agent that calls the skill:
   `{ name: '<name>', task: '/<name> {{number}}', env: { FAKE_AGENT_SCENARIO: '<name>' } }`.
3. Assert, at least: the task reached the prompt (`agentCalls()[0].prompt`),
   the skill reached the agent when it is built in
   (`agentCalls()[0].skills` contains `<name>`, from `--plugin-dir`), the log lines per lens, the
   report and findings (`job.html`, `findingPicks`), the token tally, and
   whether it may publish (`job.publishable`, MCP refusal if it posts).
   `tests/p1-formats.spec.ts` › "a synthesized spec…" is the template.

Details: [references/harness.md](references/harness.md) (architecture, env,
API helpers), [references/fixtures.md](references/fixtures.md) (prs.json,
gh.json, scenario format and directives, how to record a new stream),
[references/selectors.md](references/selectors.md) (the data-testid contract),
[references/debugging.md](references/debugging.md) (classifying failures).
