import fs from 'node:fs';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { test, expect } from '../harness/fixtures';
import { Bazel } from '../harness/bazel';

// The durable claim of integ/next2: <file>.posting is taken with O_EXCL before a
// Saved door calls gh, turns into <file>.posted when the review went, and goes
// when gh failed. One left by a process that is gone is an unknown outcome.
const AGENTS = [{ name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } }];
const reviewPosts = (bz: Bazel) => bz.ghCalls().filter((c) => c.argv[0] === 'api' && c.argv.some((a) => /\/pulls\/\d+\/reviews$/.test(a))).length;
const comments = (bz: Bazel) => bz.ghCalls().filter((c) => c.argv[0] === 'pr' && c.argv[1] === 'comment').length;
const sent = (bz: Bazel) => reviewPosts(bz) + comments(bz);

async function savedReview(bazel: Bazel) {
  await bazel.waitJob(await bazel.enqueue('acme/api-core#101', 'review-fleet'), (j) => j.state === 'done');
  const [saved] = (await bazel.api('GET', '/api/reviews')).json.reviews;
  return { name: saved.name as string, file: path.join(bazel.reviewsDir, saved.name) };
}
const deadPid = () => spawnSync('/usr/bin/true').pid!;
const claim = (file: string, pid: number, instance: string) =>
  fs.writeFileSync(`${file}.posting`, JSON.stringify({ pid, instance, at: new Date().toISOString() }));

test.describe('P1 · the publish claim', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  test('an orphan claim: both doors answer 409 with confirm, and only again sends, once', async ({ bazel }) => {
    const { name, file } = await savedReview(bazel);
    claim(file, deadPid(), 'gone-1');
    for (const door of ['publish', 'comment']) {
      const r = await bazel.api('POST', `/api/reviews/${name}/${door}`, {});
      expect(r.status, door).toBe(409);
      expect(r.json.confirm, door).toBe(true);
      expect(r.json.error, door).toContain('may already be on the PR');
    }
    expect(sent(bazel)).toBe(0);
    expect((await bazel.api('POST', `/api/reviews/${name}/comment`, { again: true })).status).toBe(200);
    expect(comments(bazel)).toBe(1);
    expect(fs.existsSync(`${file}.posted`)).toBe(true);
    expect(fs.existsSync(`${file}.posting`)).toBe(false);
    // Sent now: again does not send it twice.
    expect((await bazel.api('POST', `/api/reviews/${name}/comment`, { again: true })).status).toBe(409);
    expect(sent(bazel)).toBe(1);
  });

  test('a claim of a live process is busy: no confirm, and again does not take it', async ({ bazel }) => {
    const { name, file } = await savedReview(bazel);
    claim(file, process.pid, 'someone-else');
    for (const body of [{}, { again: true }]) {
      const r = await bazel.api('POST', `/api/reviews/${name}/comment`, body);
      expect(r.status, JSON.stringify(body)).toBe(409);
      expect(r.json.confirm ?? false, JSON.stringify(body)).toBe(false);
    }
    expect(sent(bazel)).toBe(0);
    expect(fs.existsSync(`${file}.posting`)).toBe(true);
  });

  test('a send that fails leaves no claim: the next try goes without a question', async ({ bazel }) => {
    const { name, file } = await savedReview(bazel);
    bazel.setGh({ fail: { comment: 'HTTP 502: Bad Gateway' } });
    expect((await bazel.api('POST', `/api/reviews/${name}/comment`, {})).status).toBeGreaterThanOrEqual(500);
    expect(fs.existsSync(`${file}.posting`)).toBe(false);
    bazel.setGh({});
    expect((await bazel.api('POST', `/api/reviews/${name}/comment`, {})).status).toBe(200);
    expect(comments(bazel)).toBe(2); // the failed call, then the one that went
    expect(bazel.ghCalls().filter((c) => c.argv[1] === 'comment' && c.exit === 0)).toHaveLength(1);
  });

  test('two Bazels on one BAZEL_HOME, the same Saved review at once: one write reaches GitHub', async ({ bazel }) => {
    const { name } = await savedReview(bazel);
    const other = new Bazel({ share: bazel });
    await other.start();
    try {
      for (const door of ['comment', 'publish', 'comment']) {
        const answers = await Promise.all([bazel, other].map((bz) => bz.api('POST', `/api/reviews/${name}/${door}`, {})));
        for (const r of answers) if (r.status === 202) await [bazel, other][answers.indexOf(r)].waitJob(r.json.id, (j) => j.state !== 'queued' && j.state !== 'running');
      }
      expect(sent(bazel)).toBe(1);
    } finally {
      await other.stop();
      other.cleanup();
    }
  });
});
