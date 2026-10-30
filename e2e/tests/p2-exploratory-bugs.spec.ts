import { Page } from '@playwright/test';
import { test, expect, knownBug } from '../harness/fixtures';
import { Bazel } from '../harness/bazel';
import { jobTab, openJob, ui } from '../harness/ui';

// Open bugs from the exploratory session on the new UI (master 9c3cb2d).
// Steps, expected and actual of each are in the comment over its test.
const T = 'Add rate limiter to the public API';
const plain = (name: string, scenario: string) =>
  ({ name, task: '/x {{number}}', command: 'fake-agent', args: [], format: 'plain', env: { FAKE_AGENT_SCENARIO: scenario } });
const AGENTS = [
  { name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } },
  { name: 'slow-lens', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-slow' } },
  plain('long', 'long-report'), plain('order', 'order-report'),
];
const comments = (bz: Bazel) => bz.ghCalls().filter((c) => c.argv[1] === 'comment');

async function report(app: Page, bazel: Bazel, agent = 'review-fleet') {
  const id = await bazel.enqueue('acme/api-core#101', agent);
  await bazel.waitJob(id, (j) => j.state === 'done');
  await app.reload();
  await expect(ui.prCount(app)).toHaveText(/PRs/);
  const v = await openJob(app, T);
  await jobTab(app, 'Report');
  return { id, v };
}

