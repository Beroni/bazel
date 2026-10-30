import { expect, Locator, Page } from '@playwright/test';

/**
 * Every UI selector the tests use lives here, so a redesign changes one file.
 *
 * Each one tries the data-testid of the contract (references/selectors.md)
 * first and falls back to role/visible text, which keeps the suite usable
 * against a base from before the testids (E2E_BAZEL_SRC). Where the UIs put an
 * action in different places, the helper below the selectors handles both.
 */
function tid(scope: Page | Locator, id: string, fallback: Locator): Locator {
  return scope.getByTestId(id).or(fallback).first();
}

export const ui = {
  /** Claude quota ("claude session 86% · week 16%"); in the #meter popover. */
  quota: (p: Page) => tid(p, 'quota', p.getByText(/^claude session/)),
  /** Session spend ("3,6M tokens · $4.18"); in the #meter popover. */
  spendTotal: (p: Page) => tid(p, 'spend-total', p.getByText(/^[\d,.]+[kM]? tokens( · \$[\d.]+)?$/)),
  /** Header counter: "5 PRs", "1 of 5 PRs · 1 ticked". */
  prCount: (p: Page) => tid(p, 'pr-count', p.getByText(/^\d+( of \d+)? PRs?( · \d+ ticked)?$/)),
  /** Who the server thinks you are (the gh login). */
  whoami: (p: Page) => tid(p, 'whoami', p.getByTitle(/^config: /)),
  filter: (p: Page) => tid(p, 'pr-filter', p.getByPlaceholder('filter by title, repo or author…')),
  selectAll: (p: Page) => tid(p, 'pr-select-all', p.getByRole('checkbox', { name: 'select all' })),
  hideDrafts: (p: Page) => tid(p, 'pr-hide-drafts', p.getByRole('checkbox', { name: 'hide drafts' })),
  /** "everyone" / "mine only". */
  scope: (p: Page, which: 'all' | 'mine') =>
    p.getByTestId('pr-scope').locator(`[data-scope="${which}"]`)
      .or(p.getByRole('button', { name: which === 'mine' ? 'mine only' : 'everyone' })).first(),
  repoFilter: (p: Page) => tid(p, 'pr-repo-filter', p.getByRole('combobox').filter({ has: p.getByRole('option', { name: /^any repo/ }) })),
  /** Review-state filter: a group of chips; a <select> before #30. Use filterByReview(). */
  reviewFilter: (p: Page) => tid(p, 'pr-review-filter', p.getByRole('combobox').filter({ has: p.getByRole('option', { name: 'not reviewed' }) })),
  /** The PR list row whose title is `title` (the element holding its checkbox). */
  prRow: (p: Page, title: string) =>
    p.getByTestId('pr-row').filter({ hasText: title })
      // master: the row is the element whose own child is the checkbox (a queue
      // card with the same title has none).
      .or(p.getByText(title, { exact: true }).locator('xpath=ancestor::*[./input[@type="checkbox"]][1]')).first(),
  /** A row's review tag: "✓ reviewed 2h" / "✓ 2h" / "⟳ changed since review" / "⟳ changed". */
  reviewTag: (row: Locator) => tid(row, 'pr-review-tag', row.getByText(/^(✓|⟳) /)),
  agentSelect: (p: Page) =>
    tid(p, 'agent-select', p.getByRole('combobox').filter({ has: p.getByRole('option', { name: 'review-fleet' }) })),
  runButton: (p: Page) => tid(p, 'run-review', p.getByRole('button', { name: /^run( \d+)?$/i })),
  savedTab: (p: Page) => tid(p, 'tab-saved', p.getByRole('button', { name: 'saved' })),

  /** The queue card of a PR, by its title. */
  jobCard: (p: Page, title: string) =>
    p.getByTestId('job-card').filter({ hasText: title })
      .or(p.locator('#rail > *').filter({ hasText: title })).first(),
  /** Cancel: on the card (master) or in the job's detail header (redesign). */
  cancelControl: (p: Page, card: Locator) => tid(p, 'job-cancel', card.getByRole('button', { name: 'cancel' })),
  removeButton: (card: Locator) => tid(card, 'job-remove', card.getByRole('button', { name: '✕' })),
  jobPublished: (card: Locator) => tid(card, 'job-published', card.getByText('✓ published')),

  /** The page-wide message bar (errors, warnings). */
  banner: (p: Page) => tid(p, 'banner', p.locator('#banner')),
  /** The ✕ that dismisses an error left in the banner (since the R1 fix). */
  bannerDismiss: (p: Page) => tid(p, 'banner-dismiss', p.getByRole('button', { name: 'dismiss this message' })),
  /** The review pane. */
  viewer: (p: Page) => tid(p, 'review-viewer', p.getByRole('article')),
  findingPicks: (scope: Locator) => scope.getByTestId('finding-pick').or(scope.getByRole('checkbox', { name: 'report' })),
  publishInline: (scope: Locator) => tid(scope, 'publish-inline', scope.getByRole('button', { name: /^(publish inline review|↗ publish)$/i })),
  pasteAsComment: (scope: Locator) => tid(scope, 'paste-comment', scope.getByRole('button', { name: /^(or paste as a comment|as comment)$/i })),
  /** Finding rows of the compact report (redesign c2614d9); none on master. */
  findingToggles: (scope: Locator) => scope.getByTestId('finding-toggle'),
  keepHere: (scope: Locator) => tid(scope, 'keep-here', scope.getByRole('button', { name: /^keep it here$/i })),
  alreadyPublished: (scope: Locator) => tid(scope, 'already-published', scope.getByText('✓ already on the PR')),
  /** The part of a split report that stays in Bazel, and the part that goes. */
  reviewContext: (scope: Locator) => tid(scope, 'review-context', scope.getByText('stays in Bazel — not published')),
  notAReview: (scope: Locator, agent: string) => scope.getByText(new RegExp(`what "${agent}" produces stays in Bazel`)),
};

