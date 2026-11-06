import { test, expect } from '../harness/fixtures';
import { Bazel } from '../harness/bazel';
import { ui } from '../harness/ui';

// "Needs you": the PRs where the user (e2e-user) has something to do, from one
// read of GitHub that the fake gh answers from needs.json.

const ME = 'e2e-user';
const at = (h: number) => new Date(Date.UTC(2026, 10, 1) + h * 3600_000).toISOString();

// node is a PR as the GraphQL search returns it, with nothing to say.
function node(repo: string, number: number, title: string, author: string, extra: Record<string, unknown> = {}) {
  return {
    number, title, url: `https://github.com/${repo}/pull/${number}`, isDraft: false,
    createdAt: at(0), updatedAt: at(50), additions: 1, deletions: 0, changedFiles: 1,
    headRefName: 'b', baseRefName: 'main', headRefOid: 'x', reviewDecision: '', mergeable: 'MERGEABLE',
    author: { login: author }, repository: { nameWithOwner: repo },
    commits: { nodes: [{ commit: { committedDate: at(1), statusCheckRollup: null } }] },
    forcePushes: { nodes: [] }, requests: { nodes: [] }, reviews: { nodes: [] },
    comments: { nodes: [] }, reviewThreads: { nodes: [] },
    ...extra,
  };
}
const review = (login: string, state: string, h: number) => ({ id: `${login}-${h}`, author: { login }, state, submittedAt: at(h) });
const pushed = (h: number, state: string | null = null) => ({ nodes: [{ commit: { committedDate: at(h), statusCheckRollup: state && { state } } }] });

// setup writes the answer and has the server read it: the page that opened
// before the test may already have a minute's cache of the empty one.
async function setup(bazel: Bazel) {
  bazel.setNeeds({
    requested: [node('acme/api-core', 101, 'Add rate limiter to the public API', 'alice', {
      requests: { nodes: [{ createdAt: at(5), requestedReviewer: { __typename: 'User', login: ME } }] },
    })],
    reviewed: [
      node('acme/web-app', 7, 'Dark mode toggle in settings', 'carol', { reviews: { nodes: [review(ME, 'CHANGES_REQUESTED', 10)] }, commits: pushed(12) }),
      node('acme/api-core', 103, 'WIP: experimental gRPC gateway', 'bob', { reviews: { nodes: [review(ME, 'CHANGES_REQUESTED', 10)] }, commits: pushed(9) }),
      node('acme/api-core', 104, 'Already approved by me', 'bob', { reviews: { nodes: [review(ME, 'APPROVED', 10)] } }),
    ],
    authored: [
      node('acme/api-core', 102, 'Fix nil pointer on empty config', ME, { commits: pushed(3, 'FAILURE') }),
      node('elsewhere/lib', 5, 'A PR in a repo not watched', ME, { mergeable: 'CONFLICTING' }),
    ],
  });
  expect((await bazel.api('GET', '/api/needs?refresh=1')).status).toBe(200);
  return needsCalls(bazel).length;
}

const needsCalls = (bz: Bazel) => bz.ghCalls().filter((c) => c.argv[1] === 'graphql' && c.argv.some((a) => a.startsWith('query=query bazelNeedsYou')));
const writes = (bz: Bazel) =>
  bz.ghCalls().filter((c) => (c.argv[0] === 'pr' && c.argv[1] === 'comment') || (c.argv[0] === 'api' && c.argv[1] !== 'user' && c.argv[1] !== 'graphql'));

