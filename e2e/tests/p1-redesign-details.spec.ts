import { test, expect } from '../harness/fixtures';
import { Bazel } from '../harness/bazel';
import { expandFindings, openJob, recordPosts, settled, ui } from '../harness/ui';
import { Page } from '@playwright/test';

const TITLE = 'Add rate limiter to the public API';

const plain = (name: string, scenario: string, extra: Record<string, unknown> = {}) =>
  ({ name, task: '/exploit-digger {{number}}', command: 'fake-agent', args: [], format: 'plain', env: { FAKE_AGENT_SCENARIO: scenario }, ...extra });
const AGENTS = [
  { name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } },
  { name: 'slow-lens', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-slow' } },
  plain('sections', 'sections-report'),
  plain('sections-stay', 'sections-report', { publishable: false }),
  plain('hidden', 'hidden-report'),
];

async function reviewed(app: Page, bazel: Bazel, agent: string) {
  const id = await bazel.enqueue('acme/api-core#101', agent);
  await bazel.waitJob(id, (j) => j.state === 'done');
  await app.reload();
  return { id, viewer: await openJob(app, TITLE) };
}

test.describe('P1 · redesign details', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  test('a failed PR page keeps its error on screen, with Retry at hand', async ({ app, bazel }) => {
    bazel.setGh({ fail: { 'view:acme/api-core#101': 'HTTP 502: E2E-DETAILS-DOWN' } });
    await ui.prRow(app, TITLE).click();
    const error = app.getByTestId('pr-error');
    await expect(error).toContainText('E2E-DETAILS-DOWN');
    // A job ending redraws the page: that must not wipe it.
    const id = await bazel.enqueue('acme/web-app#7', 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    await expect(ui.jobCard(app, 'Dark mode toggle in settings')).toHaveAttribute('data-state', 'done');
    await expect(error).toBeVisible();
    await expect(error.getByTestId('retry')).toBeVisible();
  });

  // BUG-QA-08, fixed in #30: the server kept a failed read for 30s and Retry
  // went through the same door, answering the old error with nothing saying why.
  test('Retry reads again once GitHub is back', async ({ app, bazel }) => {
    bazel.setGh({ fail: { 'view:acme/api-core#101': 'HTTP 502: E2E-DETAILS-DOWN' } });
    await ui.prRow(app, TITLE).click();
    await expect(app.getByTestId('pr-error')).toBeVisible();
    bazel.setGh({});
    await app.getByTestId('retry').click();
    await expect(app.getByTestId('pr-page')).toHaveAttribute('data-loaded', 'true', { timeout: 5_000 });
    await expect(app.getByTestId('pr-error')).toHaveCount(0);
  });

  test('a markdown image becomes a link in the PR body and the report, and is never fetched', async ({ app, bazel }) => {
    const fetched: string[] = [];
    app.on('request', (r) => { if (r.url().includes('attacker.example')) fetched.push(r.url()); });
    bazel.updatePR('acme/api-core#101', { body: 'Look: ![the chart](https://attacker.example/body.png)' });
    await bazel.api('GET', '/api/prs?refresh=1');
    await app.reload();
    await ui.prRow(app, TITLE).click();
    const page = app.getByTestId('pr-page');
    await expect(page.getByRole('link', { name: 'image: the chart' })).toHaveAttribute('href', 'https://attacker.example/body.png');

    const { viewer } = await reviewed(app, bazel, 'sections');
    await expandFindings(viewer);
    await app.getByRole('tab', { name: 'Report' }).click();
    await expect(viewer.getByRole('link', { name: 'image: chart' })).toBeVisible();
    expect(await viewer.locator('img[src*="attacker"]').count()).toBe(0);
    expect(fetched).toEqual([]);
  });

  test('the confirm says what goes; dismissing it sends nothing', async ({ app, bazel, dialogs }) => {
    const { viewer } = await reviewed(app, bazel, 'review-fleet');
    await app.getByRole('tab', { name: 'Report' }).click();
    await ui.findingPicks(viewer).nth(1).uncheck();
    const posts = recordPosts(app);
    dialogs.accept = false;
    await ui.publishInline(viewer).click();
    await expect.poll(() => dialogs.messages.length).toBe(1);
    expect(dialogs.messages[0]).toContain('Goes to the PR: 1 of 2 findings (whole bodies)');
    expect(dialogs.messages[0]).toContain('1 unticked finding will be left out.');
    await settled(app);
    expect(posts).toEqual([]);
    expect(bazel.agentCalls()).toHaveLength(1);
  });

  test('the rest of the report is open and flagged when it goes to the PR', async ({ app, bazel, dialogs }) => {
    const { viewer } = await reviewed(app, bazel, 'sections');
    await app.getByRole('tab', { name: 'Report' }).click();
    const more = viewer.getByTestId('review-more');
    await expect(more).toHaveAttribute('data-publishes', 'true');
    await expect(more).toHaveJSProperty('open', true);
    await expect(more).toContainText('also goes to the PR');
    await expect(viewer.getByText('E2E-SECTION-MORE')).toBeVisible();

    dialogs.accept = false;
    await ui.publishInline(viewer).click();
    await expect.poll(() => dialogs.messages.length).toBe(1);
    expect(dialogs.messages[0]).toContain('more section');
  });

  test('…and closed, without the label, when it does not', async ({ app, bazel }) => {
    const { viewer } = await reviewed(app, bazel, 'sections-stay');
    await app.getByRole('tab', { name: 'Report' }).click();
    const more = viewer.getByTestId('review-more');
    await expect(more).toHaveAttribute('data-publishes', 'false');
    await expect(more).toHaveJSProperty('open', false);
    await expect(more).not.toContainText('also goes to the PR');
  });
});

