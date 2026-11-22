import { test, expect } from '../harness/fixtures';
import { Bazel } from '../harness/bazel';
import { tickByFilter, ui } from '../harness/ui';

// The threads you opened on a PR that nobody resolved yet: a tag in the list, a
// reminder when you tick the PR or hit Review (it never stops either), and a
// section on the PR's page. Read from GitHub in one batched, read-only call.
const AGENTS = [{ name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } }];
const RATE = 'Add rate limiter to the public API';
const url = (n: number) => `https://github.com/acme/api-core/pull/101#discussion_r${n}`;
const said = (login: string, body: string, n: number, at: string) =>
  ({ author: { login, __typename: 'User' }, authorAssociation: 'MEMBER', body, url: url(n), createdAt: at });

const CONVERSATION = {
  reviewThreads: {
    nodes: [
      { isResolved: false, isOutdated: false, path: 'src/a.go', line: 12, resolvedBy: null,
        comments: { totalCount: 2, nodes: [said('e2e-user', 'why a lock here?', 1, '2026-11-01T10:00:00Z'), said('alice', 'two writers race on it', 2, '2026-11-02T10:00:00Z')] } },
      { isResolved: true, isOutdated: false, path: 'src/b.go', line: 3, resolvedBy: { login: 'e2e-user', __typename: 'User' },
        comments: { totalCount: 1, nodes: [said('e2e-user', 'settled', 3, '2026-11-01T10:00:00Z')] } },
      { isResolved: false, isOutdated: false, path: 'src/c.go', line: 9, resolvedBy: null,
        comments: { totalCount: 1, nodes: [said('bob', 'not mine', 4, '2026-11-01T10:00:00Z')] } },
      { isResolved: false, isOutdated: true, path: 'src/d.go', line: null, originalLine: 40, resolvedBy: null,
        comments: { totalCount: 1, nodes: [said('e2e-user', 'still here?', 5, '2026-11-01T10:00:00Z')] } },
    ],
  },
};

/** gh calls that write: a review, a comment, a reaction, a mutation. */
const writes = (bz: Bazel) => bz.ghCalls().filter((c) => (c.argv[0] === 'pr' && c.argv[1] === 'comment')
  || (c.argv[0] === 'api' && c.argv.some((a) => a === 'POST' || a === '-X' || a === '--method' || a.includes('mutation'))));

