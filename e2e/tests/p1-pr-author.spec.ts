import { test, expect } from '../harness/fixtures';
import { ui } from '../harness/ui';

const LONG = {
  repo: 'acme/api-core',
  number: 150,
  title: 'chore: a PR whose author has a very long login',
  url: 'https://github.com/acme/api-core/pull/150',
  headRefName: 'chore/long',
  headRefOid: '1111111111111111111111111111111111111111',
  author: { login: 'someone-with-a-really-long-github-login-name' },
};

test.describe('P1 · PR author in the list', () => {
  test('the second line says repo #n · @author, and yours stands out', async ({ app }) => {
    const row = ui.prRow(app, 'Dark mode');
    await expect(row.locator('.pr-sub')).toContainText(/^web-app #7 · @\S+/);
    const mine = app.getByTestId('pr-author').filter({ hasText: '@e2e-user' }).first();
    await expect(mine).toHaveAttribute('data-mine', 'true');
    await expect(app.getByTestId('pr-author').filter({ hasText: '@bob' }).first()).toHaveAttribute('data-mine', 'false');
  });

  test('the search matches the author', async ({ app }) => {
    await ui.filter(app).fill('bob');
    await expect(ui.prCount(app)).toHaveText('1 of 5 PRs');
    await expect(app.getByTestId('pr-author')).toHaveText(['@bob']);
  });

  test('at 1000 px a long author is cut and the state tag stays', async ({ app, bazel }) => {
    bazel.addPR(LONG);
    await bazel.api('GET', '/api/prs?refresh=1');
    const id = await bazel.enqueue('acme/api-core#150', 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    await app.setViewportSize({ width: 1000, height: 760 });
    await app.reload();
    const sub = ui.prRow(app, 'very long login').locator('.pr-sub');
    await expect(sub.getByTestId('pr-review-tag')).toBeVisible();
    const box = await sub.evaluate((s) => {
      const r = s.getBoundingClientRect();
      const tag = s.querySelector('.tag')!.getBoundingClientRect();
      const who = s.querySelector('.author') as HTMLElement;
      const repo = s.querySelector('.repo') as HTMLElement;
      return {
        tagIn: tag.right <= r.right + 1 && tag.width > 0,
        cut: who.scrollWidth > who.clientWidth,
        repoWhole: repo.scrollWidth <= repo.clientWidth,
        over: s.scrollWidth > s.clientWidth + 1,
      };
    });
    expect(box).toEqual({ tagIn: true, cut: true, repoWhole: true, over: false });
  });
});