test.describe('P2 · bugs', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  // BUG-QA-10: the server restarts while you read a review, with another job
  // running. adoptInstance empties the queue and the viewer: a blank panel, not
  // even the placeholder, no word about the restart, the running job gone and
  // the ticks lost. Expected: say what happened and where the review is (Saved).
  test('a restart says what happened to what was on screen', async ({ app, bazel }) => {
    knownBug('BUG-QA-10', 'a restart empties the screen without a word');
    const done = await bazel.enqueue('acme/web-app#7', 'review-fleet');
    await bazel.waitJob(done, (j) => j.state === 'done');
    const running = await bazel.enqueue('acme/api-core#101', 'slow-lens');
    await bazel.waitJob(running, (j) => j.state === 'running');
    await app.reload();
    await openJob(app, 'Dark mode toggle in settings');
    await jobTab(app, 'Report');
    await bazel.restart({ samePort: true, downMs: 2_000 });
    await expect(app.getByTestId('job-card')).toHaveCount(0, { timeout: 15_000 });
    const screen = `${await ui.banner(app).textContent()} ${await ui.viewer(app).textContent()}`;
    expect(screen).toMatch(/restart/i);
  });

  // BUG-QA-11: below 900px the list stacks on top, and with the queue and the
  // job's head above it Report, Live log and History get 61px, 52 of them the
  // sticky action bar: 9px to read. 1280x420 gets 112px. Collapsing the list
  // with ☰ gives 329px, but nothing says so. Expected: at least 200px to read
  // (a judgement call): the list collapses by itself on opening a job, or the
  // job takes the page.
  test.describe('in a small window', () => {
    // One repo: with two, the banner of BUG-QA-26 takes height too.
    test.use({ bazelOptions: { config: { agents: AGENTS, repos: ['acme/api-core'] } } });
    for (const [width, height] of [[800, 700], [1280, 420]]) {
      test(`a job has room to read at ${width}x${height}`, async ({ app, bazel }) => {
        knownBug('BUG-QA-11', 'no room left for the report in a small window');
        await app.setViewportSize({ width, height });
        await report(app, bazel, 'long');
        const room = (await ui.jobBody(app).evaluate((e) => e.clientHeight)) - (await ui.actionBar(app).evaluate((e) => e.offsetHeight));
        expect(room).toBeGreaterThanOrEqual(200);
      });
    }
  });

  // BUG-QA-12: the PR gets a commit after the review. The Overview warns, the
  // Report and the publish confirm do not: you publish line comments anchored to
  // a head that is gone. Expected: the warning where the decision is made.
  test('publishing a review of an older head says so', async ({ app, bazel, dialogs }) => {
    knownBug('BUG-QA-12', 'no warning on publishing over a moved head');
    const { v } = await report(app, bazel);
    bazel.movePR('acme/api-core#101');
    await bazel.api('GET', '/api/prs?refresh=1');
    await app.reload();
    await expect(ui.prCount(app)).toHaveText(/PRs/);
    await openJob(app, T);
    await jobTab(app, 'Report');
    dialogs.accept = false;
    await ui.publishInline(v).click();
    await expect.poll(() => dialogs.messages.length).toBe(1);
    expect(dialogs.messages[0]).toMatch(/changed|new commit/i);
  });

  // BUG-QA-13: untick a false positive, reload: the tick is back, silently, and
  // the review goes out whole. The ticks live in the page's memory. Expected:
  // what you took out stays out for that review, or the page tells you.
  test('an unticked finding stays unticked after a reload', async ({ app, bazel }) => {
    knownBug('BUG-QA-13', 'ticks do not survive a reload');
    const { v } = await report(app, bazel, 'order');
    await ui.findingPick(v, 'Second in the file').uncheck();
    await app.reload();
    await expect(ui.prCount(app)).toHaveText(/PRs/);
    await openJob(app, T);
    await jobTab(app, 'Report');
    await expect(ui.findingPick(v, 'Second in the file')).not.toBeChecked();
  });

  // BUG-QA-14: with every finding unticked, "As comment" posts the verdict with
  // an empty `## Findings`: "request changes", with nothing to change, on
  // someone's PR. Expected: nothing to send, so the buttons say so.
  test('with no finding ticked there is nothing to publish', async ({ app, bazel }) => {
    knownBug('BUG-QA-14', 'zero findings still publishes the verdict');
    const { v } = await report(app, bazel);
    await app.getByTestId('finding-pick-none').click();
    await expect(ui.pasteAsComment(v)).toBeDisabled();
  });

  // BUG-QA-15: one long unbroken token (a path, a hash) makes the whole Report
  // scroll sideways, the action bar with it: "2 of 2 findings" goes off the left
  // edge. Expected: the token wraps, or only its own block scrolls.
  test('a long token does not move the Report sideways', async ({ app, bazel }) => {
    knownBug('BUG-QA-15', 'long tokens widen the report');
    await report(app, bazel, 'long');
    const { sw, cw } = await ui.jobBody(app).evaluate((e) => ({ sw: e.scrollWidth, cw: e.clientWidth }));
    expect(sw).toBeLessThanOrEqual(cw);
  });

  // BUG-QA-17: focus falls to <body> when the control you used goes away: after
  // Review and after "As comment". The next Tab starts from the top of the page.
  // Expected: focus lands on the new job, or stays near the action.
  test('focus survives Review and a comment', async ({ app, bazel }) => {
    knownBug('BUG-QA-17', 'focus is lost after an action');
    await ui.filter(app).fill('rate');
    await ui.selectAll(app).check();
    await ui.runButton(app).focus();
    await app.keyboard.press('Enter');
    await expect(app.getByTestId('job-card')).toHaveCount(1);
    expect(await app.evaluate(() => document.activeElement?.tagName)).not.toBe('BODY');
  });

  // BUG-QA-18: Enter opens the filter popover and focus stays on its button, but
  // Escape is only heard inside the popover: pressing it does nothing.
  test('Escape closes the filter popover with focus on its button', async ({ app }) => {
    knownBug('BUG-QA-18', 'Escape only works inside the popover');
    const menu = app.getByTestId('pr-filter-menu');
    await menu.focus();
    await app.keyboard.press('Enter');
    await expect(menu).toHaveAttribute('aria-expanded', 'true');
    await app.keyboard.press('Escape');
    await expect(menu).toHaveAttribute('aria-expanded', 'false');
  });

  // BUG-QA-19: role=tab without the keyboard contract: arrows do nothing, and
  // every tab is a Tab stop. Expected: ArrowRight/Left move between the tabs.
  test('arrow keys move between the tabs of a job', async ({ app, bazel }) => {
    knownBug('BUG-QA-19', 'tabs ignore the arrow keys');
    await report(app, bazel);
    await app.getByRole('tab', { name: 'Report' }).focus();
    await app.keyboard.press('ArrowLeft');
    await expect(app.getByRole('tab', { name: 'Live log' })).toBeFocused();
  });

  // BUG-QA-20: Arabic titles render left to right: the ellipsis eats the start of
  // the sentence and punctuation lands on the wrong side. Expected: dir="auto".
  test('a right-to-left title reads right to left', async ({ app, bazel }) => {
    knownBug('BUG-QA-20', 'titles have no dir=auto');
    bazel.updatePR('acme/api-core#101', { title: 'تحديث واجهة المستخدم للنسخة العربية' });
    await bazel.api('GET', '/api/prs?refresh=1');
    await app.reload();
    await expect(ui.prRow(app, 'تحديث واجهة المستخدم للنسخة العربية').getByText('تحديث')).toHaveCSS('direction', 'rtl');
  });

  // BUG-QA-24: after a review is on the PR the Publish button stays, enabled, and
  // only answers "Nothing new was sent". Expected: it goes, like "As comment".
  test('Publish goes away once the review is on the PR', async ({ app, bazel }) => {
    knownBug('BUG-QA-24', 'Publish stays after publishing');
    const { id, v } = await report(app, bazel);
    await ui.pasteAsComment(v).click();
    await bazel.waitJob(id, (j) => j.posted);
    await expect(ui.alreadyPublished(v)).toBeVisible();
    await expect(ui.publishInline(v)).toHaveCount(0);
    expect(comments(bazel)).toHaveLength(1);
  });
});

