import { test, expect } from '../harness/fixtures';
import { recordPosts, settled, ui } from '../harness/ui';

// #32: a restart keeps the queue. What was pending comes back paused, runs on a
// click and on nothing else, and an agent that may publish asks first.
const AGENTS = [
  { name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } },
  { name: 'slow-lens', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-slow' } },
  { name: 'poster', task: '/review-fleet {{number}} --post', env: { FAKE_AGENT_SCENARIO: 'claude-slow' } },
];

test.describe('P1 · a queue that survives a restart', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  test('a job comes back paused and runs on a click', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'slow-lens');
    await bazel.waitJob(id, (j) => j.state === 'running');
    await app.reload();
    await bazel.restart({ samePort: true, downMs: 1_000 });
    const card = ui.jobCard(app, 'Add rate limiter to the public API');
    await expect(card).toHaveAttribute('data-state', 'paused', { timeout: 30_000 });
    await expect(ui.banner(app)).toContainText('paused');

    // A job that comes and goes shows the queue is moving: the paused one stays.
    const before = bazel.agentCalls().length;
    const other = await bazel.enqueue('acme/web-app#7', 'review-fleet');
    await bazel.waitJob(other, (j) => j.state === 'done');
    expect(bazel.agentCalls()).toHaveLength(before + 1);
    await expect(card).toHaveAttribute('data-state', 'paused');

    await card.click();
    await app.getByTestId('job-resume').click();
    await expect(card).not.toHaveAttribute('data-state', 'paused');
    await expect.poll(() => bazel.agentCalls().length).toBe(before + 2);
    bazel.release('release-slow');
  });

  test('a paused agent that may publish asks on Run, and saying no runs nothing', async ({ app, bazel, dialogs }) => {
    const a = await bazel.enqueue('acme/api-core#101', 'slow-lens');
    const b = await bazel.enqueue('acme/api-core#102', 'slow-lens');
    await bazel.waitJob(a, (j) => j.state === 'running');
    await bazel.waitJob(b, (j) => j.state === 'running');
    await bazel.enqueue('acme/api-core#103', 'poster');
    await app.reload();
    await bazel.restart({ samePort: true, downMs: 1_000 });
    const card = app.locator('[data-testid=job-card][data-pr-key="acme/api-core#103"]');
    await expect(card).toHaveAttribute('data-state', 'paused', { timeout: 30_000 });
    await card.click();

    const before = bazel.agentCalls().length;
    const posts = recordPosts(app);
    dialogs.accept = false;
    await app.getByTestId('job-resume').click();
    await expect.poll(() => dialogs.messages.at(-1) ?? '').toContain('--post');
    await settled(app);
    expect(posts).toEqual([]);
    await expect(card).toHaveAttribute('data-state', 'paused');
    expect(bazel.agentCalls()).toHaveLength(before);

    dialogs.accept = true;
    await app.getByTestId('job-resume').click();
    await expect(card).not.toHaveAttribute('data-state', 'paused');
    bazel.release('release-slow');
  });
});
