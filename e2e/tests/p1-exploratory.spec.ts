import { Page } from '@playwright/test';
import { test, expect } from '../harness/fixtures';
import { Bazel } from '../harness/bazel';
import { SYNTH } from '../harness/paths';
import { expandFindings, jobTab, openJob, ui } from '../harness/ui';
import fs from 'node:fs';
import path from 'node:path';

// What the exploratory session on the new UI tried and found sound. Each of
// these was a real doubt: they stay so that it stays sound.
const T = 'Add rate limiter to the public API';
const plain = (name: string, scenario: string, extra: Record<string, unknown> = {}) =>
  ({ name, task: '/x {{number}}', command: 'fake-agent', args: [], format: 'plain', env: { FAKE_AGENT_SCENARIO: scenario }, ...extra });
const AGENTS = [
  { name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } },
  plain('order', 'order-report'), plain('hostile', 'hostile-report'), plain('history', 'history-plain', { publishable: false }),
  plain('huge', 'x-huge'), plain('ticker', 'x-ticker'),
];
const MARKS = ['E2E-O-MINOR', 'E2E-O-CRIT', 'E2E-O-MAJOR', 'E2E-O-CUT', 'E2E-O-HUMAN'];
const kept = (body: string) => MARKS.filter((m) => body.includes(m));
const lastComment = (bz: Bazel) => bz.ghCalls().filter((c) => c.argv[1] === 'comment' && c.exit === 0).at(-1)?.stdin ?? '';

async function report(app: Page, bazel: Bazel, agent = 'order') {
  const id = await bazel.enqueue('acme/api-core#101', agent);
  await bazel.waitJob(id, (j) => j.state === 'done');
  await app.reload();
  await expect(ui.prCount(app)).toHaveText(/PRs/);
  const v = await openJob(app, T);
  await jobTab(app, 'Report');
  return { id, v };
}

test.beforeAll(() => {
  fs.mkdirSync(SYNTH, { recursive: true });
  fs.writeFileSync(path.join(SYNTH, 'x-huge.txt'), '# Huge E2E-HUGE\n\n## Findings\n\n#!repeat 3000 ### F{i} · minor · Finding number {i}\n');
  fs.writeFileSync(path.join(SYNTH, 'x-ticker.txt'), `${Array.from({ length: 30 }, (_, i) => `#!repeat 20 tick ${i}-{i}\n#!sleep 80\n`).join('')}#!wait release-ticker\n# Done E2E-TICKER\n`);
});

test.describe('P1 · the tick on screen is the finding that stays out', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS, pipelines: [{ name: 'with-history', steps: ['history', 'order', 'publish'] }] } } });

  test('findings grouped by severity keep their server index', async ({ app, bazel }) => {
    const { id, v } = await report(app, bazel);
    await ui.findingPick(v, 'Second in the file').uncheck(); // the critical one, shown first
    await ui.findingPick(v, 'Cut one').uncheck();
    await expect(app.getByTestId('pick-summary')).toContainText('3 of 5 findings');
    await ui.pasteAsComment(v).click();
    await bazel.waitJob(id, (j) => j.posted);
    expect(kept(lastComment(bazel))).toEqual(['E2E-O-MINOR', 'E2E-O-MAJOR', 'E2E-O-HUMAN']);
  });

  test('a saved review maps the same way', async ({ app, bazel }) => {
    await report(app, bazel);
    await ui.savedTab(app).click();
    await app.getByTestId('saved-row').first().click();
    const v = ui.viewer(app);
    await ui.findingPick(v, 'Third in the file').uncheck();
    await app.getByTestId('saved-comment').click();
    await expect.poll(() => kept(lastComment(bazel))).toEqual(['E2E-O-MINOR', 'E2E-O-CRIT', 'E2E-O-CUT', 'E2E-O-HUMAN']);
  });

  test('a pipeline with a step that stays in Bazel counts only what goes', async ({ app, bazel }) => {
    const { id, v } = await report(app, bazel, 'with-history');
    await ui.findingPick(v, 'First in the file').uncheck();
    await ui.pasteAsComment(v).click();
    await bazel.waitJob(id, (j) => j.posted);
    expect(kept(lastComment(bazel))).toEqual(['E2E-O-CRIT', 'E2E-O-MAJOR', 'E2E-O-CUT', 'E2E-O-HUMAN']);
    expect(lastComment(bazel)).not.toContain('E2E-HISTORY');
  });

  test('ticks survive tabs, pages, other jobs and a failed publish', async ({ app, bazel }) => {
    const { id, v } = await report(app, bazel);
    const pick = ui.findingPick(v, 'Second in the file');
    await pick.uncheck();
    await jobTab(app, 'Overview');
    await jobTab(app, 'Report');
    await expect(pick).not.toBeChecked();
    await app.getByTestId('nav-config').click();
    await app.getByTestId('nav-dashboard').click();
    await expect(pick).not.toBeChecked();
    const other = await bazel.enqueue('acme/web-app#7', 'review-fleet');
    await bazel.waitJob(other, (j) => j.state === 'done');
    await expect(pick).not.toBeChecked();

    bazel.setGh({ fail: { comment: 'HTTP 403: nope' } });
    await ui.pasteAsComment(v).click();
    await bazel.waitJob(id, (j) => !!j.post_error);
    await expect(pick).not.toBeChecked();
    bazel.setGh({});
    await ui.pasteAsComment(v).click();
    await bazel.waitJob(id, (j) => j.posted);
    expect(kept(lastComment(bazel))).not.toContain('E2E-O-CRIT');
  });
});