test.describe('P1 · needs you', () => {
  test('each PR once, under its reason, with the counter and the watched repos first', async ({ app, bazel }) => {
    const calls = await setup(bazel);
    await app.reload();
    await expect(app.getByTestId('needs-count')).toHaveText('3'); // waiting asks nothing of you
    await app.getByTestId('nav-needs').click();
    const page = app.getByTestId('needs-page');
    await expect(page.getByTestId('needs-group')).toHaveCount(4);
    const order = await page.getByTestId('needs-group').evaluateAll((gs) => gs.map((g) => (g as HTMLElement).dataset.category));
    expect(order).toEqual(['requested', 'replied', 'waiting', 'yours']);
    const row = (key: string) => page.locator(`[data-testid=needs-row][data-pr-key="${key}"]`);
    await expect(row('acme/api-core#101').getByTestId('needs-reason')).toContainText('review requested from you');
    await expect(row('acme/web-app#7').getByTestId('needs-reason')).toContainText('@carol pushed since');
    await expect(row('acme/api-core#103').getByTestId('needs-reason')).toContainText('waiting for @bob');
    await expect(row('acme/api-core#102').getByTestId('needs-reason')).toContainText('checks failing');
    await expect(row('acme/api-core#104')).toHaveCount(0);
    await expect(row('elsewhere/lib#5')).toHaveCount(0);
    await expect(app.getByTestId('needs-note')).toContainText("1 more in repos you don't watch");
    await expect(row('acme/web-app#7')).toContainText('web-app #7 · @carol');
    await expect(app).toHaveTitle('(3) Bazel · needs you');

    await app.getByTestId('needs-scope-all').click();
    await expect(row('elsewhere/lib#5').getByTestId('needs-reason')).toContainText('conflicts with main');
    // The page and the counter came from one read; the scope is the same read.
    expect(needsCalls(bazel)).toHaveLength(calls);
    expect(writes(bazel)).toEqual([]);
  });

  test('a row opens the PR in its Overview, and Review runs the default agent on it', async ({ app, bazel }) => {
    await setup(bazel);
    await app.goto(app.url().replace(/#.*$/, '') + '#/needs');
    await app.reload();
    const row = app.locator('[data-testid=needs-row][data-pr-key="acme/api-core#101"]');
    await row.getByText('Add rate limiter to the public API').click();
    await expect(app.getByTestId('nav-dashboard')).toHaveClass(/on/);
    await expect(app.getByTestId('pr-page')).toHaveAttribute('data-pr-key', 'acme/api-core#101');

    await app.getByTestId('nav-needs').click();
    await row.getByTestId('needs-review').click();
    await expect.poll(async () => (await bazel.api('GET', '/api/jobs')).json.jobs.length).toBe(1);
    const [job] = (await bazel.api('GET', '/api/jobs')).json.jobs;
    expect(job.pr.key).toBe('acme/api-core#101');
    await expect(ui.jobCard(app, 'Add rate limiter to the public API')).toBeVisible();
    // Your own PRs offer no review.
    await app.getByTestId('nav-needs').click();
    await expect(app.locator('[data-testid=needs-row][data-pr-key="acme/api-core#102"]').getByTestId('needs-review')).toHaveCount(0);
  });

  test('refresh reads again and redraws only what changed', async ({ app, bazel }) => {
    const calls = await setup(bazel);
    await app.goto(app.url().replace(/#.*$/, '') + '#/needs');
    await app.reload();
    const list = app.getByTestId('needs-list');
    await expect(list.getByTestId('needs-row')).toHaveCount(4);
    const first = await list.getByTestId('needs-row').first().elementHandle();
    await app.getByTestId('refresh').click();
    await expect.poll(() => needsCalls(bazel).length).toBe(calls + 1);
    await app.waitForTimeout(300);
    // The same answer leaves the same nodes on the page.
    expect(await first!.evaluate((n) => n.isConnected)).toBe(true);

    bazel.setNeeds({});
    await app.getByTestId('refresh').click();
    await expect(app.getByTestId('needs-empty')).toBeVisible();
    await expect(app.getByTestId('needs-count')).toBeHidden();
  });

  test('a late answer for the scope you left is dropped, and a failed read shows the last list as stale', async ({ app, bazel }) => {
    await setup(bazel);
    await app.goto(app.url().replace(/#.*$/, '') + '#/needs');
    await app.reload();
    const rows = app.getByTestId('needs-row');
    await expect(rows).toHaveCount(4);
    // "all repos" answers late, after "watched repos" was chosen again.
    let release!: () => void;
    const held = new Promise<void>((r) => { release = r; });
    await app.route('**/api/needs?*all=1*', async (route) => { await held; await route.continue(); });
    await app.getByTestId('needs-scope-all').click();
    await app.getByTestId('needs-scope-watched').click();
    await expect(rows).toHaveCount(4);
    release();
    await app.waitForTimeout(500);
    await expect(rows).toHaveCount(4);
    await expect(app.locator('[data-testid=needs-row][data-pr-key="elsewhere/lib#5"]')).toHaveCount(0);
    await app.unroute('**/api/needs?*all=1*');

    bazel.setGh({ fail: { needs: 'HTTP 502: Bad Gateway' } });
    await app.getByTestId('refresh').click();
    await expect(app.getByTestId('needs-stale')).toContainText('HTTP 502');
    await expect(rows).toHaveCount(4);
  });
});