test.describe('P3 · pipeline builder', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  // BUG-QA-25: a dragged step only lands on the gaps between cards, 4px wide.
  // Dropped on a card, where the eye goes, nothing happens and nothing says why.
  // There is also no keyboard way to reorder. Expected: a drop on a card lands
  // before it (or after, by the half), and the steps can be moved from the keyboard.
  test('a step dropped on a card lands there', async ({ app }) => {
    knownBug('BUG-QA-25', 'the drop target is 4px wide');
    await app.getByTestId('nav-config').click();
    const tray = app.getByTestId('pipeline-tray');
    await tray.getByRole('button', { name: '+ review-fleet' }).click();
    await tray.getByRole('button', { name: '+ plain' }).or(tray.getByRole('button', { name: '+ long' })).first().click();
    const chain = app.getByTestId('pipeline-step');
    await chain.nth(1).dragTo(chain.nth(0), { targetPosition: { x: 4, y: 4 } });
    await expect(chain.nth(0)).not.toHaveAttribute('data-step', 'review-fleet');
  });
});

test.describe('P3 · two owners, one repo name', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS, repos: ['acme/api-core', 'globex/api-core'] } } });

  // BUG-QA-21: acme/api-core#101 and globex/api-core#101 are both "api-core #101"
  // on the row and on the card: only the title tells them apart, and that is the
  // card you publish from. Expected: the owner shows when the name is ambiguous.
  test('cards of same-named repos can be told apart', async ({ bazel, app }) => {
    knownBug('BUG-QA-21', 'cards drop the owner');
    bazel.addPR({ repo: 'globex/api-core', number: 101, title: 'Other owner', headRefName: 'x', headRefOid: 'x', author: { login: 'z' } });
    await bazel.api('GET', '/api/prs?refresh=1');
    for (const ref of ['acme/api-core#101', 'globex/api-core#101']) await bazel.enqueue(ref, 'review-fleet');
    await expect(app.getByTestId('job-card')).toHaveCount(2);
    const heads = await app.getByTestId('job-card').evaluateAll((cards) => cards.map((c) => c.textContent!.split('✕')[0]));
    expect(new Set(heads).size).toBe(2);
  });
});

test.describe('P3 · every repository failing', () => {
  test.use({ bazelOptions: { gh: { fail: { search: 'HTTP 502', 'list:acme/api-core': 'HTTP 502: down', 'list:acme/web-app': 'HTTP 502: down' } } } });

  // BUG-QA-22: nothing could be read and the list says "no open PRs", which is a
  // claim about GitHub. The banner also opens with a bare "⚠ :". Expected: the
  // list says it could not read them.
  test('a list that could not be read does not say there are no PRs', async ({ page, bazel }) => {
    knownBug('BUG-QA-22', '"no open PRs" when every read failed');
    await page.goto(`${bazel.url}/`);
    await expect(ui.banner(page)).toContainText('HTTP 502');
    await expect(page.locator('#pr-list')).not.toContainText('no open PRs');
  });
});

test.describe('P3 · words', () => {
  // BUG-QA-23: the one string in Portuguese in an English UI (app.js:277).
  test('the list loads in English', async ({ page, bazel }) => {
    knownBug('BUG-QA-23', 'one string left in Portuguese');
    let answer = () => {};
    const held = new Promise<void>((r) => { answer = r; });
    await page.route('**/api/prs*', async (route) => { await held; await route.continue(); });
    await page.goto(`${bazel.url}/`);
    await expect(page.locator('#pr-list')).toContainText('GitHub');
    await expect(page.locator('#pr-list')).not.toContainText('consultando');
    answer();
  });
});
