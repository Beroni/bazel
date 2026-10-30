import { test, expect, knownBug } from '../harness/fixtures';
import { filterByReview, openFilters, selectByLabel, ui } from '../harness/ui';

test.describe('P1 · list filters', () => {
  test('ownership, drafts and repository filters narrow the list and the counter', async ({ app }) => {
    await expect(ui.prCount(app)).toHaveText('5 PRs');
    await openFilters(app);
    await ui.scope(app, 'mine').click();
    await expect(ui.prCount(app)).toHaveText('2 of 5 PRs');
    await openFilters(app);
    await ui.scope(app, 'all').click();
    await openFilters(app);
    await ui.hideDrafts(app).check();
    await expect(ui.prCount(app)).toHaveText('4 of 5 PRs');
    await openFilters(app);
    await ui.hideDrafts(app).uncheck();
    await openFilters(app);
    // "acme/web-app (2)" on master, "web-app (2)" when one owner is shown (#25).
    await selectByLabel(ui.repoFilter(app), /web-app \(2\)$/);
    await expect(ui.prCount(app)).toHaveText('2 of 5 PRs');
  });

  test('filtering never unticks, and run only takes what is visible', async ({ app, bazel }) => {
    await ui.selectAll(app).check();
    await expect(ui.prCount(app)).toHaveText('5 PRs · 5 ticked');
    await ui.filter(app).fill('dark mode');
    await expect(ui.prCount(app)).toHaveText('1 of 5 PRs · 1 ticked');
    await ui.filter(app).fill('');
    await expect(ui.prCount(app)).toHaveText('5 PRs · 5 ticked');
    await ui.filter(app).fill('dark mode');
    await ui.agentSelect(app).selectOption('review-fleet');
    await ui.runButton(app).click();
    await expect.poll(async () => (await bazel.api('GET', '/api/jobs')).json.jobs.length).toBe(1);
    const [job] = (await bazel.api('GET', '/api/jobs')).json.jobs;
    expect(job.pr.key).toBe('acme/web-app#7');
  });

  test('review state: ✓ reviewed, then ⟳ changed since review when the head moves', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    await app.reload();
    await filterByReview(app, 'reviewed');
    await expect(ui.prCount(app)).toHaveText('1 of 5 PRs');
    await filterByReview(app, 'none');
    await expect(ui.prCount(app)).toHaveText('4 of 5 PRs');

    bazel.updatePR('acme/api-core#101', { headRefOid: 'ffff101-new-commit' });
    const { json } = await bazel.api('GET', '/api/prs?refresh=1');
    expect(json.prs.find((p: any) => p.key === 'acme/api-core#101').changed_since_review).toBe(true);
    await app.reload();
    await filterByReview(app, 'all');
    // "⟳ changed since review" on master, "⟳ changed" since #25, "changed" on the redesign.
    await expect(ui.reviewTag(ui.prRow(app, 'Add rate limiter to the public API'))).toHaveText(/^(⟳ )?changed( since review)?$/);
    const changed = await bazel.tool('list_prs', { status: 'changed' });
    expect(changed.out.prs.map((p: any) => p.key)).toEqual(['acme/api-core#101']);
  });

  test('the PR list is cached; refresh goes back to gh', async ({ bazel }) => {
    const searches = () => bazel.ghCalls().filter((c) => c.argv[0] === 'search').length;
    await bazel.api('GET', '/api/prs');
    await bazel.api('GET', '/api/prs');
    const cached = searches();
    await bazel.api('GET', '/api/prs?refresh=1');
    expect(searches()).toBe(cached + 1);
  });
});

test.describe('P1 · gh search failing', () => {
  test.use({ bazelOptions: { gh: { fail: { search: 'HTTP 502: search is down', 'list:acme/web-app': 'HTTP 404: Not Found' } } } });

  test('falls back to one gh pr list per repo and reports every error', async ({ bazel }) => {
    const { json } = await bazel.api('GET', '/api/prs');
    expect(json.prs.map((p: any) => p.key).sort()).toEqual(['acme/api-core#101', 'acme/api-core#102', 'acme/api-core#103']);
    expect(json.repo_errors).toEqual(expect.arrayContaining([
      { repo: 'acme/web-app', error: 'HTTP 404: Not Found' },
      { repo: '', error: 'gh search prs fallback: HTTP 502: search is down' },
    ]));
  });
});

test.describe('P2 · authors and drafts from the config', () => {
  test.use({ bazelOptions: { config: { authors: ['@Alice'], include_drafts: false } } });

  test('authors (case-insensitive, @ optional) and include_drafts filter at the source', async ({ bazel }) => {
    const { json } = await bazel.api('GET', '/api/prs');
    expect(json.prs.map((p: any) => p.key)).toEqual(['acme/api-core#101']);
  });
});

// BUG-QA-06: ticking "select all" while the PR list is still loading leaves
// the box checked with nothing selected (it selects visiblePRs() of an empty
// list and the box is never re-synced on render). The run button stays
// disabled under a checked box. Under load it is also what made the built-in
// skill specs time out (the test ticked before /api/prs answered).
// Expected: the box reflects the selection — checked ⇒ the visible PRs ticked.
test.describe('P2 · select all during load', () => {
  test('a "select all" ticked before the list arrives does not lie', async ({ page, bazel }) => {
    knownBug('BUG-QA-06', 'select-all checked while nothing is selected');
    await page.route('**/api/prs*', async (r) => { await new Promise((x) => setTimeout(x, 1500)); await r.continue(); });
    await page.goto(bazel.url + '/');
    await ui.filter(page).fill('rate limiter');
    await ui.selectAll(page).check();
    await expect(ui.prRow(page, 'Add rate limiter to the public API')).toBeVisible();
    const checked = await ui.selectAll(page).isChecked();
    expect(await ui.runButton(page).isEnabled(), `select all is ${checked ? '' : 'un'}checked`).toBe(checked);
  });
});