test.describe('P1 · what a PR or an agent writes never runs in the page', () => {
  const XSS = `<img src=x onerror="window.__xss='field'">`;
  test.use({ bazelOptions: { config: { agents: [...AGENTS, plain(`<b onmouseover="window.__xss='agent'">agent</b>`, 'plain-report')] } } });

  test('title, author, commits, reviewers, labels, checks, report, log and saved', async ({ app, bazel }) => {
    bazel.updatePR('acme/api-core#101', {
      title: `${XSS} ${T}`, author: { login: `<i>${XSS}</i>` }, body: `${XSS} <script>window.__xss='body'</script>`,
      commits: [{ oid: 'a<b>', messageHeadline: XSS, committedDate: '2026-10-21T12:00:00Z', authors: [{ login: XSS }] }],
      reviewRequests: [{ login: XSS }], latestReviews: [{ author: { login: XSS }, state: 'APPROVED' }],
      labels: [{ name: XSS }], statusCheckRollup: [{ name: XSS, status: 'COMPLETED', conclusion: 'SUCCESS' }],
    });
    await bazel.api('GET', '/api/prs?refresh=1');
    const id = await bazel.enqueue('acme/api-core#101', 'hostile');
    await bazel.waitJob(id, (j) => j.state === 'done');
    await app.reload();
    await expect(ui.prCount(app)).toHaveText('5 PRs');
    await app.getByTestId('job-card').first().click();
    for (const tab of ['Report', 'Live log', 'History', 'Overview'] as const) await jobTab(app, tab);
    await jobTab(app, 'Report');
    await expandFindings(ui.viewer(app));
    await ui.savedTab(app).click();
    await app.getByTestId('saved-row').first().click();
    await app.getByTestId('nav-config').click();
    await expect(app.getByRole('heading', { name: 'Agents', exact: true })).toBeVisible();
    expect(await app.evaluate(() => (window as any).__xss)).toBeUndefined();
    expect(await app.locator('img[src="x"], [onerror], svg[onload], a[href^="javascript"]').count()).toBe(0);
  });
});

test.describe('P2 · a lot of everything', () => {
  test.use({ bazelOptions: { flags: ['--jobs', '6'], config: { agents: AGENTS } } });

  test('3000 findings open, tick and all/none in time a person would accept', async ({ app, bazel }) => {
    test.setTimeout(90_000);
    const { v } = await report(app, bazel, 'huge');
    await expect(ui.findingPicks(v)).toHaveCount(3000, { timeout: 30_000 });
    await ui.findingPicks(v).nth(5).uncheck();
    await expect(app.getByTestId('pick-summary')).toContainText('2999 of 3000');
    await app.getByTestId('finding-pick-none').click();
    await expect(app.getByTestId('pick-summary')).toContainText('0 of 3000');
  });

  test('dozens of jobs: the queue keeps its place when one more arrives', async ({ app, bazel }) => {
    test.setTimeout(90_000);
    for (let i = 0; i < 30; i++) bazel.addPR({ repo: 'acme/api-core', number: 300 + i, title: `PR ${i}`, headRefName: `p${i}`, headRefOid: `q${i}`, author: { login: 'x' } });
    await bazel.api('GET', '/api/prs?refresh=1');
    await app.reload();
    await ui.selectAll(app).check();
    await ui.runButton(app).click();
    await expect.poll(async () => (await bazel.api('GET', '/api/jobs')).json.jobs.filter((j: any) => j.state === 'done').length, { timeout: 60_000 }).toBe(35);
    const rail = app.locator('#rail');
    await rail.evaluate((r) => { r.scrollLeft = 1500; });
    await bazel.enqueue('acme/api-core#330', 'review-fleet').catch(() => {});
    await bazel.enqueue('acme/web-app#7', 'order');
    await expect(app.getByTestId('job-card')).toHaveCount(36);
    expect(await rail.evaluate((r) => r.scrollLeft)).toBe(1500);
  });
});

