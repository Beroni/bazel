import { execFileSync, spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test, expect } from '@playwright/test';
import { BAZEL_SRC, safeEnv } from '../harness/paths';

// bazel-post-report's real script against the fake gh: a clean report that
// Bazel saved as a review of what changed since a commit (its "- Since:" line)
// is no all-clear, whatever scope the model wrote. The control is the same
// report without the line, which is one.
const HEAD = 'a1b2c3d4e5f67890abcdef1234567890abcdef12';
const SINCE = '0123456789abcdef0123456789abcdef01234567';

function world() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'bz-since-'));
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
    repo: 'acme/api-core', number: 101, title: 'T', headRefOid: HEAD, headRefName: 'x', baseRefName: 'main',
    author: { login: 'alice' }, diff: 'diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n one\n+two\n',
  }]));
  execFileSync('git', ['init', '-q'], { cwd: clone });
  execFileSync('git', ['remote', 'add', 'origin', 'https://github.com/acme/api-core.git'], { cwd: clone });
  const env = safeEnv({ FAKE_GH_FIXTURES: fixtures, FAKE_GH_STATE: state });
  const run = (...args: string[]) => spawnSync('bash', [path.join(plugin, 'publish.sh'), ...args], { cwd: clone, env, encoding: 'utf8' });
  const writes = () => fs.readFileSync(path.join(state, 'calls.jsonl'), 'utf8').trim().split('\n').map((l) => JSON.parse(l)).filter((c) => c.argv.includes('-X'));
  return { root, run, writes };
}

// A complete, clean review-fleet report of the whole PR (scope origin/main...HEAD).
const clean = (bazelLines: string) => `# acme/api-core#101 — T
- Author: @alice
- Head sha: ${HEAD}
- Agent: fleet
- Steps: ["fleet"]
${bazelLines}- Truncated: no

---

<!-- review-fleet:v1
repo: acme/api-core
pr: 101
base: main
head_sha: ${HEAD}
scope_cmd: git diff origin/main...HEAD
files: 1
lenses_ran: exploit-digger
lenses_skipped: none
verdict: approve
-->

# Review Fleet — PR #101 · T

**Verdict:** approve · no findings
**Scope:** \`git diff origin/main...HEAD\` — 1 files · base \`main\` · head \`${HEAD.slice(0, 7)}\`
**Lenses:** exploit-digger
`;

function publish(file: string) {
  const w = world();
  fs.writeFileSync(path.join(w.root, 'review.md'), file);
  const prepared = w.run('prepare', path.join(w.root, 'review.md'));
  const out = { prepared, body: '' as string, writes: [] as any[] };
  if (prepared.status === 0) {
    const published = w.run('publish', JSON.parse(prepared.stdout).work_folder);
    expect(published.status, published.stdout + published.stderr).toBe(0);
    out.writes = w.writes();
    out.body = out.writes.length ? JSON.parse(out.writes[0].stdin).body : '';
  }
  fs.rmSync(w.root, { recursive: true });
  return out;
}

test.describe('P0 · a review of what changed is no all-clear', () => {
  test('control: the clean report of the whole PR is an all-clear', () => {
    const r = publish(clean(''));
    expect(r.prepared.status, r.prepared.stdout + r.prepared.stderr).toBe(0);
    expect(r.body).toContain(`all-clear @ ${HEAD}`);
  });

  test('with Bazel\'s "- Since:" line and a whole scope: posted, but not as an all-clear', () => {
    const r = publish(clean(`- Since: ${SINCE}\n`));
    expect(r.prepared.status, r.prepared.stdout + r.prepared.stderr).toBe(0);
    expect(r.writes).toHaveLength(1);
    expect(r.body).not.toContain('all-clear');
  });

  test('a Since that is no commit is refused, and nothing reaches GitHub', () => {
    const r = publish(clean('- Since: not-a-commit\n'));
    expect(r.prepared.status).toBe(3);
    expect(r.prepared.stdout).toContain("Bazel's Since line is not a commit");
    expect(r.writes).toEqual([]);
  });

  test('a report over the changes since another commit than Bazel asked for is refused', () => {
    const other = 'fedcba9876543210fedcba9876543210fedcba98';
    const r = publish(clean(`- Since: ${SINCE}\n`).replace('scope_cmd: git diff origin/main...HEAD', `scope_cmd: git diff ${other}..HEAD`));
    expect(r.prepared.status).toBe(3);
    expect(r.prepared.stdout).toContain('another commit than Bazel asked for');
  });
});
