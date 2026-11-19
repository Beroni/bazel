import { test, expect } from '../harness/fixtures';
import { Bazel } from '../harness/bazel';
import { openJob, ui } from '../harness/ui';

// Your decision on a PR after reading its review: approve, approve with
// comments, request changes. It stays in Bazel — nothing goes to GitHub — and
// the queue's Summary turns the decisions into the lines pasted into Slack.
const AGENTS = [{ name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } }];
const RATE = 'Add rate limiter to the public API';

/** gh calls that write to a PR: a review, a comment, a reaction. */
const writes = (bz: Bazel) => bz.ghCalls().filter((c) => (c.argv[0] === 'pr' && c.argv[1] === 'comment')
  || (c.argv[0] === 'api' && c.argv.some((a) => a === 'POST' || a === '-X' || a === '--method')));

async function reviewed(bazel: Bazel, ref: string): Promise<{ id: string; url: string }> {
  const id = await bazel.enqueue(ref, 'review-fleet');
  const job = await bazel.waitJob(id, (j) => j.state === 'done');
  const [repo, n] = ref.split('#');
  return { id, url: job.pr.url || `https://github.com/${repo}/pull/${n}` };
}

test.describe('P1 · your decision on a PR', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  test('the Report\'s buttons take, switch and take back the decision; the card shows it; GitHub hears nothing', async ({ app, bazel }) => {
    await reviewed(bazel, 'acme/api-core#101');
    await app.reload();
    const viewer = await openJob(app, RATE);
    const card = ui.jobCard(app, RATE);
    for (const id of ['approve', 'approve_with_comments', 'request_changes'] as const) {
      await expect(ui.verdict(viewer, id)).toHaveAttribute('aria-pressed', 'false');
    }
    await expect(ui.jobVerdict(card)).toBeHidden();

    await ui.verdict(viewer, 'approve').click();
    await expect(ui.verdict(viewer, 'approve')).toHaveAttribute('aria-pressed', 'true');
    await expect(ui.jobVerdict(card)).toHaveText('✅');

    await ui.verdict(viewer, 'request_changes').click();
    await expect(ui.verdict(viewer, 'request_changes')).toHaveAttribute('aria-pressed', 'true');
    await expect(ui.verdict(viewer, 'approve')).toHaveAttribute('aria-pressed', 'false');
    await expect(ui.jobVerdict(card)).toHaveText('🔄');
    expect((await bazel.api('GET', '/api/verdicts')).json.verdicts['acme/api-core#101'].verdict).toBe('request_changes');

    // A reload reads it from the server.
    await app.reload();
    const again = await openJob(app, RATE);
    await expect(ui.verdict(again, 'request_changes')).toHaveAttribute('aria-pressed', 'true');

    await ui.verdict(again, 'request_changes').click();
    await expect(ui.verdict(again, 'request_changes')).toHaveAttribute('aria-pressed', 'false');
    await expect(ui.jobVerdict(ui.jobCard(app, RATE))).toBeHidden();
    expect((await bazel.api('GET', '/api/verdicts')).json.verdicts).toEqual({});
    expect(writes(bazel)).toEqual([]);
  });

  test('the Saved view decides too, and the decision outlives a restart', async ({ app, bazel }) => {
    await reviewed(bazel, 'acme/api-core#101');
    await app.reload();
    await ui.savedTab(app).click();
    await app.getByTestId('saved-row').first().click();
    const viewer = ui.viewer(app);
    await ui.verdict(viewer, 'approve_with_comments').click();
    await expect(ui.verdict(viewer, 'approve_with_comments')).toHaveAttribute('aria-pressed', 'true');

    await bazel.restart({ samePort: true });
    await app.reload();
    await ui.savedTab(app).click();
    await app.getByTestId('saved-row').first().click();
    await expect(ui.verdict(ui.viewer(app), 'approve_with_comments')).toHaveAttribute('aria-pressed', 'true');
    expect(writes(bazel)).toEqual([]);
  });

  test('a PR that moved after the decision says so, and keeps it', async ({ app, bazel }) => {
    await reviewed(bazel, 'acme/api-core#101');
    await app.reload();
    const viewer = await openJob(app, RATE);
    await ui.verdict(viewer, 'approve').click();
    await expect(ui.verdict(viewer, 'approve')).toHaveAttribute('aria-pressed', 'true');
    await expect(ui.verdictMoved(viewer)).toBeHidden();
    const decided = (await bazel.api('GET', '/api/verdicts')).json.verdicts['acme/api-core#101'].head as string;

    bazel.movePR('acme/api-core#101');
    await app.getByTestId('refresh').click();
    await expect(ui.verdictMoved(viewer)).toHaveText(`decided on ${decided.slice(0, 7)}, PR moved`);
    await expect(ui.verdict(viewer, 'approve')).toHaveAttribute('aria-pressed', 'true');
    await expect(ui.jobVerdict(ui.jobCard(app, RATE))).toHaveText('✅');

    // BUG-QA-38: in the Summary it is no approval: out of the text, listed apart.
    await ui.summaryButton(app).click();
    await expect(ui.summaryText(app)).toHaveValue('');
    await expect(ui.summaryMoved(app)).toHaveText('moved since your decision: #101');
    expect((await bazel.api('GET', '/api/summary')).json.moved.map((p: any) => p.key)).toEqual(['acme/api-core#101']);
  });

  test('Summary: one Slack line per decided PR, grouped by author, each once; the rest listed apart; edit and copy', async ({ app, bazel }) => {
    const a = await reviewed(bazel, 'acme/api-core#101');
    const b = await reviewed(bazel, 'acme/api-core#102');
    const c = await reviewed(bazel, 'acme/web-app#7');
    const a2 = await reviewed(bazel, 'acme/api-core#101'); // the same PR, a second card
    const decide = async (id: string, verdict: string) =>
      expect((await bazel.api('POST', `/api/jobs/${id}/verdict`, { verdict })).status).toBe(200);
    await decide(a.id, 'approve');
    await decide(c.id, 'approve_with_comments');
    await decide(a2.id, 'request_changes'); // the PR's decision, from either card

    // By the PR's author (alice opened #101, carol web-app#7), then repo and
    // number; #101's fixture asks bob and has carol's approval.
    const text = `alice\n${a.url} :git_change_request: — bob, carol\n\ncarol\n${c.url} :memo::white_check_mark:`;
    const summary = (await bazel.api('GET', '/api/summary')).json;
    expect(summary.text).toBe(text);
    expect(summary.undecided.map((p: any) => p.key)).toEqual(['acme/api-core#102']);

    await app.context().grantPermissions(['clipboard-read', 'clipboard-write']);
    await app.reload();
    await ui.summaryButton(app).click();
    await expect(ui.summaryDialog(app)).toBeVisible();
    await expect(ui.summaryText(app)).toHaveValue(text);
    await expect(ui.summaryMissing(app)).toHaveText('no decision yet: #102');
    expect(b.url).not.toBe('');

    const edited = text + '\nthanks!';
    await ui.summaryText(app).fill(edited);
    await ui.summaryCopy(app).click();
    await expect(ui.summaryNote(app)).toHaveText('copied');
    expect(await app.evaluate(() => navigator.clipboard.readText())).toBe(edited);
    expect(writes(bazel)).toEqual([]);
  });

  // PV-3: "/" jumps to the PR search from anywhere, but not while you type: in
  // the Summary's text, a URL is all slashes.
  test('typing "/" in the Summary stays in the text', async ({ app, bazel }) => {
    const a = await reviewed(bazel, 'acme/api-core#101');
    expect((await bazel.api('POST', `/api/jobs/${a.id}/verdict`, { verdict: 'approve' })).status).toBe(200);
    await app.reload();
    await ui.summaryButton(app).click();
    const text = ui.summaryText(app);
    await text.fill('');
    await text.pressSequentially('a/b/c');
    await expect(text).toHaveValue('a/b/c');
    await expect(text).toBeFocused();
    await expect(ui.summaryDialog(app)).toBeVisible();
    await expect(ui.filter(app)).toHaveValue('');

    // Outside a field the shortcut still works.
    await app.keyboard.press('Escape');
    await expect(ui.summaryDialog(app)).toBeHidden();
    await app.locator('body').click({ position: { x: 1, y: 1 } });
    await app.keyboard.press('/');
    await expect(ui.filter(app)).toBeFocused();
  });

  // Who reviews each PR goes after its emoji, from one read-only GraphQL call:
  // requests pending and reviews, each once, with no author, no you, no bot and
  // no dismissed review; a team as org/slug; no @, so nothing mentions anyone.
  test('Summary names the reviewers; when gh cannot read them, the lines go without', async ({ app, bazel }) => {
    bazel.updatePR('acme/api-core#101', {
      reviewRequests: [{ login: 'bob' }, { slug: 'core', organization: { login: 'acme' } }, { login: 'e2e-user' }, { __typename: 'Bot', login: 'copilot' }],
      latestReviews: [
        { author: { login: 'carol' }, state: 'APPROVED' },
        { author: { login: 'BOB' }, state: 'COMMENTED' },
        { author: { login: 'alice' }, state: 'COMMENTED' },
        { author: { login: 'renovate[bot]' }, state: 'COMMENTED' },
        { author: { login: 'dan' }, state: 'DISMISSED' },
      ],
    });
    const a = await reviewed(bazel, 'acme/api-core#101');
    const c = await reviewed(bazel, 'acme/web-app#7');
    for (const [id, verdict] of [[a.id, 'approve'], [c.id, 'request_changes']]) {
      expect((await bazel.api('POST', `/api/jobs/${id}/verdict`, { verdict })).status).toBe(200);
    }
    const text = `alice\n${a.url} :white_check_mark: — bob, acme/core, carol\n\ncarol\n${c.url} :git_change_request:`;
    await app.reload();
    await ui.summaryButton(app).click();
    await expect(ui.summaryText(app)).toHaveValue(text);
    await expect(ui.summaryWarning(app)).toBeHidden();
    const summary = (await bazel.api('GET', '/api/summary')).json;
    expect(summary.decided.map((p: any) => p.reviewers)).toEqual([['bob', 'acme/core', 'carol'], []]);
    expect(summary.text).not.toContain('@');

    // A gh that fails, for a PR not read in the last minute: the lines go
    // without names, and the dialog says why.
    await ui.summaryDialog(app).getByTestId('summary-close').click();
    bazel.setGh({ fail: { reviewers: 'HTTP 502: Bad Gateway' } });
    await reviewed(bazel, 'acme/web-app#8');
    await ui.summaryButton(app).click();
    await expect(ui.summaryText(app)).toHaveValue(`alice\n${a.url} :white_check_mark:\n\ncarol\n${c.url} :git_change_request:`);
    await expect(ui.summaryWarning(app)).toHaveText('Reviewers could not be read, so the lines go without them.');
    expect(writes(bazel)).toEqual([]);
  });

  // BUG-QA-37: `.btn { display: … }` beat the hidden attribute, so Summary
  // showed with an empty queue and on the Saved tab.
  test('Summary only shows on a queue with a review in it', async ({ app, bazel }) => {
    await expect(ui.summaryButton(app)).toBeHidden();
    await reviewed(bazel, 'acme/api-core#101');
    await app.reload();
    await expect(ui.summaryButton(app)).toBeVisible();
    await ui.savedTab(app).click();
    await expect(ui.summaryButton(app)).toBeHidden();
  });

  // BUG-QA-39: two PRs of the summary with one number read the same as "#101";
  // then each goes by repo#N, and a number alone is enough otherwise.
  test('the lists apart name the repo when two PRs share a number', async ({ app, bazel }) => {
    bazel.addPR({ repo: 'globex/api-core', number: 101, title: 'Other owner, same name', headRefName: 'g', author: { login: 'zoe' } });
    await reviewed(bazel, 'acme/api-core#101');
    await reviewed(bazel, 'globex/api-core#101');
    await reviewed(bazel, 'acme/web-app#7');
    await app.reload();
    await ui.summaryButton(app).click();
    await expect(ui.summaryMissing(app)).toHaveText('no decision yet: acme/api-core#101, #7, globex/api-core#101');
  });
});
