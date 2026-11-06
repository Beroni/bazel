import fs from 'node:fs';
import { test, expect, knownBug } from '../harness/fixtures';
import { Bazel } from '../harness/bazel';
import { asked, ME, node, pushed, review } from '../harness/needs';
import { ui } from '../harness/ui';

// Exploratory session of the Needs you page (feat/needs-you e3f15a2). What held
// is pinned here as it is; what did not is a knownBug with its id.
const AGENTS = [{ name: 'slow-lens', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-slow' } }];
const ONE = { requested: [asked('acme/api-core', 101, 'Add rate limiter to the public API')] };

async function open(app: any, bazel: Bazel) {
  expect((await bazel.api('GET', '/api/needs?refresh=1')).status).toBe(200);
  await app.goto(bazel.url + '/#/needs');
  await expect(app.getByTestId('needs-page')).toBeVisible();
}
const many = (n: number, repo: string, from: number) => Array.from({ length: n }, (_, i) => asked(repo, from + i, `Generated PR number ${i}`, 'zoe', i % 40));

test.describe('P2 · needs you, what held', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  test('more than 50 a search: every row it got, and a note that GitHub has more', async ({ app, bazel }) => {
    bazel.setNeeds({ requested: many(50, 'acme/api-core', 1000), reviewed: many(50, 'acme/web-app', 2000).map((n: any) => ({ ...n, requests: { nodes: [] }, reviews: { nodes: [review(ME, 'CHANGES_REQUESTED', 1)] }, commits: pushed(3) })) }, { requested: 73, reviewed: 120 });
    await open(app, bazel);
    await expect(app.getByTestId('needs-row')).toHaveCount(100);
    await expect(app.getByTestId('needs-note')).toContainText('GitHub has more than these');
    await expect(app.getByTestId('needs-count')).toHaveText('100');
  });

  test('a PR in two searches is one row', async ({ app, bazel }) => {
    const both = asked('acme/api-core', 101, 'In two searches');
    bazel.setNeeds({ requested: [both], reviewed: [{ ...both, reviews: { nodes: [review(ME, 'CHANGES_REQUESTED', 1)] } }] });
    await open(app, bazel);
    await expect(app.locator('[data-testid=needs-row][data-pr-key="acme/api-core#101"]')).toHaveCount(1);
  });

  for (const [w, h] of [[1000, 800], [800, 700]]) {
    test(`at ${w}x${h} long and right-to-left titles stay on their line, and Review in the row`, async ({ app, bazel }) => {
      bazel.setNeeds({
        requested: [asked('acme/api-core', 101, 'A very long title '.repeat(12) + 'END'), asked('acme/api-core', 102, 'إضافة محدد معدل لواجهة البرمجة العامة', 'عمر')],
        authored: [node('acme/web-app', 8, 'x'.repeat(200), ME)],
      });
      await app.setViewportSize({ width: w, height: h });
      await open(app, bazel);
      const r = await app.evaluate(() => ({
        sideways: document.scrollingElement!.scrollWidth - document.scrollingElement!.clientWidth,
        rows: [...document.querySelectorAll('[data-testid=needs-row]')].map((row) => {
          const b = row.getBoundingClientRect();
          const btn = row.querySelector('[data-testid=needs-review]')?.getBoundingClientRect();
          const t = row.querySelector('[role=button]') as HTMLElement;
          return { inside: !btn || btn.right <= b.right, oneLine: t.offsetHeight < 24, dir: getComputedStyle(t).direction };
        }),
      }));
      expect(r.sideways).toBeLessThanOrEqual(0);
      expect(r.rows.every((x) => x.inside && x.oneLine)).toBe(true);
      expect(r.rows[1].dir).toBe('rtl');
    });
  }

  test('fast toggles of the scope end on the last one chosen', async ({ app, bazel }) => {
    bazel.setNeeds({ requested: [asked('acme/api-core', 101, 'Watched one'), asked('elsewhere/lib', 5, 'Not watched')] });
    await open(app, bazel);
    for (let i = 0; i < 9; i++) await app.getByTestId(i % 2 ? 'needs-scope-watched' : 'needs-scope-all').click();
    await expect(app.getByTestId('needs-scope-all')).toHaveAttribute('aria-pressed', 'true');
    await expect(app.getByTestId('needs-row')).toHaveCount(2);
  });

  test('the keyboard reaches the scope, each title and each Review, in that order', async ({ app, bazel }) => {
    bazel.setNeeds({ ...ONE, authored: [node('acme/api-core', 102, 'Fix nil pointer on empty config', ME, { commits: pushed(3, 'FAILURE') })] });
    await open(app, bazel);
    await app.getByTestId('refresh').focus();
    const seen: string[] = [];
    for (let i = 0; i < 5; i++) {
      await app.keyboard.press('Tab');
      seen.push(await app.evaluate(() => (document.activeElement as HTMLElement).dataset.testid || (document.activeElement as HTMLElement).getAttribute('aria-label') || ''));
    }
    expect(seen).toEqual(['needs-scope-watched', 'needs-scope-all', 'open acme/api-core#101: Add rate limiter to the public API', 'needs-review', 'open acme/api-core#102: Fix nil pointer on empty config']);
  });

});

test.describe('P2 · needs you, a first read that fails', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS }, gh: { fail: { needs: 'HTTP 502: Bad Gateway' } } } });

  // ↻ during the server's backoff (30s, doubling) reads nothing: the error stays
  // as it was, which is the design (internal/server/needs.go), not a bug.
  test('says so on the page, with nothing else in its place', async ({ app, bazel }) => {
    await app.goto(bazel.url + '/#/needs');
    await expect(app.getByTestId('needs-error')).toContainText('Could not read GitHub: HTTP 502');
    await expect(app.getByTestId('needs-row')).toHaveCount(0);
    await expect(app.getByTestId('needs-count')).toBeHidden();
  });
});

