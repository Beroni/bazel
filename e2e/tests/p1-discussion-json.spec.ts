import { test, expect } from '../harness/fixtures';
import { Bazel } from '../harness/bazel';

// discussion-json: before a review's first step Bazel leaves the PR's discussion
// in <plugin>/discussion.json for the fleet, from one read-only gh read. A read
// that fails leaves no file, says so in the log, and the review goes.
const AGENTS = [{ name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } }];
const K = 'acme/api-core#101';
const said = (login: string, body: string, n: number, type = 'User') =>
  ({ author: { login, __typename: type }, authorAssociation: 'MEMBER', body, url: `https://github.com/acme/api-core/pull/101#discussion_r${n}`, createdAt: '2026-11-01T10:00:00Z' });
const CONVERSATION = {
  reviewThreads: {
    nodes: [
      { isResolved: true, isOutdated: false, path: 'src/a.go', line: 12, resolvedBy: { login: 'alice', __typename: 'User' },
        comments: { totalCount: 2, nodes: [said('e2e-user', 'why a lock here?', 1), said('alice', 'two writers race on it', 2)] } },
      { isResolved: false, isOutdated: true, path: 'src/b.go', line: null, originalLine: 40, resolvedBy: null,
        comments: { totalCount: 1, nodes: [said('bob', 'x'.repeat(5000), 3)] } },
    ],
  },
  comments: { nodes: [said('ci-bot', 'coverage 91%', 4, 'Bot')] },
  reviews: { nodes: [{ author: { login: 'carol', __typename: 'User' }, authorAssociation: 'MEMBER', state: 'APPROVED', body: 'ok', url: 'https://github.com/acme/api-core/pull/101#pullrequestreview-5', submittedAt: '2026-11-02T10:00:00Z' }] },
};
const writes = (bz: Bazel) => bz.ghCalls().filter((c) => (c.argv[0] === 'pr' && c.argv[1] === 'comment')
  || (c.argv[0] === 'api' && c.argv.some((a) => a === 'POST' || a === '-X' || a === '--method' || a.includes('mutation'))));

test.describe('P1 · discussion.json for the fleet', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });
  test.afterEach(async ({ bazel }) => { expect(writes(bazel)).toEqual([]); });

  test('a review finds the PR\'s discussion in its plugin, private to the user, in the fleet\'s shape', async ({ bazel }) => {
    bazel.updatePR(K, { conversation: CONVERSATION });
    const id = await bazel.enqueue(K, 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    const [call] = bazel.agentCalls();
    expect(call.plugin_dir).not.toBe('');
    expect(call.discussion_mode).toBe('0600');
    const d = JSON.parse(call.discussion);
    expect(Object.keys(d).sort()).toEqual(['author', 'comments', 'complete', 'head_sha', 'pr', 'repo', 'reviews', 'threads']);
    expect(d).toMatchObject({ repo: 'acme/api-core', pr: 101, complete: true, author: { login: 'alice', type: 'User' } });
    expect(d.head_sha).toMatch(/^[0-9a-f]{40}$/);
    expect(d.threads).toHaveLength(2);
    const [resolved, outdated] = d.threads;
    expect(resolved).toMatchObject({ path: 'src/a.go', line: 12, isResolved: true, isOutdated: false, resolvedBy: { login: 'alice', type: 'User' } });
    expect(resolved.comments.map((c: any) => `${c.author.login}:${c.body}`)).toEqual(['e2e-user:why a lock here?', 'alice:two writers race on it']);
    expect(resolved.comments[0].author.association).toBe('MEMBER');
    expect(outdated).toMatchObject({ path: 'src/b.go', isResolved: false, isOutdated: true, resolvedBy: null });
    // A body past 4000 characters is cut, and says so.
    expect(outdated.comments[0].cut).toBe(true);
    expect(outdated.comments[0].body.endsWith('[cut by Bazel]')).toBe(true);
    expect(d.comments[0]).toMatchObject({ author: { login: 'ci-bot', type: 'Bot' }, body: 'coverage 91%' });
    expect(d.reviews[0]).toMatchObject({ author: { login: 'carol' }, state: 'APPROVED', body: 'ok' });
    // Read, and nothing else: the plugin goes with the job.
    expect(bazel.runtimes()).toEqual([]);
  });

  test('a PR that discussed nothing gets an empty, complete file', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#102', 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    const d = JSON.parse(bazel.agentCalls()[0].discussion);
    expect(d).toMatchObject({ pr: 102, complete: true, threads: [], comments: [], reviews: [] });
  });

  test('a read that fails: no file, a line in the log, and the review still goes', async ({ bazel }) => {
    bazel.updatePR(K, { conversation: CONVERSATION });
    bazel.setGh({ fail: { conversation: 'HTTP 502: Bad Gateway' } });
    const id = await bazel.enqueue(K, 'review-fleet');
    const job = await bazel.waitJob(id, (j) => j.state === 'done' || j.state === 'failed');
    expect(job.state).toBe('done');
    const [call] = bazel.agentCalls();
    expect(call.discussion).toBe('');
    expect(call.discussion_mode).toBe('');
    const log = (await bazel.api('GET', `/api/jobs/${id}/log?from=0`)).json.lines.map((l: any) => l.text).join('\n');
    expect(log).toContain("the PR's discussion could not be read, so the review goes without it");
    expect(log).toContain('HTTP 502');
  });
});
