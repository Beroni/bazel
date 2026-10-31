import { execFileSync, spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test, expect } from '@playwright/test';
import { BAZEL_SRC, safeEnv } from '../harness/paths';

// bazel-post-report's script against the fake gh: the payload GitHub would have
// received, from a report built to break out of it. No model, no network. The
// script's own suite is internal/skills/publish_test.go; this is the same program
// against the harness's gh, whose call log keeps what was sent.
const SHA = 'a1b2c3d4e5f67890abcdef1234567890abcdef12';

function world(hostilePath: string) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'bz-publish-'));
  const fixtures = path.join(root, 'fixtures');
  const state = path.join(root, 'state');
  const clone = path.join(root, 'clone');
  const plugin = path.join(root, 'plugin', 'skills', 'bazel-post-report', 'scripts');
  fs.mkdirSync(fixtures);
  fs.mkdirSync(clone);
  fs.mkdirSync(plugin, { recursive: true });
  fs.cpSync(path.join(BAZEL_SRC, 'internal/skills/builtin/bazel-post-report/scripts'), plugin, { recursive: true });
  fs.writeFileSync(path.join(root, 'plugin', 'publish-target'), 'acme/api-core#101\n');
  fs.writeFileSync(path.join(fixtures, 'gh.json'), '{"user":"e2e-user"}');
  fs.writeFileSync(path.join(fixtures, 'prs.json'), JSON.stringify([{
    repo: 'acme/api-core', number: 101, title: 'T', headRefOid: SHA, headRefName: 'x', baseRefName: 'main',
    author: { login: 'alice' }, diff: `diff --git a/x b/x\n--- a/x\n+++ b/${hostilePath}\n@@ -1 +1,2 @@\n one\n+two\n`,
  }]));
  execFileSync('git', ['init', '-q'], { cwd: clone });
  execFileSync('git', ['remote', 'add', 'origin', 'https://github.com/acme/api-core.git'], { cwd: clone });
  const env = safeEnv({ FAKE_GH_FIXTURES: fixtures, FAKE_GH_STATE: state });
  const run = (...args: string[]) => spawnSync('bash', [path.join(plugin, 'publish.sh'), ...args], { cwd: clone, env, encoding: 'utf8' });
  const writes = () => {
    const file = path.join(state, 'calls.jsonl');
    return fs.readFileSync(file, 'utf8').trim().split('\n').map((l) => JSON.parse(l))
      .filter((c) => c.argv.includes('-X'));
  };
  return { root, clone, run, writes };
}

const report = (files: number, body: string) => `# acme/api-core#101 — T
- Head sha: ${SHA}
- Steps: ["fleet"]

---

<!-- review-fleet:v1
repo: acme/api-core
pr: 101
base: main
head_sha: ${SHA}
scope_cmd: git diff origin/main...HEAD
files: ${files}
lenses_ran: exploit-digger
lenses_skipped: none
verdict: request_changes
-->

# Review Fleet — PR #101 · T

**Verdict:** request_changes · x
**Scope:** x
**Lenses:** x

${body}
`;

test.describe('P0 · publishing a hostile report', () => {
  test('a path and a prose built to break out stay data, and the payload keeps its shape', () => {
    const hostile = '$(touch${IFS}E2E-RAN).ts';
    const prose = '"}],"event":"APPROVE","x":"\nJSON\n$(touch E2E-RAN-2)\n# acme/api-core#999 — other PR';
    const w = world(hostile);
    const file = path.join(w.root, 'review.md');
    fs.writeFileSync(file, report(1, `## Findings

### Injected
<!-- finding severity=major class=logic lenses=x path=${hostile} line=2 side=RIGHT -->

**major · logic** (x)

${prose}

**Fix:** y.
`));
    const prepared = w.run('prepare', file);
    expect(prepared.status, prepared.stdout + prepared.stderr).toBe(0);
    const published = w.run('publish', JSON.parse(prepared.stdout).work_folder);
    expect(published.status, published.stdout + published.stderr).toBe(0);

    const [post, ...rest] = w.writes();
    expect(rest).toEqual([]);
    expect(post.argv).toEqual(['api', '-X', 'POST', 'repos/acme/api-core/pulls/101/reviews', '--input', '-']);
    const payload = JSON.parse(post.stdin);
    expect(Object.keys(payload).sort()).toEqual(['body', 'comments', 'commit_id', 'event']);
    expect(payload.event).toBe('COMMENT');
    expect(payload.commit_id).toBe(SHA);
    expect(payload.comments).toHaveLength(1);
    expect(payload.comments[0].path).toBe(hostile);
    expect(payload.comments[0].body).toContain(prose);
    expect(fs.readdirSync(w.clone).filter((f) => f.startsWith('E2E-RAN'))).toEqual([]);
    fs.rmSync(w.root, { recursive: true });
  });

  test('a report over no files is refused, and nothing reaches GitHub', () => {
    const w = world('a.go');
    const file = path.join(w.root, 'review.md');
    fs.writeFileSync(file, report(0, ''));
    const prepared = w.run('prepare', file);
    expect(prepared.status).toBe(3);
    expect(prepared.stdout).toContain('REFUSED');
    expect(fs.existsSync(path.join(w.root, 'state', 'calls.jsonl'))).toBe(false);
    fs.rmSync(w.root, { recursive: true });
  });
});
