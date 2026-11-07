import { test, expect } from '../harness/fixtures';
import { Bazel } from '../harness/bazel';

// The publish claim of integ/next2: a Saved door claims the review before it calls
// gh, so two doors or two Bazels cannot both send it, and a send that never said
// how it ended (a crash, a gh timeout) asks a person before going again.
const AGENTS = [{ name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } }];
const reviewPosts = (bz: Bazel) => bz.ghCalls().filter((c) => c.argv[0] === 'api' && c.argv.some((a) => /\/pulls\/\d+\/reviews$/.test(a))).length;

async function savedReview(bazel: Bazel) {
  await bazel.waitJob(await bazel.enqueue('acme/api-core#101', 'review-fleet'), (j) => j.state === 'done');
  const [saved] = (await bazel.api('GET', '/api/reviews')).json.reviews;
  return { name: saved.name as string };
}
const okComments = (bz: Bazel) => bz.ghCalls().filter((c) => c.argv[0] === 'pr' && c.argv[1] === 'comment' && c.exit === 0).length;

/** A comment from Saved that gh holds open, and a server that dies in the middle of it. */
async function diesWhileSending(bazel: Bazel, name: string) {
  bazel.setGh({ hold: { comment: 'gate' } });
  const pending = bazel.api('POST', `/api/reviews/${name}/comment`, {}).catch(() => null);
  await expect.poll(() => bazel.holdingGh('gate')).toBe(true);
  await bazel.kill();
  await pending;
  bazel.releaseGh('gate', 'fail'); // the gh it had started ends on its own; nobody hears it
  bazel.setGh({});
}

test.describe('P1 · the publish claim', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  // What the user sees, not the claim's file: its format is the server's business.
  test('a server that died while sending: both doors ask (409 with confirm), and only again sends, once', async ({ bazel }) => {
    const { name } = await savedReview(bazel);
    await diesWhileSending(bazel, name);
    await bazel.restart();
    for (const door of ['publish', 'comment']) {
      const r = await bazel.api('POST', `/api/reviews/${name}/${door}`, {});
      expect(r.status, door).toBe(409);
      expect(r.json.confirm, door).toBe(true);
    }
    expect(okComments(bazel) + reviewPosts(bazel)).toBe(0);
    expect((await bazel.api('POST', `/api/reviews/${name}/comment`, { again: true })).status).toBe(200);
    expect(okComments(bazel)).toBe(1);
    // Sent now: again does not send it twice.
    expect((await bazel.api('POST', `/api/reviews/${name}/comment`, { again: true })).status).toBe(409);
    expect(okComments(bazel) + reviewPosts(bazel)).toBe(1);
  });

  test('a send that fails asks nothing the next time', async ({ bazel }) => {
    const { name } = await savedReview(bazel);
    bazel.setGh({ fail: { comment: 'HTTP 502: Bad Gateway' } });
    expect((await bazel.api('POST', `/api/reviews/${name}/comment`, {})).status).toBeGreaterThanOrEqual(400);
    bazel.setGh({});
    const r = await bazel.api('POST', `/api/reviews/${name}/comment`, {});
    expect(r.status, JSON.stringify(r.json)).toBe(200);
    expect(okComments(bazel)).toBe(1);
  });

  test('two Bazels on one BAZEL_HOME: while one sends, the other is told it is being published, and one write reaches GitHub', async ({ bazel }) => {
    const { name } = await savedReview(bazel);
    const other = new Bazel({ share: bazel });
    await other.start();
    try {
      bazel.setGh({ hold: { comment: 'gate' } });
      const first = bazel.api('POST', `/api/reviews/${name}/comment`, {});
      await expect.poll(() => bazel.holdingGh('gate')).toBe(true);
      for (const door of ['comment', 'publish']) {
        const r = await other.api('POST', `/api/reviews/${name}/${door}`, { again: true });
        expect(r.status, door).toBe(409);
        expect(r.json.confirm ?? false, door).toBe(false);
        expect(r.json.error, door).toMatch(/publish/i);
      }
      bazel.releaseGh('gate');
      expect((await first).status).toBe(200);
      bazel.setGh({});
      for (const bz of [bazel, other]) {
        const r = await bz.api('POST', `/api/reviews/${name}/publish`, { again: true });
        expect(r.status).toBe(409);
      }
      expect(okComments(bazel) + reviewPosts(bazel)).toBe(1);
    } finally {
      await other.stop();
      other.cleanup();
    }
  });

  test('two Bazels on one BAZEL_HOME, every door at once: one write reaches GitHub', async ({ bazel }) => {
    const { name } = await savedReview(bazel);
    const other = new Bazel({ share: bazel });
    await other.start();
    try {
      for (const door of ['comment', 'publish', 'comment']) {
        const answers = await Promise.all([bazel, other].map((bz) => bz.api('POST', `/api/reviews/${name}/${door}`, {})));
        for (const [i, r] of answers.entries()) if (r.status === 202) await [bazel, other][i].waitJob(r.json.id, (j) => j.state !== 'queued' && j.state !== 'running');
      }
      expect(okComments(bazel) + reviewPosts(bazel)).toBe(1);
    } finally {
      await other.stop();
      other.cleanup();
    }
  });
});
