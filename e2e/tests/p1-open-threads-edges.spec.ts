import fs from 'node:fs';
import { test, expect, knownBug } from '../harness/fixtures';
import { Bazel } from '../harness/bazel';
import { tickByFilter, ui } from '../harness/ui';

// feat/open-threads, beyond p1-open-threads: whose thread counts, many PRs in
// batches, what a hostile thread can put on the page, failures and the backoff,
// several PRs ticked, keyboard and axe. Everything here is a read.
const AGENTS = [{ name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } }];
const RATE = 'Add rate limiter to the public API';
const NIL = 'Fix nil pointer on empty config';
const said = (login: string, body: string, n: number) =>
  ({ author: { login, __typename: 'User' }, authorAssociation: 'MEMBER', body, url: `https://github.com/acme/api-core/pull/101#discussion_r${n}`, createdAt: '2026-11-01T10:00:00Z' });
const thread = (path: string, line: number | null, comments: any[], extra: Record<string, unknown> = {}) =>
  ({ isResolved: false, isOutdated: false, path, line, resolvedBy: null, comments: { totalCount: comments.length, nodes: comments }, ...extra });
const conv = (...threads: any[]) => ({ reviewThreads: { nodes: threads } });
const reads = (bz: Bazel) => bz.ghCalls().filter((c) => c.argv.some((a) => a.startsWith('query=query bazelConversation(')));
const writes = (bz: Bazel) => bz.ghCalls().filter((c) => (c.argv[0] === 'pr' && c.argv[1] === 'comment')
  || (c.argv[0] === 'api' && c.argv.some((a) => a === 'POST' || a === '-X' || a === '--method' || a.includes('mutation'))));
const tag = (app: any, title: string) => ui.prRow(app, title).getByTestId('pr-open-threads');
const threads = async (bz: Bazel, refresh = false) => (await bz.api('GET', `/api/open-threads${refresh ? '?refresh=1' : ''}`)).json;

