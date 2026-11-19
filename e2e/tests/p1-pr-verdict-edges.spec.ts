import fs from 'node:fs';
import { test, expect } from '../harness/fixtures';
import { Bazel } from '../harness/bazel';
import { openJob, ui } from '../harness/ui';

// feat/pr-verdict, beyond p1-pr-verdict: the edges of a decision and of the
// queue's Summary. The decision is local: no test here may see a gh write.
const AGENTS = [{ name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } }];
const RATE = 'Add rate limiter to the public API';
const K = 'acme/api-core#101';
const url = (key: string) => `https://github.com/${key.replace('#', '/pull/')}`;
// The fixture's #101 asks bob and has carol's approval: its line names them.
const WHO: Record<string, string> = { 'acme/api-core#101': ' — bob, carol' };
const line = (key: string, emoji: string) => `${url(key)} ${emoji}${WHO[key] ?? ''}`;

const writes = (bz: Bazel) => bz.ghCalls().filter((c) => (c.argv[0] === 'pr' && c.argv[1] === 'comment')
  || (c.argv[0] === 'api' && c.argv.some((a) => a === 'POST' || a === '-X' || a === '--method')));
async function done(bazel: Bazel, ref: string) {
  const id = await bazel.enqueue(ref, 'review-fleet');
  await bazel.waitJob(id, (j) => j.state === 'done');
  return id;
}
const verdicts = async (bz: Bazel) => (await bz.api('GET', '/api/verdicts')).json.verdicts;
const summary = async (bz: Bazel) => (await bz.api('GET', '/api/summary')).json;
const decide = async (bz: Bazel, id: string, verdict: string) => expect((await bz.api('POST', `/api/jobs/${id}/verdict`, { verdict })).status).toBe(200);
const pressed = (scope: any) => scope.locator('[data-testid^=verdict-][aria-pressed=true]').evaluateAll((b: Element[]) => b.map((x) => (x as HTMLElement).dataset.verdict));

