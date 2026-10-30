import { test, expect, knownBug } from '../harness/fixtures';
import { Bazel } from '../harness/bazel';
import { expandFindings, openJob, ui } from '../harness/ui';
import { Page } from '@playwright/test';

// Behaviours of feat/redesign that have no master counterpart. Bases without
// the redesign skip them (no testids in the page).
const TITLE = 'Add rate limiter to the public API';
const redesignOnly = async (page: Page) =>
  test.skip((await page.getByTestId('review-viewer').count()) === 0, 'this base is not the redesign');

const plain = (name: string, scenario: string, extra: Record<string, unknown> = {}) =>
  ({ name, task: '/exploit-digger {{number}}', command: 'fake-agent', args: [], format: 'plain', env: { FAKE_AGENT_SCENARIO: scenario }, ...extra });
const AGENTS = [
  { name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } },
  { name: 'slow-lens', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-slow' } },
  plain('sections', 'sections-report'),
  plain('sections-stay', 'sections-report', { publishable: false }),
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
    await redesignOnly(app);
    bazel.setGh({ fail: { 'view:acme/api-core#101': 'HTTP 502: E2E-DETAILS-DOWN' } });
    await ui.prRow(app, TITLE).click();
    const error = app.getByTestId('pr-error');
    await expect(error).toContainText('E2E-DETAILS-DOWN');
    await app.waitForTimeout(1_200); // a routine refresh must not wipe it
    await expect(error).toBeVisible();
    await expect(error.getByTestId('retry')).toBeVisible();
  });

  // BUG-QA-08: the server keeps a failed read for detailsErrTTL (30s, "not
  // retried, whoever asks"), and the Retry button goes through the same door:
  // GitHub is back, you click Retry, and for up to 30s you get the old error
  // again, with nothing saying why. Expected: Retry reads again, or says when
  // it can. Reported to the Bazel Developer.
  test('Retry reads again once GitHub is back', async ({ app, bazel }) => {
    knownBug('BUG-QA-08', 'Retry answers the cached failure for 30s');
    await redesignOnly(app);
    bazel.setGh({ fail: { 'view:acme/api-core#101': 'HTTP 502: E2E-DETAILS-DOWN' } });
    await ui.prRow(app, TITLE).click();
    await expect(app.getByTestId('pr-error')).toBeVisible();
    bazel.setGh({});
    await app.getByTestId('retry').click();
    await expect(app.getByTestId('pr-page')).toHaveAttribute('data-loaded', 'true', { timeout: 5_000 });
    await expect(app.getByTestId('pr-error')).toHaveCount(0);
  });

  test('a markdown image becomes a link in the PR body and the report, and is never fetched', async ({ app, bazel }) => {
    await redesignOnly(app);
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
    await redesignOnly(app);
    const { viewer } = await reviewed(app, bazel, 'review-fleet');
    await app.getByRole('tab', { name: 'Report' }).click();
    await ui.findingPicks(viewer).nth(1).uncheck();
    dialogs.accept = false;
    await ui.publishInline(viewer).click();
    await expect.poll(() => dialogs.messages.length).toBe(1);
    expect(dialogs.messages[0]).toContain('Goes to the PR: 1 of 2 findings (whole bodies)');
    expect(dialogs.messages[0]).toContain('1 unticked finding will be left out.');
    expect(bazel.agentCalls()).toHaveLength(1);
  });

  test('the rest of the report is open and flagged when it goes to the PR', async ({ app, bazel, dialogs }) => {
    await redesignOnly(app);
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
    await redesignOnly(app);
    const { viewer } = await reviewed(app, bazel, 'sections-stay');
    await app.getByRole('tab', { name: 'Report' }).click();
    const more = viewer.getByTestId('review-more');
    await expect(more).toHaveAttribute('data-publishes', 'false');
    await expect(more).toHaveJSProperty('open', false);
    await expect(more).not.toContainText('also goes to the PR');
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
    await redesignOnly(app);
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

  // BUG-QA-07: upsertJob only redraws when the job is the open one
  // (state.activeJob). With the PR's own page open (openPR: activePR, no
  // activeJob), a job of that PR ending clears state.details and draws nothing:
  // the Activity keeps saying "is writing…" / "is reviewing with" until you
  // leave and come back. Expected: the open page follows the job.
  test('the Activity of an open PR page follows the job to its end', async ({ app, bazel }) => {
    knownBug('BUG-QA-07', 'open PR page does not redraw when a job of the PR ends');
    await redesignOnly(app);
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
    await redesignOnly(app);
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
    await redesignOnly(page);
    await expect(page.getByText('no open PRs')).toHaveCount(0);
    await expect(page.getByTestId('pr-count')).not.toHaveText(/^0 PRs/);
    await page.waitForTimeout(300);
    await expect(page.getByText('no open PRs')).toHaveCount(0);
    answer();
    await expect(ui.prCount(page)).toHaveText('5 PRs');
  });
});
