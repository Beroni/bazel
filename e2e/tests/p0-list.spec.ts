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
  test('the list loads every fixture PR', async ({ app }) => {
    for (const title of TITLES) await expect(ui.prRow(app, title)).toBeVisible();
    await expect(ui.prCount(app)).toHaveText('5 PRs');
    await expect(ui.whoami(app)).toHaveText('@e2e-user');
  });

  // BUG-QA-26, fixed in #32: with two or more repositories Bazel asked `gh search
  // prs --json` for fields only `gh pr list` has, so every listing failed, fell
  // back to one `gh pr list` per repo and showed "⚠ : gh search prs fallback". A
  // fake that took any field hid it: the fake now keeps gh's field lists.
  test('listing two repos fails nothing', async ({ app, bazel }) => {
    await expect(ui.prCount(app)).toHaveText('5 PRs');
    expect((await bazel.api('GET', '/api/prs')).json.repo_errors).toEqual([]);
    await expect(ui.banner(app)).toBeHidden();
    expect(bazel.ghCalls().filter((c) => c.exit !== 0)).toEqual([]);
  });

  test('the list goes through gh with both repositories', async ({ app, bazel }) => {
    await expect(ui.prCount(app)).toHaveText('5 PRs');
    // One `gh pr list` for each watched repo, open PRs only.
    const lists = bazel.ghCalls().filter((c) => c.argv[0] === 'pr' && c.argv[1] === 'list' && c.exit === 0);
    const repo = (c: { argv: string[] }) => c.argv[c.argv.indexOf('--repo') + 1];
    expect(lists.map(repo).sort()).toEqual(['acme/api-core', 'acme/web-app']);
    for (const c of lists) expect(c.argv[c.argv.indexOf('--state') + 1]).toBe('open');
  });

  test('GET /api/prs returns the fixtures with repo, author and ownership', async ({ bazel }) => {
    const { status, json } = await bazel.api('GET', '/api/prs');
    expect(status).toBe(200);
    const byKey = Object.fromEntries(json.prs.map((p: any) => [p.key, p]));
    expect(Object.keys(byKey).sort()).toEqual([
      'acme/api-core#101', 'acme/api-core#102', 'acme/api-core#103', 'acme/web-app#7', 'acme/web-app#8',
    ]);
    expect(byKey['acme/api-core#102'].mine).toBe(true);
    expect(byKey['acme/api-core#101'].mine).toBe(false);
    expect(byKey['acme/api-core#103'].draft).toBe(true);
  });

  test('the text filter narrows the list and the counter says so', async ({ app }) => {
    await ui.filter(app).fill('dark mode');
    await expect(ui.prCount(app)).toHaveText('1 of 5 PRs');
    await expect(ui.prRow(app, 'Dark mode toggle in settings')).toBeVisible();
    await expect(app.getByText('Add rate limiter to the public API', { exact: true })).toHaveCount(0);
  });
});