/**
 * Opens every collapsed finding. The redesign shows each finding as a compact
 * row with its body closed (only the first starts open); master shows every
 * body. Call it before asserting on text inside a finding's body.
 */
export async function expandFindings(viewer: Locator): Promise<void> {
  const toggles = ui.findingToggles(viewer);
  const n = await toggles.count();
  for (let i = 0; i < n; i++) {
    const t = toggles.nth(i);
    if ((await t.getAttribute('aria-expanded')) === 'false') await t.click();
  }
}

/**
 * The redesign keeps the PR filters (scope, drafts, owner, repo) in a popover
 * behind `pr-filter-menu`; master shows them inline. Opens it when needed.
 */
export async function openFilters(page: Page): Promise<void> {
  const menu = page.getByTestId('pr-filter-menu');
  if ((await menu.count()) && (await menu.getAttribute('aria-expanded')) !== 'true') await menu.click();
}

/**
 * The redesign keeps count, spend, quota and login in a popover that shows on
 * hover/focus of #meter; master shows them in the bar. Call before toBeVisible.
 */
export async function showMeters(page: Page): Promise<void> {
  const meter = page.locator('#meter');
  if (await meter.count()) await meter.hover();
}

/** Ticks the PRs matching `text` through the filter + "select all". */
export async function tickByFilter(page: Page, text: string): Promise<void> {
  await ui.filter(page).fill(text);
  await ui.selectAll(page).check();
  // The box being checked is not enough (BUG-QA-06): the counter must say so.
  await expect(ui.prCount(page)).toHaveText(/ · \d+ ticked$/);
}

/** Opens a job in the review pane (click on master, Enter-able card on redesign). */
export async function openJob(page: Page, title: string): Promise<Locator> {
  await ui.jobCard(page, title).click();
  return ui.viewer(page);
}

/** Cancels a job from wherever the UI puts the button. */
export async function cancelJob(page: Page, title: string): Promise<void> {
  const card = ui.jobCard(page, title);
  await card.click();
  await ui.cancelControl(page, card).click();
}

/** Filters the list by review state: all | none | reviewed | changed. */
export async function filterByReview(page: Page, value: 'all' | 'none' | 'reviewed' | 'changed'): Promise<void> {
  const f = ui.reviewFilter(page);
  if ((await f.evaluate((el) => el.tagName)) === 'SELECT') await f.selectOption(value);
  else await f.locator(`[data-review="${value}"]`).click();
}

/** Picks the option whose label matches (labels differ across UIs; values do not always exist). */
export async function selectByLabel(select: Locator, label: RegExp): Promise<void> {
  const text = (await select.getByRole('option', { name: label }).first().textContent()) ?? '';
  await select.selectOption({ label: text.trim() });
}
