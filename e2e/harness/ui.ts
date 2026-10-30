import { expect, Locator, Page } from '@playwright/test';

/**
 * Every UI selector the tests use lives here, so a redesign changes one file.
 *
 * This is the contract with the page (references/selectors.md): a selector goes
 * by data-testid, with no fallback. A testid that goes away fails here, loudly,
 * instead of a test passing on some other element.
 */
export const ui = {
  /** Claude quota ("claude session 86% · week 16%"), in the #meter popover. */
  quota: (p: Page) => p.getByTestId('quota'),
  /** Session spend ("3,6M tokens · $4.18"), in the #meter popover. */
  spendTotal: (p: Page) => p.getByTestId('spend-total'),
  /** Header counter: "5 PRs", "1 of 5 PRs · 1 ticked". */
  prCount: (p: Page) => p.getByTestId('pr-count'),
  /** Who the server thinks you are (the gh login). */
  whoami: (p: Page) => p.getByTestId('whoami'),
  filter: (p: Page) => p.getByTestId('pr-filter'),
  selectAll: (p: Page) => p.getByTestId('pr-select-all'),
  hideDrafts: (p: Page) => p.getByTestId('pr-hide-drafts'),
  /** "everyone" / "mine only". */
  scope: (p: Page, which: 'all' | 'mine') => p.getByTestId('pr-scope').locator(`[data-scope="${which}"]`),
  repoFilter: (p: Page) => p.getByTestId('pr-repo-filter'),
  /** The chips of the review state: all | none | reviewed | changed. */
  reviewChip: (p: Page, value: string) => p.getByTestId('pr-review-filter').locator(`[data-review="${value}"]`),
  /** The PR list row whose title is `title`. */
  prRow: (p: Page, title: string) => p.getByTestId('pr-row').filter({ hasText: title }),
  /** A row's review tag: "reviewed 2h", "reviewed now", "changed", "reviewing". */
  reviewTag: (row: Locator) => row.getByTestId('pr-review-tag'),
  agentSelect: (p: Page) => p.getByTestId('agent-select'),
  runButton: (p: Page) => p.getByTestId('run-review'),
  savedTab: (p: Page) => p.getByTestId('tab-saved'),

  /** The queue card of a PR, by its title. */
  jobCard: (p: Page, title: string) => p.getByTestId('job-card').filter({ hasText: title }),
  /** In the header of the job's detail. */
  cancelControl: (p: Page) => p.getByTestId('job-cancel'),
  removeButton: (card: Locator) => card.getByTestId('job-remove'),
  jobPublished: (card: Locator) => card.getByTestId('job-published'),

  /** The page-wide message bar (errors, warnings). */
  banner: (p: Page) => p.getByTestId('banner'),
  /** The ✕ that dismisses an error left in the banner. */
  bannerDismiss: (p: Page) => p.getByRole('button', { name: 'dismiss this message' }),
  /** The scroller of a job's detail. By class until it has a testid (`job-body`). */
  jobBody: (p: Page) => p.locator('#viewer .job-body'),
  /** The bar with the publish buttons. By class until it has a testid (`viewer-actions`). */
  actionBar: (p: Page) => p.locator('#viewer .viewer-actions'),
  findingPick: (scope: Locator, text: string) => scope.getByTestId('finding-row').filter({ hasText: text }).getByTestId('finding-pick'),
  /** The review pane. */
  viewer: (p: Page) => p.getByTestId('review-viewer'),
  findingPicks: (scope: Locator) => scope.getByTestId('finding-pick'),
  publishInline: (scope: Locator) => scope.getByTestId('publish-inline'),
  pasteAsComment: (scope: Locator) => scope.getByTestId('paste-comment'),
  findingToggles: (scope: Locator) => scope.getByTestId('finding-toggle'),
  keepHere: (scope: Locator) => scope.getByTestId('keep-here'),
  alreadyPublished: (scope: Locator) => scope.getByTestId('already-published'),
  /** The part of a split report that stays in Bazel. */
  reviewContext: (scope: Locator) => scope.getByTestId('review-context'),
  notAReview: (scope: Locator, agent: string) => scope.getByText(new RegExp(`what "${agent}" produces stays in Bazel`)),
};

/**
 * Opens every collapsed finding: each is a compact row and only the first starts
 * open. Call it before asserting on text inside a finding's body.
 */
export async function expandFindings(viewer: Locator): Promise<void> {
  const toggles = ui.findingToggles(viewer);
  const n = await toggles.count();
  for (let i = 0; i < n; i++) {
    const t = toggles.nth(i);
    if ((await t.getAttribute('aria-expanded')) === 'false') await t.click();
  }
}

/** The PR filters (scope, drafts, owner, repo) live in a popover behind `pr-filter-menu`. */
export async function openFilters(page: Page): Promise<void> {
  const menu = page.getByTestId('pr-filter-menu');
  if ((await menu.getAttribute('aria-expanded')) !== 'true') await menu.click();
}

/** Count, spend, quota and login show on hover or focus of #meter. Call before toBeVisible. */
export async function showMeters(page: Page): Promise<void> {
  await page.locator('#meter').hover();
}

/** Every POST the page sends from now on, by path. */
export function recordPosts(page: Page): string[] {
  const posts: string[] = [];
  page.on('request', (r) => {
    if (r.method() === 'POST') posts.push(new URL(r.url()).pathname);
  });
  return posts;
}

/**
 * Returns when whatever the page has sent so far has shown up in recordPosts. A
 * dismissed confirm runs on in the same task, so its fetch goes out before this
 * one, and requests reach the recorder in order: no time to guess.
 */
export async function settled(page: Page): Promise<void> {
  await Promise.all([
    page.waitForRequest('**/api/state?settled'),
    page.evaluate(() => fetch('/api/state?settled').then((r) => r.text())),
  ]);
}

/** Opens one of the tabs of a job's detail. */
export async function jobTab(page: Page, name: 'Overview' | 'History' | 'Live log' | 'Report'): Promise<void> {
  await page.getByRole('tab', { name }).click();
}

/** Ticks the PRs matching `text` through the filter + "select all". */
export async function tickByFilter(page: Page, text: string): Promise<void> {
  await ui.filter(page).fill(text);
  await ui.selectAll(page).check();
  // The box being checked is not enough (BUG-QA-06): the counter must say so.
  await expect(ui.prCount(page)).toHaveText(/ · \d+ ticked$/);
}

/** Opens the newest job of a PR in the review pane: the queue lists newest first. */
export async function openJob(page: Page, title: string): Promise<Locator> {
  await ui.jobCard(page, title).first().click();
  return ui.viewer(page);
}

/** Cancels a job: the button is in the header of its detail. */
export async function cancelJob(page: Page, title: string): Promise<void> {
  await ui.jobCard(page, title).first().click();
  await ui.cancelControl(page).click();
}

/** Filters the list by review state. */
export async function filterByReview(page: Page, value: 'all' | 'none' | 'reviewed' | 'changed'): Promise<void> {
  await ui.reviewChip(page, value).click();
}
