import { Locator, Page } from '@playwright/test';

/**
 * Every UI selector the tests use lives here, so the redesign changes one file.
 *
 * Today they go by role and visible text. Each one names the data-testid that
 * PLAN.md asks the UI for; `tid()` prefers it once it exists in the page and
 * falls back to the role/text locator until then.
 */
function tid(page: Page, id: string, fallback: Locator): Locator {
  return page.getByTestId(id).or(fallback).first();
}

export const ui = {
  /** Header counter: "5 PRs", "1 of 5 PRs · 1 ticked". */
  prCount: (p: Page) => tid(p, 'pr-count', p.getByText(/^\d+( of \d+)? PRs?( · \d+ ticked)?$/)),
  /** Who the server thinks you are (the gh login). */
  whoami: (p: Page) => tid(p, 'whoami', p.getByTitle(/^config: /)),
  filter: (p: Page) => tid(p, 'pr-filter', p.getByPlaceholder('filter by title, repo or author…')),
  selectAll: (p: Page) => tid(p, 'pr-select-all', p.getByRole('checkbox', { name: 'select all' })),
  /** The PR list row whose title is `title`. */
  prRow: (p: Page, title: string) => p.getByTestId('pr-row').filter({ hasText: title }).or(p.getByText(title, { exact: true })).first(),
  agentSelect: (p: Page) =>
    tid(p, 'agent-select', p.getByRole('combobox').filter({ has: p.getByRole('option', { name: 'review-fleet' }) })),
  runButton: (p: Page) => tid(p, 'run-review', p.getByRole('button', { name: /^run( \d+)?$/ })),
  /** The queue card of a PR, by its title. */
  jobCard: (p: Page, title: string) =>
    p.getByTestId('job-card').filter({ hasText: title }).or(
      p.locator('#rail > *').filter({ hasText: title }),
    ).first(),
  cancelButton: (scope: Locator) => scope.getByRole('button', { name: 'cancel' }),
  /** The review pane. */
  viewer: (p: Page) => tid(p, 'review-viewer', p.getByRole('article')),
  findingPicks: (scope: Locator) => scope.getByRole('checkbox', { name: 'report' }),
  publishInline: (scope: Locator) => scope.getByRole('button', { name: 'publish inline review' }),
  pasteAsComment: (scope: Locator) => scope.getByRole('button', { name: 'or paste as a comment' }),
};

/** Ticks the PRs matching `text` through the filter + "select all". */
export async function tickByFilter(page: Page, text: string): Promise<void> {
  await ui.filter(page).fill(text);
  await ui.selectAll(page).check();
}