test.describe('P1 · your open threads, edges', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });
  test.afterEach(async ({ bazel }) => { expect(writes(bazel)).toEqual([]); });

  test('whose thread it is: yours whatever the case; one you only answered is not', async ({ bazel }) => {
    bazel.updatePR('acme/api-core#101', { conversation: conv(
      thread('a.go', 1, [said('E2E-User', 'mine, other case', 1)]),
      thread('b.go', 2, [said('bob', 'bob asks', 2), said('e2e-user', 'I answer', 3)]),
      thread('c.go', 3, [said('e2e-user', 'resolved', 4)], { isResolved: true }),
      thread('d.go', null, [said('e2e-user', 'outdated', 5)], { isOutdated: true, originalLine: 40 }),
    ) });
    await bazel.api('GET', '/api/prs?refresh=1');
    const t = (await threads(bazel, true)).threads['acme/api-core#101'];
    expect(t.map((x: any) => `${x.path}:${x.line ?? ''}${x.outdated ? ' outdated' : ''}`)).toEqual(['a.go:1', 'd.go:40 outdated']);
  });

  test('threads on several PRs: each row its tag, and the reminder lists every ticked PR', async ({ app, bazel }) => {
    bazel.updatePR('acme/api-core#101', { conversation: conv(thread('a.go', 1, [said('e2e-user', 'q', 1)]), thread('b.go', 2, [said('e2e-user', 'q', 2)])) });
    bazel.updatePR('acme/api-core#102', { conversation: conv(thread('c.go', 3, [said('e2e-user', 'q', 3)])) });
    await app.getByTestId('refresh').click();
    await expect(tag(app, RATE)).toHaveText('💬 2 open');
    await expect(tag(app, NIL)).toHaveText('💬 1 open');
    await tickByFilter(app, 'api-core');
    const notes = app.getByTestId('threads-note-pr');
    await expect(notes).toHaveCount(2);
    await expect(app.locator('[data-testid=threads-note-pr][data-pr-key="acme/api-core#102"]')).toContainText('1 open thread on this PR — c.go:3');
  });

  test('more than ten PRs: read in batches of ten, every one tagged', async ({ app, bazel }) => {
    for (let i = 0; i < 24; i++) {
      bazel.addPR({ repo: 'acme/api-core', number: 600 + i, title: `Many ${i}`, headRefName: `m${i}`, author: { login: 'zoe' },
        conversation: conv(thread(`f${i}.go`, i + 1, [said('e2e-user', 'q', i)])) });
    }
    await bazel.api('GET', '/api/prs?refresh=1'); // 29 PRs listed
    const before = reads(bazel).length;
    const t = await threads(bazel, true);
    expect(reads(bazel).length - before).toBe(3);
    expect(Object.keys(t.threads)).toHaveLength(24);
    await app.reload();
    await expect(app.getByTestId('pr-open-threads')).toHaveCount(24);
  });

  test('a hostile path, body or markdown stays text in the tag, the reminder and the Overview', async ({ app, bazel }) => {
    const evil = '<img src=x onerror="window.__xss=1"><b>bold</b> [link](javascript:alert(1)) **x**';
    const path = '<img src=x onerror="window.__xss=2">.go';
    bazel.updatePR('acme/api-core#101', { conversation: conv(thread(path, 3, [said('e2e-user', 'q', 1), said('alice', evil, 2)])) });
    await app.getByTestId('refresh').click();
    await expect(tag(app, RATE)).toHaveAttribute('title', `your threads still open:\n${path}:3 (author replied)`);
    await tickByFilter(app, RATE);
    await expect(app.getByTestId('threads-note')).toContainText(`${path}:3`);
    await ui.prRow(app, RATE).locator('.title').click();
    const item = app.getByTestId('pr-open-thread');
    await expect(item).toContainText(`@alice: ${evil}`);
    expect(await app.locator('#threads-note img, [data-testid=pr-open-threads-list] img, [data-testid=pr-open-threads-list] b').count()).toBe(0);
    expect(await app.evaluate(() => (window as any).__xss)).toBeUndefined();
  });

  test('↻ in the backoff of a failed read asks gh nothing', async ({ app, bazel }) => {
    bazel.setGh({ fail: { conversation: 'HTTP 502: Bad Gateway' } });
    await app.getByTestId('refresh').click();
    await expect.poll(() => reads(bazel).filter((c) => c.exit !== 0).length).toBe(1);
    bazel.setGh({});
    const n = reads(bazel).length;
    await app.getByTestId('refresh').click();
    const r = await threads(bazel, true);
    expect(r.error).toContain('open threads could not be read');
    expect(reads(bazel).length).toBe(n);
  });

  // BUG-QA-40: a read that worked shows the tag; GitHub then fails one ↻, and the
  // page drops every tag and reminder for the whole backoff, though what it had
  // was right a minute ago. The server answers 200 with {threads: {}, error},
  // and loadOpenThreads takes the empty map. Expected: the last good tags stay
  // (stale) until a read succeeds, as Needs you keeps its last list.
  test('a failed ↻ keeps the tags the last good read showed', async ({ app, bazel }) => {
    knownBug('BUG-QA-40', 'a failed refresh wipes the open-thread tags');
    bazel.updatePR('acme/api-core#101', { conversation: conv(thread('a.go', 12, [said('e2e-user', 'why?', 1)])) });
    await app.getByTestId('refresh').click();
    await expect(tag(app, RATE)).toHaveText('💬 1 open');
    bazel.setGh({ fail: { conversation: 'HTTP 502: Bad Gateway' } });
    await app.getByTestId('refresh').click();
    await expect.poll(() => reads(bazel).filter((c) => c.exit !== 0).length).toBe(1);
    await expect(tag(app, RATE)).toHaveText('💬 1 open');
  });

  test('keyboard reaches the reminder\'s links and the Overview\'s, and axe is clean on both', async ({ app, bazel }) => {
    bazel.updatePR('acme/api-core#101', { conversation: conv(
      thread('a.go', 1, [said('e2e-user', 'q', 1), said('alice', 'a', 2)]),
      thread('b.go', 2, [said('e2e-user', 'q', 3)], { isOutdated: true }),
    ) });
    await app.getByTestId('refresh').click();
    await tickByFilter(app, RATE);
    const link = app.getByTestId('threads-note').getByRole('link', { name: 'a.go:1' });
    await link.focus();
    await expect(link).toBeFocused();
    await app.keyboard.press('Tab');
    await expect(app.getByTestId('threads-note').getByRole('link', { name: 'b.go:2' })).toBeFocused();
    await ui.prRow(app, RATE).locator('.title').click();
    await expect(app.getByTestId('pr-open-thread')).toHaveCount(2);
    await expect(app.getByTestId('pr-open-threads-list').getByRole('link')).toHaveCount(2);
    await app.addScriptTag({ content: fs.readFileSync(require.resolve('axe-core/axe.min.js'), 'utf8') });
    for (const sel of ['#threads-note', '[data-testid=pr-open-threads-list]']) {
      const r: any = await app.evaluate(async (s) => (window as any).axe.run(document.querySelector(s), {
        runOnly: ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa', 'best-practice'],
      }), sel);
      expect(r.violations.map((v: any) => v.id), sel).toEqual([]);
    }
  });
});
