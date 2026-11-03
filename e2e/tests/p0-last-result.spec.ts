import { test, expect } from '../harness/fixtures';
import { Bazel } from '../harness/bazel';
import { openJob, ui } from '../harness/ui';

// A fleet that runs its lenses in the background ends a first turn saying so and
// writes the report in a later one. The report is the last result; a run that
// only left the status line has nothing to publish.

const writes = (bz: Bazel) =>
  bz.ghCalls().filter((c) => (c.argv[0] === 'pr' && c.argv[1] === 'comment') || (c.argv[0] === 'api' && c.argv[1] !== 'user'));
const TITLE = 'Add rate limiter to the public API';

const AGENTS = [
  { name: 'background-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-background' } },
  { name: 'narrating-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-narration' } },
  { name: 'doubtful-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-background-doubt' } },
  { name: 'unfinished-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-background-pending' } },
];
const PIPELINES = [
  { name: 'review-and-publish', steps: ['background-fleet', 'publish'] },
  { name: 'narrate-and-publish', steps: ['narrating-fleet', 'publish'] },
  { name: 'doubt-and-publish', steps: ['doubtful-fleet', 'publish'] },
  { name: 'unfinished-and-publish', steps: ['unfinished-fleet', 'publish'] },
];

test.describe('P0 · the report is the last result', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS, pipelines: PIPELINES } } });

  test('lenses in the background: the last turn is the report, and a process that lingers is ended', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'review-and-publish');
    const job = await bazel.waitJob(id, (j) => j.state === 'done');
    expect(job.body).toContain('E2E-BG-REPORT');
    expect(job.body).not.toContain('E2E-BG-FIRST');
    expect(job.incomplete ?? '').toBe('');
    expect(job.awaits_publish).toBe(true);
    const saved = (await bazel.api('GET', '/api/reviews')).json.reviews;
    expect(saved).toHaveLength(1);
    const file = (await bazel.api('GET', `/api/reviews/${saved[0].name}`)).json;
    expect(file.body).toContain('E2E-BG-FINDING');
    expect(file.body).not.toContain('E2E-BG-FIRST');

    await app.reload();
    const viewer = await openJob(app, TITLE);
    await expect(ui.findingPicks(viewer)).toHaveCount(1);
    await expect(viewer.getByTestId('review-incomplete')).toHaveCount(0);
    await expect(ui.publishInline(viewer)).toBeEnabled();
  });

  test('only a short line and no stream signal: a warning, and it can still go', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'narrate-and-publish');
    const job = await bazel.waitJob(id, (j) => j.state === 'done');
    expect(job.incomplete ?? '').toBe('');
    expect(job.short).toBe(true);
    expect(job.awaits_publish).toBe(true);

    await app.reload();
    const viewer = await openJob(app, TITLE);
    await expect(viewer.getByTestId('review-short')).toBeVisible();
    await expect(viewer.getByTestId('review-incomplete')).toHaveCount(0);
    await expect(ui.publishInline(viewer)).toBeEnabled();
    app.removeAllListeners('dialog');
    let asked = '';
    app.once('dialog', (d) => { asked = d.message(); void d.dismiss(); });
    await ui.publishInline(viewer).click();
    await expect.poll(() => asked).toContain('short and has no findings');
    expect(writes(bazel)).toEqual([]);

    const [saved] = (await bazel.api('GET', '/api/reviews')).json.reviews;
    expect((await bazel.api('GET', `/api/reviews/${saved.name}`)).json.short).toBe(true);
  });

  test('a lens still out when the process ends: incomplete, and no door to the PR', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'unfinished-and-publish');
    const job = await bazel.waitJob(id, (j) => j.state === 'done');
    expect(job.incomplete).toContain('background agent');
    expect(job.awaits_publish ?? false).toBe(false);

    await app.reload();
    await expect(ui.jobCard(app, TITLE)).toContainText(/incomplete/i);
    const viewer = await openJob(app, TITLE);
    await expect(viewer.getByTestId('review-incomplete')).toBeVisible();
    await expect(ui.keepHere(viewer)).toHaveCount(0);
    for (const b of [ui.publishInline(viewer), viewer.getByTestId('paste-comment')]) {
      await expect(b).toBeDisabled();
      await expect(b).toHaveAttribute('title', /background/);
    }
    await expect(viewer.getByTestId('run-again')).toBeEnabled();
    expect((await bazel.api('POST', `/api/jobs/${id}/post`, { skip: [] })).status).toBe(409);
    expect((await bazel.api('POST', `/api/jobs/${id}/publish`, { skip: [] })).status).toBe(409);
    const [saved] = (await bazel.api('GET', '/api/reviews')).json.reviews;
    expect((await bazel.api('POST', `/api/reviews/${saved.name}/comment`, { skip: [] })).status).toBe(409);
    expect((await bazel.api('POST', `/api/reviews/${saved.name}/publish`, { skip: [] })).status).toBe(409);
    expect(writes(bazel)).toEqual([]);
    await viewer.getByTestId('run-again').click();
    await expect.poll(() => bazel.agentCalls().length).toBe(2);
  });

  test('the last turn began with a lens out: a warning, and it can still go', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'doubt-and-publish');
    const job = await bazel.waitJob(id, (j) => j.state === 'done');
    expect(job.body).toContain('E2E-DOUBT-REPORT');
    expect(job.incomplete ?? '').toBe('');
    expect(job.doubt).toContain('background');
    expect(job.awaits_publish).toBe(true);
    await app.reload();
    const viewer = await openJob(app, TITLE);
    await expect(viewer.getByTestId('review-doubt')).toBeVisible();
    await expect(ui.publishInline(viewer)).toBeEnabled();
    app.removeAllListeners('dialog');
    let asked = '';
    app.once('dialog', (d) => { asked = d.message(); void d.dismiss(); });
    await ui.publishInline(viewer).click();
    await expect.poll(() => asked).toContain('background agents');
    const [saved] = (await bazel.api('GET', '/api/reviews')).json.reviews;
    expect((await bazel.api('GET', `/api/reviews/${saved.name}`)).json.doubt).toContain('background');
    expect(writes(bazel)).toEqual([]);
  });
});