test.describe('P1 · your decision, edges', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });
  test.afterEach(async ({ bazel }) => { expect(writes(bazel)).toEqual([]); });

  test('two jobs of one PR share one decision, on both cards and once in the summary', async ({ app, bazel }) => {
    const a = await done(bazel, K);
    await done(bazel, K);
    await app.reload();
    const cards = app.locator(`[data-testid=job-card][data-pr-key="${K}"]`);
    await expect(cards).toHaveCount(2);
    await decide(bazel, a, 'approve');
    await expect(cards.getByTestId('job-verdict')).toHaveText(['✅', '✅']);
    expect((await summary(bazel)).text).toBe(line(K, ':white_check_mark:'));
  });

  test('a decision from Saved, on a PR no longer in the queue, sticks, and the summary of an empty queue is empty', async ({ bazel }) => {
    await done(bazel, K);
    await bazel.restart(); // the finished job goes; the saved file stays
    const [saved] = (await bazel.api('GET', '/api/reviews')).json.reviews;
    const r = await bazel.api('POST', `/api/reviews/${saved.name}/verdict`, { verdict: 'approve' });
    expect(r.status).toBe(200);
    expect(r.json.verdict.head).toMatch(/^[0-9a-f]{40}$/); // the head the file says it read
    expect((await verdicts(bazel))[K].verdict).toBe('approve');
    expect(await summary(bazel)).toEqual({ text: '', decided: [], undecided: [], moved: [] });
  });

  test('fast clicks end on the last one, on the page and on the server', async ({ app, bazel }) => {
    await done(bazel, K);
    await app.reload();
    const v = await openJob(app, RATE);
    for (const id of ['approve', 'request_changes', 'approve_with_comments'] as const) await ui.verdict(v, id).click();
    await expect.poll(async () => (await verdicts(bazel))[K]?.verdict).toBe('approve_with_comments');
    await expect.poll(() => pressed(v)).toEqual(['approve_with_comments']);
    await expect(ui.jobVerdict(ui.jobCard(app, RATE))).toHaveText('📝✅');
  });

  test('two tabs follow each other\'s decision', async ({ app, bazel, context }) => {
    await done(bazel, K);
    await app.reload();
    const other = await context.newPage();
    await other.goto(bazel.url + '/');
    const v1 = await openJob(app, RATE);
    const v2 = await openJob(other, RATE);
    await ui.verdict(v1, 'approve').click();
    await expect(ui.verdict(v2, 'approve')).toHaveAttribute('aria-pressed', 'true');
    await ui.verdict(v2, 'request_changes').click();
    await expect(ui.verdict(v1, 'request_changes')).toHaveAttribute('aria-pressed', 'true');
  });

  test('a restart in the middle keeps the decision; the queue it summed up is gone', async ({ app, bazel }) => {
    await done(bazel, K);
    await app.reload();
    const v = await openJob(app, RATE);
    await ui.verdict(v, 'request_changes').click();
    await expect(ui.verdict(v, 'request_changes')).toHaveAttribute('aria-pressed', 'true');
    await bazel.restart({ samePort: true, downMs: 1_000 });
    expect((await verdicts(bazel))[K].verdict).toBe('request_changes');
    expect((await summary(bazel)).decided).toEqual([]);
    await expect(app.getByTestId('job-card')).toHaveCount(0, { timeout: 30_000 });
  });

  test('moved, then reviewed again: the bar says it moved, and deciding on the new review clears that', async ({ app, bazel }) => {
    const a = await done(bazel, K);
    await decide(bazel, a, 'approve');
    const old = (await verdicts(bazel))[K].head;
    bazel.movePR(K);
    await bazel.api('GET', '/api/prs?refresh=1');
    expect((await verdicts(bazel))[K].moved).toBe(true);
    const b = await done(bazel, K);
    await app.reload();
    const v = await openJob(app, RATE);
    await expect(ui.verdictMoved(v)).toHaveText(`decided on ${old.slice(0, 7)}, PR moved`);
    await decide(bazel, b, 'approve');
    const now = (await verdicts(bazel))[K];
    expect(now.head).not.toBe(old);
    expect(now.moved ?? false).toBe(false);
    await expect(ui.verdictMoved(v)).toBeHidden();
  });

  // BUG-QA-38, fixed in feat/pr-verdict: the text approved a PR that moved after
  // the decision. Now a moved PR leaves the text, is named apart, and comes back
  // with a decision on its new review.
  test('a PR that moved since the decision leaves the text, is named apart, and comes back with a new decision', async ({ app, bazel }) => {
    const a = await done(bazel, K);
    const w = await done(bazel, 'acme/web-app#7');
    await decide(bazel, a, 'approve');
    await decide(bazel, w, 'request_changes');
    bazel.movePR(K);
    await bazel.api('GET', '/api/prs?refresh=1');
    const s1 = await summary(bazel);
    expect(s1.text).toBe(line('acme/web-app#7', ':git_change_request:'));
    expect(s1.moved.map((p: any) => p.key)).toEqual([K]);
    await app.reload();
    await ui.summaryButton(app).click();
    await expect(ui.summaryText(app)).toHaveValue(line('acme/web-app#7', ':git_change_request:'));
    await expect(app.getByTestId('summary-moved')).toContainText('moved since your decision: #101');
    await app.keyboard.press('Escape');

    const b = await done(bazel, K);
    await decide(bazel, b, 'approve');
    const s2 = await summary(bazel);
    expect(s2.moved).toEqual([]);
    expect(s2.text).toBe([line(K, ':white_check_mark:'), line('acme/web-app#7', ':git_change_request:')].join('\n'));
    await ui.summaryButton(app).click();
    await expect(app.getByTestId('summary-moved')).toBeHidden();
  });

  test('a queue with no decision: an empty text that says why, and the PRs listed apart', async ({ app, bazel }) => {
    await done(bazel, K);
    await done(bazel, 'acme/web-app#7');
    await app.reload();
    await ui.summaryButton(app).click();
    await expect(ui.summaryText(app)).toHaveValue('');
    await expect(ui.summaryText(app)).toHaveAttribute('placeholder', "no decision on the queue's PRs yet");
    await expect(ui.summaryMissing(app)).toHaveText('no decision yet: #7, #101');
  });

  test('repos apart: each PR is its own line, by its own URL', async ({ bazel }) => {
    bazel.addPR({ repo: 'globex/api-core', number: 101, title: 'Other owner, same name', headRefName: 'g', author: { login: 'zoe' } });
    const a = await done(bazel, K);
    const g = await done(bazel, 'globex/api-core#101');
    const w = await done(bazel, 'acme/web-app#7');
    await decide(bazel, a, 'approve');
    await decide(bazel, g, 'request_changes');
    await decide(bazel, w, 'approve_with_comments');
    expect((await summary(bazel)).text).toBe([
      line('acme/web-app#7', ':memo::white_check_mark:'),
      line('globex/api-core#101', ':git_change_request:'),
      line(K, ':white_check_mark:'),
    ].join('\n'));
  });

  // BUG-QA-39, fixed in feat/pr-verdict: two undecided PRs with one number both
  // read "#101". Now a shared number names its repo.
  test('the undecided list tells two #101 of different repos apart', async ({ app, bazel }) => {
    bazel.addPR({ repo: 'globex/api-core', number: 101, title: 'Other owner, same name', headRefName: 'g', author: { login: 'zoe' } });
    await done(bazel, K);
    await done(bazel, 'globex/api-core#101');
    await app.reload();
    await ui.summaryButton(app).click();
    await expect(ui.summaryMissing(app)).toContainText('globex');
    await expect(ui.summaryMissing(app)).toContainText('acme');
  });

  test('the API takes only the three decisions or "", and only for a review there is', async ({ bazel }) => {
    const id = await done(bazel, K);
    for (const body of [{}, { verdict: 'merge' }, { verdict: 'APPROVE' }, { verdict: null }]) {
      expect((await bazel.api('POST', `/api/jobs/${id}/verdict`, body)).status, JSON.stringify(body)).toBe(400);
    }
    expect((await bazel.api('POST', '/api/jobs/nope/verdict', { verdict: 'approve' })).status).toBe(404);
    expect((await bazel.api('POST', '/api/reviews/nope.md/verdict', { verdict: 'approve' })).status).toBe(404);
    expect((await bazel.api('POST', '/api/reviews/..%2Fconfig.yaml/verdict', { verdict: 'approve' })).status).toBe(404);
    const running = await bazel.enqueue('acme/web-app#7', 'review-fleet');
    expect((await bazel.api('POST', `/api/jobs/${running}/verdict`, { verdict: 'approve' })).status).toBe(409);
    const cross = await bazel.rawRequest('POST', `/api/jobs/${id}/verdict`, { 'Content-Type': 'application/json', 'Sec-Fetch-Site': 'cross-site' }, JSON.stringify({ verdict: 'approve' }));
    expect(cross.status).toBe(403);
    expect(await verdicts(bazel)).toEqual({});
  });

  // BUG-QA-37, fixed in feat/pr-verdict: `.btn { display: flex }` beat the hidden
  // attribute, so Summary showed with an empty queue and on the Saved tab.
  test('no Summary button with nothing in the queue, nor on Saved', async ({ app, bazel }) => {
    await expect(ui.summaryButton(app)).toBeHidden();
    await done(bazel, K);
    await expect(ui.summaryButton(app)).toBeVisible();
    await ui.savedTab(app).click();
    await expect(ui.summaryButton(app)).toBeHidden();
  });

  test('the dialog: keyboard in and out, axe clean, and a denied clipboard leaves the text selected', async ({ app, bazel }) => {
    await decide(bazel, await done(bazel, K), 'approve');
    await app.reload();
    await ui.summaryButton(app).focus();
    await app.keyboard.press('Enter');
    await expect(ui.summaryDialog(app)).toBeVisible();
    await expect(ui.summaryText(app)).toBeFocused();
    const order: string[] = [];
    for (let i = 0; i < 2; i++) {
      await app.keyboard.press('Tab');
      order.push(await app.evaluate(() => (document.activeElement as HTMLElement).dataset.testid ?? ''));
    }
    expect(order).toEqual(['summary-close', 'summary-copy']);

    await app.addScriptTag({ content: fs.readFileSync(require.resolve('axe-core/axe.min.js'), 'utf8') });
    const axe = async (sel: string) => (await app.evaluate(async (s) => (window as any).axe.run(document.querySelector(s), {
      runOnly: ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa', 'best-practice'],
    }), sel)).violations.map((v: any) => v.id);
    expect(await axe('#summary-dialog')).toEqual([]);

    await app.keyboard.press('Escape');
    await expect(ui.summaryDialog(app)).toBeHidden();
    await expect(ui.summaryButton(app)).toBeFocused();
    const v = await openJob(app, RATE);
    expect(await axe('[data-testid=verdict-bar]')).toEqual([]);
    expect(await pressed(v)).toEqual(['approve']);

    await app.evaluate(() => Object.defineProperty(navigator, 'clipboard', {
      value: { writeText: () => Promise.reject(new DOMException('denied', 'NotAllowedError')) },
    }));
    await ui.summaryButton(app).click();
    await ui.summaryCopy(app).click();
    await expect(ui.summaryNote(app)).toHaveText(/^(copied|select the text and copy it yourself)$/);
    const selected = await app.evaluate(() => { const t = document.querySelector('#summary-text') as HTMLTextAreaElement; return t.value.slice(t.selectionStart, t.selectionEnd); });
    expect(selected).toBe(line(K, ':white_check_mark:'));
  });
});

