import { test, expect, knownBug } from '../harness/fixtures';
import { FlakyProxy } from '../harness/flaky-proxy';
import { ui } from '../harness/ui';

const AGENTS = [
  { name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } },
  { name: 'slow-lens', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-slow' } },
];

test.describe('P1 · the connection drops and comes back', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  // BUG-QA-09: the server stays up and the page's EventSource dies (the lid
  // closes, the wifi blips). It reconnects by itself and `hello` carries the
  // same instance, so adoptInstance does nothing, and the events of the gap are
  // gone: nothing asks the server what happened meanwhile. A job that ended in
  // the gap stays "running" with its clock counting, Report stays disabled, a
  // job started in the gap never shows, until a reload. Expected: on
  // reconnecting the page reads the state again.
  test('a job that ended during the drop shows as done afterwards', async ({ page, bazel }) => {
    knownBug('BUG-QA-09', 'nothing resyncs after the event stream reconnects');
    const proxy = new FlakyProxy(bazel.port);
    await proxy.start();
    try {
      await page.goto(`http://127.0.0.1:${proxy.port}/`);
      await expect(ui.prCount(page)).toHaveText('5 PRs');
      const id = await bazel.enqueue('acme/api-core#101', 'slow-lens');
      await bazel.waitJob(id, (j) => j.state === 'running');
      const card = page.getByTestId('job-card').first();
      await expect(card).toHaveAttribute('data-state', 'running');

      proxy.drop();
      await expect(page.getByTestId('conn-status')).toHaveAttribute('data-state', 'closed');
      bazel.release('release-slow');
      await bazel.waitJob(id, (j) => j.state === 'done');
      const other = await bazel.enqueue('acme/web-app#7', 'review-fleet');
      await bazel.waitJob(other, (j) => j.state === 'done');

      proxy.resume();
      await expect(page.getByTestId('conn-status')).toHaveAttribute('data-state', 'open', { timeout: 15_000 });
      await expect(page.getByTestId('job-card').filter({ hasText: 'api-core #101' })).toHaveAttribute('data-state', 'done', { timeout: 5_000 });
      await expect(page.getByTestId('job-card')).toHaveCount(2);
    } finally {
      proxy.stop();
    }
  });

  // The same gap, with the list: a read that failed while down says so forever.
  test('a PR list that failed during the drop loads again', async ({ page, bazel }) => {
    knownBug('BUG-QA-09', 'nothing resyncs after the event stream reconnects');
    const proxy = new FlakyProxy(bazel.port);
    await proxy.start();
    try {
      await page.goto(`http://127.0.0.1:${proxy.port}/`);
      await expect(ui.prCount(page)).toHaveText('5 PRs');
      proxy.drop();
      await page.getByTestId('refresh').click();
      await expect(page.locator('#pr-list')).toContainText('Failed to fetch');
      proxy.resume();
      await expect(page.getByTestId('conn-status')).toHaveAttribute('data-state', 'open', { timeout: 15_000 });
      await expect(ui.prRow(page, 'Bump dependencies')).toBeVisible({ timeout: 5_000 });
    } finally {
      proxy.stop();
    }
  });

  // The control: a real reload does read the truth, which is the workaround.
  test('a reload after the drop shows the truth', async ({ page, bazel }) => {
    const proxy = new FlakyProxy(bazel.port);
    await proxy.start();
    try {
      await page.goto(`http://127.0.0.1:${proxy.port}/`);
      await expect(ui.prCount(page)).toHaveText('5 PRs');
      const id = await bazel.enqueue('acme/api-core#101', 'slow-lens');
      await bazel.waitJob(id, (j) => j.state === 'running');
      proxy.drop();
      bazel.release('release-slow');
      await bazel.waitJob(id, (j) => j.state === 'done');
      proxy.resume();
      await page.reload();
      await expect(page.getByTestId('job-card').first()).toHaveAttribute('data-state', 'done');
    } finally {
      proxy.stop();
    }
  });
});