test.describe('P1 · moving around', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  test('a reload in the middle of a job finds it running, and its log goes on', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'ticker');
    await bazel.waitJob(id, (j) => j.state === 'running');
    await app.reload();
    await openJob(app, T);
    await jobTab(app, 'Live log');
    await expect(app.getByTestId('log-line').first()).toBeVisible();
    await app.reload();
    await expect(app.getByTestId('job-card').first()).toHaveAttribute('data-state', 'running');
    await openJob(app, T);
    await jobTab(app, 'Live log');
    await expect(app.getByTestId('log-line').first()).toBeVisible();
    bazel.release('release-ticker');
    await bazel.waitJob(id, (j) => j.state === 'done');
    await expect(app.getByTestId('job-card').first()).toHaveAttribute('data-state', 'done');
  });

  test('back and forward between dashboard and config keep the job and its tab', async ({ app, bazel }) => {
    await report(app, bazel, 'review-fleet');
    await jobTab(app, 'Live log');
    await app.getByTestId('nav-config').click();
    await expect(app).toHaveURL(/#\/config$/);
    await app.goBack();
    await expect(app.getByRole('tab', { name: 'Live log' })).toHaveAttribute('aria-selected', 'true');
    await app.goForward();
    await expect(app.getByTestId('repo-input')).toBeVisible();
  });

  test('two tabs follow the same queue, and a publish from one reaches the other', async ({ app, bazel, context }) => {
    const other = await context.newPage();
    other.on('dialog', (d) => d.accept());
    await other.goto(`${bazel.url}/`);
    await expect(ui.prCount(other)).toHaveText('5 PRs');
    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    await expect(other.getByTestId('job-card').first()).toHaveAttribute('data-state', 'done');

    const here = await openJob(app, T);
    const there = await openJob(other, T);
    await jobTab(app, 'Report');
    await jobTab(other, 'Report');
    await ui.pasteAsComment(here).click();
    await bazel.waitJob(id, (j) => j.posted);
    await expect(ui.alreadyPublished(there)).toBeVisible();
    await expect(ui.pasteAsComment(there)).toHaveCount(0);

    await ui.removeButton(ui.jobCard(app, T)).click();
    await expect(other.getByTestId('job-card')).toHaveCount(0);
  });

  test('a page that loses the server recovers the moment you retry', async ({ app, bazel }) => {
    const { id, v } = await report(app, bazel, 'review-fleet');
    bazel.setGh({ fail: { comment: 'HTTP 502' } });
    await ui.pasteAsComment(v).click();
    await bazel.waitJob(id, (j) => !!j.post_error);
    await expect(ui.banner(app)).toContainText('HTTP 502');
    bazel.setGh({});
    await ui.pasteAsComment(v).click();
    await bazel.waitJob(id, (j) => j.posted);
    await expect(ui.banner(app)).not.toContainText('HTTP 502');
  });
});

test.describe('P2 · layout holds', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });
  for (const [width, height] of [[1440, 900], [1000, 800], [800, 700], [1280, 420]]) {
    test(`no sideways scroll on the page at ${width}x${height}`, async ({ app }) => {
      await app.setViewportSize({ width, height });
      await expect(ui.runButton(app)).toBeInViewport();
      expect(await app.evaluate(() => document.scrollingElement!.scrollWidth - document.scrollingElement!.clientWidth)).toBeLessThanOrEqual(0);
    });
  }
});
