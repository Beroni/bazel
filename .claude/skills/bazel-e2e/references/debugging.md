# Debugging e2e failures

## Classify first

| Signal | Likely |
| --- | --- |
| Same failure on every run, assertion on server/API/gh-call data | product bug |
| `strict mode violation`, locator resolves to 0 or >1 elements | test bug (selector) — fix it in `harness/ui.ts` |
| Passes alone, fails under `--repeat-each 5 --workers 8` | flake: a race in the test (missing wait) or in Bazel (report it) |
| `bazel exited … before serving` | config the server rejected — read `server.log` (yaml, `format`) |
| `preflight: gh resolves to …` | your bin dir or PATH changed — never "fix" by widening PATH |
| `isolation broken: ~/.bazel changed` | something ran without `BAZEL_HOME` — stop and find it |
| `fake gh: unsupported: gh …` in server.log or a job error | Bazel started a new gh call: teach the fake (and a test) |
| `fake-agent: no scenario named …` | `FAKE_AGENT_SCENARIO` typo, or a new agent without `env` |

## Useful commands

```sh
cd e2e
npx playwright test -g 'publish' --headed --workers 1     # watch
npx playwright test -g 'publish' --debug                  # step with the inspector
npx playwright show-trace .out/results/<dir>/trace.zip
jq -c '.argv' .out/runs/bz-*/gh/calls.jsonl               # what reached "GitHub"
jq -r '.prompt' .out/runs/bz-*/agent/calls.jsonl | less   # exact prompts
```

Run the fakes by hand:

```sh
export FAKE_GH_FIXTURES=$PWD/fixtures/default FAKE_GH_STATE=/tmp/fgh GIT_ALLOW_PROTOCOL=file
.out/bin/gh search prs --json number,title -- "repo:acme/api-core"
FAKE_AGENT_SCENARIOS=$PWD/fixtures/agent FAKE_AGENT_SCENARIO=claude-findings \
  FAKE_AGENT_STATE=/tmp/fag .out/bin/fake-agent < /dev/null
```

## Reporting a product bug

1. Reproduce it in a test; mark it `test.fail(true, 'BUG-QA-NN: one line')`
   with a comment: what happens, what README/PRD says should happen.
2. Report to the Bazel Manager: steps, expected vs. actual, the test file and
   name, and the evidence (gh-call or agent-call excerpt, server.log line).
3. When the fix lands, the test turns into "expected to fail, but passed":
   delete the `test.fail` line in the same PR as the fix.