// The reviewers on each Summary line: pending requests first (teams as
// org/slug), then who approved, asked for changes or commented. Each once, case
// aside; never the author, you, a bot, a dismissed or a pending review.
test.describe('P1 · the reviewers in the Summary', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });
  test.afterEach(async ({ bazel }) => { expect(writes(bazel)).toEqual([]); });

  const R = 'acme/api-core#102';
  const review = (login: string, state: string, bot = false) => ({ author: { login, ...(bot ? { __typename: 'Bot' } : {}) }, state });

  test('who is named, in what order, and who is left out', async ({ bazel }) => {
    bazel.updatePR(R, {
      reviewRequests: [{ login: 'Dave' }, { slug: 'core', organization: { login: 'acme' } }, { login: 'renovate[bot]' }, { login: 'e2e-user' }],
      latestReviews: [
        review('erin', 'CHANGES_REQUESTED'), review('dave', 'APPROVED'), review('frank', 'COMMENTED'),
        review('gina', 'DISMISSED'), review('hank', 'PENDING'), review('dependabot', 'APPROVED', true),
        review('app/ci', 'COMMENTED'), review('e2e-user', 'APPROVED'), review('E2E-USER', 'COMMENTED'),
      ],
    });
    // #102 is e2e-user's own PR in the fixtures: the author is the same check.
    await bazel.api('GET', '/api/prs?refresh=1');
    await decide(bazel, await done(bazel, R), 'approve');
    const s = await summary(bazel);
    expect(s.decided[0].reviewers).toEqual(['Dave', 'acme/core', 'erin', 'frank']);
    expect(s.text).toBe(`${url(R)} :white_check_mark: — Dave, acme/core, erin, frank`);
  });

  test('a PR with nobody to name keeps the line as it was; an undecided one still lists its reviewers', async ({ bazel }) => {
    await decide(bazel, await done(bazel, 'acme/web-app#7'), 'approve');
    await done(bazel, K);
    const s = await summary(bazel);
    expect(s.text).toBe(`${url('acme/web-app#7')} :white_check_mark:`);
    expect(s.undecided[0].reviewers).toEqual(['bob', 'carol']);
  });

  test('one read of gh for the whole queue, and a minute of cache', async ({ bazel }) => {
    await decide(bazel, await done(bazel, K), 'approve');
    await decide(bazel, await done(bazel, 'acme/web-app#7'), 'approve');
    const reads = () => bazel.ghCalls().filter((c) => c.argv.some((a) => a.startsWith('query=query bazelReviewers('))).length;
    await summary(bazel);
    expect(reads()).toBe(1);
    await summary(bazel);
    expect(reads()).toBe(1);
  });

  test('when the read fails the text goes without names and the dialog says why', async ({ app, bazel }) => {
    await decide(bazel, await done(bazel, K), 'approve');
    bazel.setGh({ fail: { reviewers: 'HTTP 502: Bad Gateway' } });
    const s = await summary(bazel);
    expect(s.text).toBe(`${url(K)} :white_check_mark:`);
    expect(s.reviewers_error).toContain('reviewers could not be read');
    await app.reload();
    await ui.summaryButton(app).click();
    await expect(app.getByTestId('summary-warning')).toContainText(/reviewers could not be read/i);
    await expect(ui.summaryText(app)).toHaveValue(`${url(K)} :white_check_mark:`);
  });
});
