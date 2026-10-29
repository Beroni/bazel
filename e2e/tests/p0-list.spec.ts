import { test, expect } from '../harness/fixtures';
import { ui } from '../harness/ui';

const TITLES = [
  'Add rate limiter to the public API',
  'Fix nil pointer on empty config',
  'WIP: experimental gRPC gateway',
  'Dark mode toggle in settings',
  'Bump dependencies',
];

test.describe('P0 · PR list', () => {
  test('the list loads every fixture PR, from one gh search', async ({ app, bazel }) => {
    for (const title of TITLES) await expect(ui.prRow(app, title)).toBeVisible();
    await expect(ui.prCount(app)).toHaveText('5 PRs');
    await expect(ui.whoami(app)).toHaveText('@e2e-user');

    // Two watched repos → one `gh search prs` with both, not a fan-out.
    const searches = bazel.ghCalls().filter((c) => c.argv[0] === 'search' && c.argv[1] === 'prs');
    expect(searches.length).toBeGreaterThanOrEqual(1);
    expect(searches[0].argv.at(-1)).toBe('repo:acme/api-core repo:acme/web-app');
    expect(bazel.ghCalls().some((c) => c.argv[0] === 'pr' && c.argv[1] === 'list')).toBe(false);
  });

  test('GET /api/prs returns the fixtures with repo, author and ownership', async ({ bazel }) => {
    const { status, json } = await bazel.api('GET', '/api/prs');
    expect(status).toBe(200);
    expect(json.repo_errors).toEqual([]);
    const byKey = Object.fromEntries(json.prs.map((p: any) => [p.key, p]));
    expect(Object.keys(byKey).sort()).toEqual([
      'acme/api-core#101', 'acme/api-core#102', 'acme/api-core#103', 'acme/web-app#7', 'acme/web-app#8',
    ]);
    expect(byKey['acme/api-core#102'].mine).toBe(true);
    expect(byKey['acme/api-core#101'].mine).toBe(false);
    expect(byKey['acme/api-core#103'].draft).toBe(true);
    expect(byKey['acme/api-core#101'].body_html).toContain('<strong>token bucket</strong>');
  });

  test('the text filter narrows the list and the counter says so', async ({ app }) => {
    await ui.filter(app).fill('dark mode');
    await expect(ui.prCount(app)).toHaveText('1 of 5 PRs');
    await expect(ui.prRow(app, 'Dark mode toggle in settings')).toBeVisible();
    await expect(app.getByText('Add rate limiter to the public API', { exact: true })).toHaveCount(0);
  });
});
