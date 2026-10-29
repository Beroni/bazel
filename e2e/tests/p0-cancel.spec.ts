import { test, expect } from '../harness/fixtures';
import { tickByFilter, ui } from '../harness/ui';

test.describe('P0 · cancel', () => {
  test('cancelling a running job from its card kills the agent and saves nothing', async ({ app, bazel }) => {
    await tickByFilter(app, 'dark mode');
    await ui.agentSelect(app).selectOption('slow-lens');
    await ui.runButton(app).click();

    const card = ui.jobCard(app, 'Dark mode toggle in settings');
    await expect(card).toBeVisible();
    // Running for real: the agent started and its first tool call reached the log.
    await expect.poll(() => bazel.agentCalls().length).toBe(1);
    const id = (await bazel.api('GET', '/api/jobs')).json.jobs[0].id;
    await bazel.waitJob(id, (j) => j.state === 'running' && j.log_lines >= 2);

    await ui.cancelButton(card).click();
    await bazel.waitJob(id, (j) => j.state === 'canceled');
    await expect(card.getByText(/cancel+ed/)).toBeVisible();
    await expect(ui.cancelButton(card)).toHaveCount(0);

    // Releasing the scenario now must change nothing: the process is dead.
    bazel.release('release-slow');
    await new Promise((r) => setTimeout(r, 300));
    const job = await bazel.job(id);
    expect(job.state).toBe('canceled');
    expect(bazel.savedReviews()).toEqual([]);
    expect(bazel.clones()).toEqual([]);
  });
});

test.describe('P0 · cancel (one worker)', () => {
  test.use({ bazelOptions: { flags: ['--jobs', '1'] } });

  test('cancelling a queued job (jobs=1) never starts its agent', async ({ bazel }) => {
    const busy = await bazel.enqueue('acme/api-core#101', 'slow-lens');
    await bazel.waitJob(busy, (j) => j.state === 'running');
    const queued = await bazel.enqueue('acme/web-app#7', 'review-fleet');
    expect((await bazel.job(queued)).state).toBe('queued');

    const { status } = await bazel.api('POST', `/api/jobs/${queued}/cancel`);
    expect(status).toBe(200);
    expect((await bazel.job(queued)).state).toBe('canceled');

    bazel.release('release-slow');
    await bazel.waitJob(busy, (j) => j.state === 'done');
    expect(bazel.agentCalls().map((c) => c.scenario)).toEqual(['claude-slow']);
  });
});
