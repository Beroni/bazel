import { test, expect } from '../harness/fixtures';
import { Bazel } from '../harness/bazel';
import { ui } from '../harness/ui';

// BUG-QA-34: a review that went to the PR could go again from the Saved tab
// after a restart. The guard against publishing twice (S7) lives in memory,
// and the saved file keeps no word that it was published: the restart forgets,
// and the PR gets the same review or comment a second time. Expected: every
// door of Saved refuses (409) a review already on the PR, restart or not, and
// the Saved tab says so instead of offering Publish.
const AGENTS = [{ name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } }];
const PIPELINES = [{ name: 'read-before-sending', steps: ['review-fleet', 'publish'] }];

const reviewPosts = (bz: Bazel) => bz.ghCalls().filter((c) => c.argv[0] === 'api' && c.argv.some((a) => /\/pulls\/\d+\/reviews$/.test(a))).length;
const comments = (bz: Bazel) => bz.ghCalls().filter((c) => c.argv[0] === 'pr' && c.argv[1] === 'comment').length;
const sent = (bz: Bazel) => reviewPosts(bz) + comments(bz);

async function published(bazel: Bazel, how: 'pipeline' | 'inline' | 'comment') {
  const id = await bazel.enqueue('acme/api-core#101', how === 'pipeline' ? 'read-before-sending' : 'review-fleet');
  await bazel.waitJob(id, (j) => j.state === 'done');
  expect((await bazel.api('POST', `/api/jobs/${id}/${how === 'comment' ? 'post' : 'publish'}`, {})).status).toBeLessThan(300);
  const job = await bazel.waitJob(id, (j) => j.posted || !!j.post_error);
  expect(job.post_error ?? '').toBe('');
  expect(sent(bazel)).toBe(1);
  const [saved] = (await bazel.api('GET', '/api/reviews')).json.reviews;
  return saved.name as string;
}

test.describe('P1 · a saved review goes to the PR once, restart or not', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS, pipelines: PIPELINES } } });

  for (const how of ['pipeline', 'inline', 'comment'] as const) {
    test(`published by ${how}, then a restart: neither Saved door sends it again`, async ({ bazel }) => {
      
      const name = await published(bazel, how);
      await bazel.restart();
      for (const door of ['publish', 'comment']) {
        const r = await bazel.api('POST', `/api/reviews/${name}/${door}`, {});
        if (r.status === 202) await bazel.waitJob(r.json.id, (j) => j.state !== 'queued' && j.state !== 'running');
        expect(r.status, door).toBe(409);
      }
      expect(sent(bazel)).toBe(1);
    });
  }

  test('the Saved tab says it is on the PR and offers no Publish', async ({ app, bazel }) => {
    
    await published(bazel, 'pipeline');
    await app.reload();
    await ui.savedTab(app).click();
    await app.getByTestId('saved-row').first().click();
    await expect(app.getByTestId('already-published')).toBeVisible();
    await expect(app.getByTestId('saved-publish')).toHaveCount(0);
  });
});
