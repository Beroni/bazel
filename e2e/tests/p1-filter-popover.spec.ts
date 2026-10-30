import { test, expect } from '../harness/fixtures';
import { openFilters, selectByLabel, ui } from '../harness/ui';

// Redesign sidebar (79f3807): scope, drafts, owner and repo live in a popover.
// Closing it must not drop what was chosen, and the menu says filters are on.
test.describe('P1 · filter popover keeps its filters', () => {
  test.use({ bazelOptions: { config: { repos: ['acme/api-core', 'acme/web-app', 'globex/site'] } } });

  test('mine only, hide drafts, owner and repo survive closing and reopening the popover', async ({ app, bazel }) => {
    test.skip((await app.getByTestId('pr-filter-menu').count()) === 0, 'this base has no filter popover');
    bazel.addPR({ repo: 'globex/site', number: 3, title: 'Globex landing page', headRefName: 'feat/landing', headRefOid: 'gggg003', author: { login: 'dave' } });
    await bazel.api('GET', '/api/prs?refresh=1');
    await app.reload();
    await expect(ui.prCount(app)).toHaveText('6 PRs');
    const menu = app.getByTestId('pr-filter-menu');
    const pop = app.getByTestId('pr-filter-pop');
    const owner = app.getByTestId('pr-owner-filter');

    await openFilters(app);
    await ui.scope(app, 'mine').click();
    await expect(ui.prCount(app)).toHaveText('2 of 6 PRs');
    await app.keyboard.press('Escape');
    await expect(pop).toBeHidden();
    await expect(ui.prCount(app)).toHaveText('2 of 6 PRs');
    await expect(menu).toHaveAttribute('title', /filters on/);

    await openFilters(app);
    await expect(ui.scope(app, 'mine')).toHaveAttribute('aria-pressed', 'true');
    await ui.scope(app, 'all').click();
    await ui.hideDrafts(app).check();
    await selectByLabel(owner, /^globex/);
    await expect(ui.prCount(app)).toHaveText('1 of 6 PRs');
    await app.mouse.click(5, 5); // a click outside closes it
    await expect(pop).toBeHidden();
    await expect(ui.prCount(app)).toHaveText('1 of 6 PRs');

    await openFilters(app);
    await expect(ui.hideDrafts(app)).toBeChecked();
    await expect(owner.locator('option:checked')).toHaveText(/^globex/);
    await selectByLabel(owner, /^any owner|^everyone|^all/i);
    await selectByLabel(ui.repoFilter(app), /web-app \(2\)$/);
    await expect(ui.prCount(app)).toHaveText('2 of 6 PRs');
    await app.keyboard.press('Escape');
    await openFilters(app);
    await expect(ui.repoFilter(app).locator('option:checked')).toHaveText(/web-app \(2\)$/);
    await expect(ui.hideDrafts(app)).toBeChecked();
  });
});
