import fs from 'node:fs';
import { Page } from '@playwright/test';
import { test, expect, knownBug } from '../harness/fixtures';
import { jobTab, openFilters, openJob } from '../harness/ui';

// axe-core over the main screens: WCAG 2.x A/AA plus best practices.
const AXE = fs.readFileSync(require.resolve('axe-core/axe.min.js'), 'utf8');
const T = 'Add rate limiter to the public API';
const AGENTS = [{ name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } }];

async function violations(page: Page): Promise<string[]> {
  await page.addScriptTag({ content: AXE });
  const r: any = await page.evaluate(async () => (window as any).axe.run(document, {
    runOnly: ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa', 'best-practice'],
  }));
  return r.violations.map((v: any) => `${v.impact}:${v.id} ${v.nodes[0].target.join(' ')}`);
}

test.describe('P2 · accessibility', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  // BUG-QA-16: `#agent`, `#repo-filter` and `#owner-filter` are named only by
  // `title` (serious); the dim text on the empty dashboard is 4.25:1 (serious,
  // AA asks 4.5); no page has an <h1>; the Config page has no <main>.
  // Expected: axe clean on these screens.
  test('the dashboard and its filters are clean', async ({ app }) => {
    knownBug('BUG-QA-16', 'unnamed selects, contrast, landmarks');
    await openFilters(app);
    expect(await violations(app)).toEqual([]);
  });

  test('the config page is clean', async ({ app }) => {
    knownBug('BUG-QA-16', 'unnamed selects, contrast, landmarks');
    await app.getByTestId('nav-config').click();
    await expect(app.getByTestId('repo-input')).toBeVisible();
    expect(await violations(app)).toEqual([]);
  });

  test('nothing critical on a job, in any tab', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    await app.reload();
    await openJob(app, T);
    for (const tab of ['Overview', 'Report', 'Live log', 'History'] as const) {
      await jobTab(app, tab);
      expect((await violations(app)).filter((v) => v.startsWith('critical')), tab).toEqual([]);
    }
  });
});