test.describe('P1 · your open threads', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  test('the list tags them, ticking and Review remind of them, and the PR page lists them', async ({ app, bazel }) => {
    bazel.updatePR('acme/api-core#101', { conversation: CONVERSATION });
    await app.getByTestId('refresh').click(); // the page read them at boot: ↻ reads them again
    const tag = ui.prRow(app, RATE).getByTestId('pr-open-threads');
    await expect(tag).toHaveText('💬 2 open');
    await expect(tag).toHaveAttribute('title', 'your threads still open:\nsrc/a.go:12 (author replied)\nsrc/d.go:40');
    await expect(ui.prRow(app, 'Fix nil pointer on empty config').getByTestId('pr-open-threads')).toHaveCount(0);

    await tickByFilter(app, RATE);
    const note = app.getByTestId('threads-note');
    await expect(note).toHaveText('acme/api-core#101: You still have 2 open threads on this PR — src/a.go:12, src/d.go:40');
    await expect(note.getByRole('link', { name: 'src/a.go:12' })).toHaveAttribute('href', url(1));

    // A reminder, not a gate: the review goes.
    await ui.runButton(app).click();
    await expect(ui.jobCard(app, RATE)).toHaveCount(1);
    await expect(note).toBeVisible();

    await ui.prRow(app, RATE).locator('.title').click();
    const page = app.getByTestId('pr-page');
    const items = page.getByTestId('pr-open-thread');
    await expect(items).toHaveCount(2);
    await expect(items.first()).toContainText('src/a.go:12');
    await expect(items.first()).toContainText('@alice: two writers race on it');
    await expect(items.first().getByTestId('thread-author-replied')).toHaveText('author replied');
    await expect(items.nth(1)).toContainText('outdated');
    await expect(items.nth(1).getByTestId('thread-author-replied')).toHaveCount(0);
    expect(writes(bazel)).toEqual([]);
  });

  test('a read that fails blocks nothing: no tags, no reminder, and the review goes', async ({ app, bazel }) => {
    bazel.updatePR('acme/api-core#101', { conversation: CONVERSATION });
    bazel.setGh({ fail: { conversation: 'HTTP 502: Bad Gateway' } });
    // A server with no good read to fall back on: the page's first read at
    // boot may have come before the failure, and would be kept as stale.
    await bazel.restart({ samePort: true });
    await app.reload();
    await expect(ui.prRow(app, RATE)).toBeVisible();
    const r = await bazel.api('GET', '/api/open-threads?refresh=1');
    expect(r.status).toBe(200);
    expect(r.json.error).toContain('open threads could not be read');
    await expect(app.getByTestId('pr-open-threads')).toHaveCount(0);
    // With nothing to fall back on, the page says the threads could not be read.
    await expect(app.getByTestId('threads-error')).toHaveText('Your open threads could not be read from GitHub, so none are shown.');
    await tickByFilter(app, RATE);
    await expect(app.getByTestId('threads-note-pr')).toHaveCount(0);
    await ui.runButton(app).click();
    await expect(ui.jobCard(app, RATE)).toHaveCount(1);
    expect(writes(bazel)).toEqual([]);
  });

  // L-OT-2: resolve the last open thread and refresh: the tag goes, and so
  // does the section on the PR's page, which follows the new read.
  test('the PR page follows a read where the threads were resolved', async ({ app, bazel }) => {
    const one = { reviewThreads: { nodes: [CONVERSATION.reviewThreads.nodes[0]] } };
    bazel.updatePR('acme/api-core#101', { conversation: one });
    await app.getByTestId('refresh').click();
    await expect(ui.prRow(app, RATE).getByTestId('pr-open-threads')).toHaveText('💬 1 open');
    await ui.prRow(app, RATE).locator('.title').click();
    const page = app.getByTestId('pr-page');
    await expect(page.getByTestId('pr-open-thread')).toHaveCount(1);

    const resolved = { ...CONVERSATION.reviewThreads.nodes[0], isResolved: true, resolvedBy: { login: 'e2e-user', __typename: 'User' } };
    bazel.updatePR('acme/api-core#101', { conversation: { reviewThreads: { nodes: [resolved] } } });
    await app.getByTestId('refresh').click();
    await expect(ui.prRow(app, RATE).getByTestId('pr-open-threads')).toHaveCount(0);
    await expect(app.getByTestId('pr-page').getByTestId('pr-open-thread')).toHaveCount(0);
    await expect(app.getByTestId('pr-page').getByTestId('pr-open-threads-list')).toHaveCount(0);
  });

  // BUG-QA-40: a read that fails after one that worked keeps its tags, marked
  // stale with the time of the read they came from.
  test('a failed refresh keeps the last good tags, as stale', async ({ app, bazel }) => {
    bazel.updatePR('acme/api-core#101', { conversation: { reviewThreads: { nodes: [CONVERSATION.reviewThreads.nodes[0]] } } });
    await app.getByTestId('refresh').click();
    const tag = ui.prRow(app, RATE).getByTestId('pr-open-threads');
    await expect(tag).toHaveAttribute('data-stale', 'false');
    bazel.setGh({ fail: { conversation: 'HTTP 502: Bad Gateway' } });
    await app.getByTestId('refresh').click();
    await expect(tag).toHaveAttribute('data-stale', 'true');
    await expect(tag).toHaveText('💬 1 open');
    await expect(tag).toHaveAttribute('title', /as of \d\d:\d\d.* \(could not refresh\)$/);
    // The reminder and the PR's page say it too.
    await tickByFilter(app, RATE);
    await expect(app.getByTestId('threads-note-pr').getByTestId('threads-stale')).toHaveText(/^as of \d\d:\d\d.* \(could not refresh\)$/);
    await ui.prRow(app, RATE).locator('.title').click();
    await expect(app.getByTestId('pr-page').getByTestId('threads-stale')).toHaveText(/^as of \d\d:\d\d.* \(could not refresh\)$/);
    await expect(app.getByTestId('pr-open-thread')).toHaveCount(1);
    // Asked again, inside the backoff: the same last good read, still stale.
    const r = await bazel.api('GET', '/api/open-threads?refresh=1');
    expect([r.json.stale, Object.keys(r.json.threads)]).toEqual([true, ['acme/api-core#101']]);
  });

  // OT2-1: GitHub says few points are left: the tags' reads stop until the
  // reset, the last good tags stay, stale, and the rest of Bazel keeps reading.
  test('a low rate limit stops the tag reads, not the rest', async ({ app, bazel }) => {
    bazel.updatePR('acme/api-core#101', { conversation: { reviewThreads: { nodes: [CONVERSATION.reviewThreads.nodes[0]] } } });
    bazel.setGh({ rate_remaining: 300 });
    await app.getByTestId('refresh').click();
    const tag = ui.prRow(app, RATE).getByTestId('pr-open-threads');
    await expect(tag).toHaveText('💬 1 open');
    const tags = () => bazel.ghCalls().filter((c) => c.argv.some((a) => a.startsWith('query=query bazelThreadTags('))).length;
    const before = tags();
    await app.getByTestId('refresh').click();
    await expect(tag).toHaveAttribute('data-stale', 'true');
    expect(tags()).toBe(before);
    const r = await bazel.api('GET', '/api/open-threads?refresh=1');
    expect(r.json.error).toContain('rate limit is low');
    expect((await bazel.api('GET', '/api/needs?refresh=1')).status).toBe(200);
  });
});