test.describe('P3 · needs you, bugs', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  // BUG-QA-29, fixed in 771e53d: renderJobs wrote the tab title every second
  // while a job ran, whatever page was open, so "(1) Bazel · needs you" became
  // "(1) Bazel".
  test('the tab title of Needs you survives a running job', async ({ app, bazel }) => {
    bazel.setNeeds(ONE);
    await open(app, bazel);
    await expect(app).toHaveTitle('(1) Bazel · needs you');
    const id = await bazel.enqueue('acme/web-app#7', 'slow-lens');
    await bazel.waitJob(id, (j) => j.state === 'running');
    await expect.poll(() => app.evaluate(() => new Promise((r) => setTimeout(() => r(document.title), 1500)))).toBe('(1) Bazel · needs you');
    bazel.release('release-slow');
  });

  // BUG-QA-30: the counter by "Needs you" counts the scope chosen last on the
  // page: choose "all repos" and it grows, on every page, until a reload puts it
  // back on the watched ones. Expected: one meaning, the watched repos (or say
  // which).
  test('the counter does not change with the scope chosen on the page', async ({ app, bazel }) => {
    knownBug('BUG-QA-30', 'the counter follows the last scope chosen');
    bazel.setNeeds({ requested: [asked('acme/api-core', 101, 'Watched one'), asked('elsewhere/lib', 5, 'Not watched')] });
    await open(app, bazel);
    await expect(app.getByTestId('needs-count')).toHaveText('1');
    await app.getByTestId('needs-scope-all').click();
    await expect(app.getByTestId('needs-row')).toHaveCount(2);
    await app.getByTestId('nav-dashboard').click();
    await expect(app.getByTestId('needs-count')).toHaveText('1');
  });

  // BUG-QA-31: Enter on a row's title opens the PR on the dashboard and the
  // focus falls to <body>: a keyboard user starts over from the top. Expected:
  // focus on the PR page that opened (or its heading).
  test('opening a PR from Needs you by keyboard keeps the focus on the page', async ({ app, bazel }) => {
    knownBug('BUG-QA-31', 'focus lost after opening a PR from Needs you');
    bazel.setNeeds(ONE);
    await open(app, bazel);
    await app.getByRole('button', { name: /open acme\/api-core#101/ }).focus();
    await app.keyboard.press('Enter');
    await expect(app.getByTestId('pr-page')).toHaveAttribute('data-pr-key', 'acme/api-core#101');
    await expect(app.getByTestId('pr-page')).toHaveAttribute('data-loaded', 'true');
    expect(await app.evaluate(() => document.activeElement !== document.body)).toBe(true);
  });

  // BUG-QA-32: POST /api/reviews refuses with {"errors":[…]} and api() reads only
  // body.error, so the banner says "400 Bad Request" and drops the reason (here
  // "Could not resolve to a PullRequest"). Same from the dashboard's Run.
  // Expected: the server's reason on screen.
  test('a refused review says why', async ({ app, bazel }) => {
    knownBug('BUG-QA-32', 'the reason of a 400 from /api/reviews is dropped');
    bazel.setNeeds({ requested: [asked('elsewhere/lib', 5, 'A PR gh cannot read')] });
    await open(app, bazel);
    await app.getByTestId('needs-scope-all').click();
    await app.getByTestId('needs-review').click();
    await expect(ui.banner(app)).toBeVisible();
    await expect(ui.banner(app)).toContainText('Could not resolve');
  });

  // BUG-QA-33: the page has an h2 ("Needs you") and its groups are h4: axe
  // heading-order (moderate). Expected: h3, or the levels of the dashboard.
  test('axe is clean on Needs you', async ({ app, bazel }) => {
    knownBug('BUG-QA-33', 'heading levels skip on Needs you');
    bazel.setNeeds({ ...ONE, reviewed: [node('acme/web-app', 7, 'Dark mode', 'carol', { reviews: { nodes: [review(ME, 'CHANGES_REQUESTED', 1)] }, commits: pushed(3) })] });
    await open(app, bazel);
    await app.addScriptTag({ content: fs.readFileSync(require.resolve('axe-core/axe.min.js'), 'utf8') });
    const r: any = await app.evaluate(async () => (window as any).axe.run(document.querySelector('#page-needs'), {
      runOnly: ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa', 'best-practice'],
    }));
    expect(r.violations.map((v: any) => v.id)).toEqual([]);
  });
});