test.describe('P1 · what the markdown hides', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  test('hidden text is flagged on top, counted in the confirm, and a malformed finding comment shows', async ({ app, bazel, dialogs }) => {
    const { viewer } = await reviewed(app, bazel, 'hidden');
    await app.getByRole('tab', { name: 'Report' }).click();
    const hidden = viewer.getByTestId('review-hidden');
    await expect(hidden).toContainText('E2E-HIDDEN-COMMENT');
    await expect(hidden).toContainText('E2E-HIDDEN-NOTE');
    const pieces = await hidden.locator('pre').count();
    expect(pieces).toBeGreaterThanOrEqual(2);

    // Only the exact shape stays hidden: anything else is the agent's text.
    await expandFindings(viewer);
    await expect(viewer.getByText('finding id="H9"')).toBeVisible();
    await expect(viewer.getByText('finding severity=minor')).toHaveCount(0);

    dialogs.accept = false;
    await ui.publishInline(viewer).click();
    await expect.poll(() => dialogs.messages.length).toBe(1);
    expect(dialogs.messages[0]).toContain(`It carries ${pieces} piece`);
    expect(dialogs.messages[0]).toContain('Not everything in the markdown shows on screen');
  });
});

test.describe('P1 · a history_agent of your own', () => {
  const step = (extra: Record<string, unknown>) => ({
    name: 'history', task: '/history-pr {{number}}', command: 'fake-agent', args: [], format: 'plain',
    env: { FAKE_AGENT_SCENARIO: 'history-md' }, ...extra,
  });
  const generate = (bazel: Bazel) => bazel.api('POST', '/api/prs/acme/api-core/101/history');

  test.describe('that may publish', () => {
    test.use({ bazelOptions: { config: { agents: AGENTS, history_agent: step({ task: '/history-pr {{number}} --post' }) } } });

    test('does not stop the boot: the refusal comes on POST, and no agent runs', async ({ app, bazel }) => {
      const r = await generate(bazel);
      expect(r.status).toBe(409);
      expect(JSON.stringify(r.json)).toContain('history_agent has a task that publishes');
      expect(bazel.agentCalls()).toEqual([]);
    });
  });

  test.describe('with a prompt that does not forbid publishing', () => {
    test.use({ bazelOptions: { config: { agents: AGENTS, history_agent: step({ prompt: 'Tell the story of {{repo}}#{{number}}.' }) } } });

    test('is refused too', async ({ app, bazel }) => {
      const r = await generate(bazel);
      expect(r.status).toBe(409);
      expect(JSON.stringify(r.json)).toContain('do not publish anything to GitHub');
      expect(bazel.agentCalls()).toEqual([]);
    });
  });

  test.describe('that is merely custom', () => {
    test.use({ bazelOptions: { config: { agents: AGENTS, history_agent: step({}) } } });

    test('runs, and the History tab says Bazel cannot vouch for it', async ({ app, bazel }) => {
      expect((await bazel.api('GET', '/api/state')).json.history_custom).toBe(true);
      await reviewed(app, bazel, 'review-fleet');
      await app.getByRole('tab', { name: 'History' }).click();
      await expect(app.getByTestId('history-custom')).toContainText('cannot vouch');
    });
  });

  test('a PR outside the list needs offlist=1, on history as on details', async ({ bazel }) => {
    expect((await bazel.api('GET', '/api/prs/acme/api-core/999/history')).status).toBe(404);
    expect(bazel.ghCalls().filter((c) => c.argv.includes('999'))).toEqual([]);
  });
});

