import { test, expect } from '../harness/fixtures';
import { filterByReview, openFilters, ui } from '../harness/ui';

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
    await ui.repoFilter(app).selectOption({ label: 'web-app (2)' });
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

  test('review state: reviewed, then changed when the head moves', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    await app.reload();
    await filterByReview(app, 'reviewed');
    await expect(ui.prCount(app)).toHaveText('1 of 5 PRs');
    await filterByReview(app, 'none');
    await expect(ui.prCount(app)).toHaveText('4 of 5 PRs');

    bazel.movePR('acme/api-core#101');
    const { json } = await bazel.api('GET', '/api/prs?refresh=1');
    expect(json.prs.find((p: any) => p.key === 'acme/api-core#101').changed_since_review).toBe(true);
    await app.reload();
    await filterByReview(app, 'all');
    await expect(ui.reviewTag(ui.prRow(app, 'Add rate limiter to the public API'))).toHaveText('changed');
    const changed = await bazel.tool('list_prs', { status: 'changed' });
    expect(changed.out.prs.map((p: any) => p.key)).toEqual(['acme/api-core#101']);
  });

  test('the PR list is cached; refresh goes back to gh', async ({ bazel }) => {
    const lists = () => bazel.ghCalls().filter((c) => c.argv[0] === 'pr' && c.argv[1] === 'list').length;
    await bazel.api('GET', '/api/prs');
    await bazel.api('GET', '/api/prs');
    const cached = lists();
    await bazel.api('GET', '/api/prs?refresh=1');
    expect(lists()).toBe(cached + 2); // one per repo
  });
});

test.describe('P1 · a repository failing', () => {
  test.use({ bazelOptions: { gh: { fail: { 'list:acme/web-app': 'HTTP 404: Not Found' } } } });

  test('the others still list, and the error says which', async ({ bazel }) => {
    const { json } = await bazel.api('GET', '/api/prs');
    expect(json.prs.map((p: any) => p.key).sort()).toEqual(['acme/api-core#101', 'acme/api-core#102', 'acme/api-core#103']);
    expect(json.repo_errors).toEqual(expect.arrayContaining([{ repo: 'acme/web-app', error: 'HTTP 404: Not Found' }]));
  });
});

test.describe('P1 · what gh lists', () => {
  test('a closed PR is not listed', async ({ bazel }) => {
    bazel.addPR({ repo: 'acme/api-core', number: 150, title: 'Closed long ago', state: 'CLOSED', headRefName: 'old', author: { login: 'zoe' } });
    const { json } = await bazel.api('GET', '/api/prs?refresh=1');
    expect(json.prs.map((p: any) => p.key)).toHaveLength(5);
  });

  // S16, fixed in #32: gh stops at the limit in silence, so a repo with more open PRs looks
  // like a repo with exactly that many. Expected: the list says it is cut short.
  test('a repository past the limit says the list is cut short', async ({ bazel }) => {
    for (let n = 200; n < 301; n++) {
      bazel.addPR({ repo: 'acme/web-app', number: n, title: `Generated ${n}`, headRefName: `gen/${n}`, headRefOid: `gen${n}`, author: { login: 'zoe' } });
    }
    const { json } = await bazel.api('GET', '/api/prs?refresh=1');
    expect(json.repo_errors).toEqual(expect.arrayContaining([expect.objectContaining({ repo: 'acme/web-app', error: expect.stringContaining('more than 100 open PRs') })]));
  });
});

test.describe('P2 · authors and drafts from the config', () => {
  test.use({ bazelOptions: { config: { authors: ['@Alice'], include_drafts: false } } });

  test('authors (case-insensitive, @ optional) and include_drafts filter at the source', async ({ bazel }) => {
    const { json } = await bazel.api('GET', '/api/prs');
    expect(json.prs.map((p: any) => p.key)).toEqual(['acme/api-core#101']);
  });
});

// BUG-QA-06, fixed in #27: ticking "select all" while the PR list was loading
// left the box checked with nothing selected and the run button disabled.
// The box has to reflect the selection.
test.describe('P2 · select all during load', () => {
  test('a "select all" ticked before the list arrives does not lie', async ({ page, bazel }) => {
    await page.route('**/api/prs*', async (r) => { await new Promise((x) => setTimeout(x, 1500)); await r.continue(); });
    await page.goto(bazel.url + '/');
    await ui.filter(page).fill('rate limiter');
    await ui.selectAll(page).check();
    await expect(ui.prRow(page, 'Add rate limiter to the public API')).toBeVisible();
    const checked = await ui.selectAll(page).isChecked();
    expect(await ui.runButton(page).isEnabled(), `select all is ${checked ? '' : 'un'}checked`).toBe(checked);
  });
});
