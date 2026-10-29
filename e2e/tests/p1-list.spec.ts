import { test, expect } from '../harness/fixtures';
import { ui } from '../harness/ui';

test.describe('P1 · list filters', () => {
  test('ownership, drafts and repository filters narrow the list and the counter', async ({ app }) => {
    await expect(ui.prCount(app)).toHaveText('5 PRs');
    await app.getByRole('button', { name: 'mine only' }).click();
    await expect(ui.prCount(app)).toHaveText('2 of 5 PRs');
    await app.getByRole('button', { name: 'everyone' }).click();
    await app.getByRole('checkbox', { name: 'hide drafts' }).check();
    await expect(ui.prCount(app)).toHaveText('4 of 5 PRs');
    await app.getByRole('checkbox', { name: 'hide drafts' }).uncheck();
    const repo = app.getByRole('combobox').filter({ has: app.getByRole('option', { name: /^any repo/ }) });
    await repo.selectOption({ label: 'acme/web-app (2)' });
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
    const state = app.getByRole('combobox').filter({ has: app.getByRole('option', { name: 'not reviewed' }) });
    await state.selectOption({ label: '✓ reviewed' });
    await expect(ui.prCount(app)).toHaveText('1 of 5 PRs');
    await state.selectOption({ label: 'not reviewed' });
    await expect(ui.prCount(app)).toHaveText('4 of 5 PRs');

    bazel.updatePR('acme/api-core#101', { headRefOid: 'ffff101-new-commit' });
    const { json } = await bazel.api('GET', '/api/prs?refresh=1');
    expect(json.prs.find((p: any) => p.key === 'acme/api-core#101').changed_since_review).toBe(true);
    await app.reload();
    await expect(app.getByText('⟳ changed since review', { exact: true }).last()).toBeVisible();
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
