import { test, expect } from '../harness/fixtures';
import { Bazel } from '../harness/bazel';
import { openJob, ui } from '../harness/ui';

// The PR's own page (Overview) and its History, from feat/redesign 6789bcb /
// 160a9bc. Bases without the routes skip these tests.
const DETAILS = '/api/prs/acme/api-core/101/details';
const HISTORY = '/api/prs/acme/api-core/101/history';

async function needsPRPage(bazel: Bazel) {
  const r = await fetch(bazel.url + DETAILS);
  test.skip(!(r.headers.get('content-type') ?? '').includes('json'), 'this base has no PR page routes');
}

const detailCalls = (bazel: Bazel) =>
  bazel.ghCalls().filter((c) => c.argv[0] === 'pr' && c.argv[1] === 'view' && (c.argv.at(-1) ?? '').includes('statusCheckRollup'));

test.describe('P1 · PR page (Overview)', () => {
  test('details come from one gh pr view per head sha, in the page shape', async ({ bazel }) => {
    await bazel.api('GET', '/api/prs'); // the list is what tells Bazel the head sha
    await needsPRPage(bazel);
    const { status, json } = await bazel.api('GET', DETAILS);
    expect(status).toBe(200);
    expect(json.sha).toBe('aaaa101');
    expect(json.commits.map((c: any) => c.headline)).toEqual(['E2E-COMMIT-1 add the bucket', 'E2E-COMMIT-2 wire it into /v1']);
    expect(json.files.map((f: any) => f.path)).toEqual(['internal/limit/bucket.go']);
    expect(json.labels).toEqual(['e2e-label-perf']);
    expect(json.reviewers).toEqual(expect.arrayContaining([
      { login: 'bob', state: 'requested' }, { login: 'carol', state: 'approved' },
    ]));
    expect(json.checks).toEqual(expect.arrayContaining([
      { name: 'ci/test', state: 'passed' }, { name: 'lint', state: 'failed' },
    ]));
    expect(json.body_html).toContain('<strong>token bucket</strong>');

    await bazel.api('GET', DETAILS);
    await bazel.api('GET', DETAILS);
    expect(detailCalls(bazel)).toHaveLength(1); // cached while the head does not move

    bazel.updatePR('acme/api-core#101', { headRefOid: 'bbbb-new-head' });
    await bazel.api('GET', '/api/prs?refresh=1');
    expect((await bazel.api('GET', DETAILS)).json.sha).toBe('bbbb-new-head');
    expect(detailCalls(bazel)).toHaveLength(2);
  });

  test('a repository that is not watched gets 404 and no gh call, on every PR page route', async ({ bazel }) => {
    await needsPRPage(bazel);
    for (const [method, path] of [['GET', '/api/prs/evil/repo/1/details'], ['GET', '/api/prs/evil/repo/1/history'], ['POST', '/api/prs/evil/repo/1/history']]) {
      expect((await bazel.api(method, path)).status, `${method} ${path}`).toBe(404);
    }
    expect(bazel.ghCalls().filter((c) => c.argv.includes('evil/repo'))).toEqual([]);
    expect(bazel.agentCalls()).toEqual([]);
  });

  test('clicking a PR opens its page with files, reviewers and checks', async ({ app, bazel }) => {
    await needsPRPage(bazel);
    await ui.prRow(app, 'Add rate limiter to the public API').click();
    const page = app.getByTestId('pr-page');
    await expect(page).toHaveAttribute('data-loaded', 'true');
    await expect(page.getByTestId('pr-files')).toContainText('internal/limit/bucket.go');
    await expect(page.getByTestId('pr-check').and(app.locator('[data-name="lint"]'))).toHaveAttribute('data-state', 'failed');
    await expect(page.getByTestId('pr-reviewer').and(app.locator('[data-login="carol"]'))).toHaveAttribute('data-state', /approved/);
    await expect(page.getByTestId('pr-label')).toContainText('e2e-label-perf');
  });
});

test.describe('P1 · History', () => {
  test.use({
    bazelOptions: {
      config: {
        history_agent: { name: 'history', task: '/history-pr {{number}}', command: 'fake-agent', args: [], format: 'plain', env: { FAKE_AGENT_SCENARIO: 'history-md' } },
      },
    },
  });

  test('generated on demand as a job; parsed; never marks the PR reviewed nor lands in Saved', async ({ bazel }) => {
    await bazel.api('GET', '/api/prs');
    await needsPRPage(bazel);
    const before = (await bazel.api('GET', HISTORY)).json;
    expect(before.found).toBe(false);
    expect(before.agent).toBe('/history-pr');

    const q = await bazel.api('POST', HISTORY);
    expect(q.status).toBe(202);
    expect(q.json.history).toBe(true);
    expect(q.json.publishable).toBe(false);
    await bazel.waitJob(q.json.id, (j) => j.state === 'done');
    expect(bazel.agentCalls()[0].prompt.startsWith('/history-pr 101')).toBe(true);

    const h = (await bazel.api('GET', HISTORY)).json;
    expect(h.found).toBe(true);
    expect(h.stale ?? false).toBe(false);
    expect(h.commits_read).toBe(2);
    expect(h.what_html).toContain('E2E-HISTORY-WHAT');
    expect(h.decisions.map((d: any) => [d.n, d.title, d.sha])).toEqual([
      [1, 'E2E-DECISION-ONE token bucket over a fixed window', 'a0a0101'],
      [2, 'E2E-DECISION-TWO limits per API key', 'aaaa101'],
    ]);
    expect(h.decisions[0].rejected).toEqual(['Fixed window — spikes at the boundary.']);
    expect(h.still_open_html.join('')).toContain('E2E-STILL-OPEN');

    // A history is not a review.
    const pr = (await bazel.api('GET', '/api/prs')).json.prs.find((p: any) => p.key === 'acme/api-core#101');
    expect(pr.reviewed ?? false).toBe(false);
    expect((await bazel.api('GET', '/api/reviews')).json.reviews).toEqual([]);
    expect(bazel.savedReviews()).toEqual([]);
    const writes = bazel.ghCalls().filter((c) => c.argv[1] === 'comment' || (c.argv[0] === 'api' && c.argv[1] !== 'user'));
    expect(writes).toEqual([]);

    // A new head makes the history stale until it is generated again.
    bazel.updatePR('acme/api-core#101', { headRefOid: 'cccc-moved' });
    await bazel.api('GET', '/api/prs?refresh=1');
    expect((await bazel.api('GET', HISTORY)).json.stale).toBe(true);
  });

  test('the History tab of a job generates it and lists the decisions', async ({ app, bazel }) => {
    await needsPRPage(bazel);
    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    await app.reload();
    await openJob(app, 'Add rate limiter to the public API');
    await app.getByRole('tab', { name: 'History' }).click();
    await app.getByTestId('history-generate').click();
    await expect(app.getByTestId('history-decision')).toHaveCount(2, { timeout: 20_000 });
    await expect(app.getByTestId('history-decision').first()).toHaveAttribute('data-n', '1');
  });
});
