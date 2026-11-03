import { test, expect } from '../harness/fixtures';
import { tickByFilter, ui } from '../harness/ui';

// test/e2e-stability: under load the suite failed now and then, a different test
// each time. Most of those tests read `running` as "the agent started" and gave
// what came next the default 10s, but a job is running from its clone on, and
// with the load average near 100 the clone and the spawn took 14 to 41s. A clone
// slowed down on purpose makes that gap certain: these must pass all the same,
// because they wait on the agent itself.
const AGENTS = [
  { name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } },
  { name: 'slow-lens', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-slow' } },
];

test.describe('P1 · a slow machine', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS }, gh: { slow_clone_ms: 12_000 } } });

  test('running is not the agent: the wait is on the agent', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/web-app#7', 'slow-lens');
    await bazel.waitJob(id, (j) => j.state === 'running');
    expect(bazel.agentCalls(), 'the clone is still going').toEqual([]);
    const [call] = await bazel.waitAgents(1);
    expect(bazel.agentAlive(call.pid)).toBe(true);
    expect((await bazel.api('POST', `/api/jobs/${id}/cancel`)).status).toBe(200);
    await expect.poll(() => bazel.agentAlive(call.pid)).toBe(false);
  });

  test('a review from the page lands, however long the clone takes', async ({ app, bazel }) => {
    await tickByFilter(app, 'rate limiter');
    await ui.agentSelect(app).selectOption('review-fleet');
    await ui.runButton(app).click();
    const [job] = (await bazel.api('GET', '/api/jobs')).json.jobs;
    await bazel.waitJob(job.id, (j) => j.state === 'done');
    await expect(ui.viewer(app).getByText('E2E-FINDING-ONE')).toBeVisible();
  });
});