test.describe('P1 · a history job is not a review', () => {
  test.use({
    bazelOptions: {
      config: {
        agents: AGENTS,
        history_agent: { name: 'history', task: '/history-pr {{number}}', command: 'fake-agent', args: [], format: 'plain', env: { FAKE_AGENT_SCENARIO: 'history-md', FAKE_AGENT_GATE: 'release-history' } },
      },
    },
  });

  test('no "reviewing" tag in the list; the Activity says it is writing, then wrote, the history', async ({ app, bazel }) => {
    const q = await bazel.api('POST', '/api/prs/acme/api-core/101/history');
    await bazel.waitJob(q.json.id, (j) => j.state === 'running');
    await app.reload();
    const row = ui.prRow(app, TITLE);
    await row.click();
    await expect(app.getByTestId('pr-activity')).toContainText('is writing the history with');
    await expect(row.getByTestId('pr-review-tag')).toHaveCount(0);

    bazel.release('release-history');
    await bazel.waitJob(q.json.id, (j) => j.state === 'done');
    await app.reload();
    await row.click();
    await expect(app.getByTestId('pr-activity')).toContainText('wrote the history with');
    await expect(row.getByTestId('pr-review-tag')).toHaveCount(0);
  });

  // BUG-QA-07, fixed in #30: with the PR's own page open, a job of that PR
  // ending drew nothing, and the Activity kept saying "is writing…".
  test('the Activity of an open PR page follows the job to its end', async ({ app, bazel }) => {
    const q = await bazel.api('POST', '/api/prs/acme/api-core/101/history');
    await bazel.waitJob(q.json.id, (j) => j.state === 'running');
    await app.reload();
    await ui.prRow(app, TITLE).click();
    const activity = app.getByTestId('pr-activity');
    await expect(activity).toContainText('is writing the history with');
    bazel.release('release-history');
    await bazel.waitJob(q.json.id, (j) => j.state === 'done');
    await expect(activity).toContainText('wrote the history with', { timeout: 5_000 });
  });

  test('a running review does get the tag (the control for the test above)', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'slow-lens');
    await bazel.waitJob(id, (j) => j.state === 'running');
    await app.reload();
    await expect(ui.prRow(app, TITLE).getByTestId('pr-review-tag')).toHaveAttribute('data-state', 'reviewing');
    bazel.release('release-slow');
  });
});

test.describe('P2 · the list while it loads', () => {
  test('before /api/prs answers it says it is loading, never "no open PRs"', async ({ page, bazel }) => {
    let answer = () => {};
    const held = new Promise<void>((r) => { answer = r; });
    await page.route('**/api/prs*', async (route) => { await held; await route.continue(); });
    await page.goto(bazel.url + '/');
    await expect(page.getByText('no open PRs')).toHaveCount(0);
    await expect(page.getByTestId('pr-count')).not.toHaveText(/^0 PRs/);
    await page.waitForTimeout(300);
    await expect(page.getByText('no open PRs')).toHaveCount(0);
    answer();
    await expect(ui.prCount(page)).toHaveText('5 PRs');
  });
});
