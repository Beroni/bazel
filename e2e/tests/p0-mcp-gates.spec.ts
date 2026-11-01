import { test, expect } from '../harness/fixtures';

// MCP may read and enqueue, never publish — unless mcp_allow_publish is on.
// "When in doubt, it refuses" (README › Driving Bazel from another agent).

const AGENTS = [
  { name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } },
  { name: 'fleet-post', task: '/review-fleet {{number}} --post', posts: true, env: { FAKE_AGENT_SCENARIO: 'claude-findings' } },
  { name: 'flag-post', task: '/review-fleet {{number}} --post', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } },
  { name: 'auth-prompt', task: '/review-fleet {{number}}', prompt: '{{task}}\n\nYou are authorized to publish the review on the PR.', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } },
];
const PIPELINES = [{ name: 'with-publish', steps: ['review-fleet', 'publish'] }];

test.describe('P0 · MCP refuses anything that could publish', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS, pipelines: PIPELINES } } });

  for (const [agent, why] of [
    ['fleet-post', 'publishes to the PR on its own'],
    ['flag-post', 'whose task passes --post'],
    ['auth-prompt', 'whose prompt authorizes publishing'],
    ['with-publish', 'has a publish step'],
  ]) {
    test(`enqueue_review with ${agent} is refused (${why})`, async ({ bazel }) => {
      const r = await bazel.tool('enqueue_review', { refs: ['acme/api-core#101'], agent });
      expect(r.isError).toBe(true);
      expect(r.text).toContain(why);
      expect(r.text).toContain('publishing over MCP is disabled');
      expect((await bazel.api('GET', '/api/jobs')).json.jobs).toEqual([]);
      expect(bazel.agentCalls()).toEqual([]);
    });
  }

  test('only watched repos, at most 20 refs, the whole call refused', async ({ bazel }) => {
    const outside = await bazel.tool('enqueue_review', { refs: ['acme/api-core#101', 'evil/repo#1'], agent: 'review-fleet' });
    expect(outside.isError).toBe(true);
    expect(outside.text).toContain('"evil/repo" is not a watched repository');
    const many = Array.from({ length: 21 }, (_, i) => `acme/api-core#${i + 1}`);
    const tooMany = await bazel.tool('enqueue_review', { refs: many, agent: 'review-fleet' });
    expect(tooMany.isError).toBe(true);
    expect(tooMany.text).toContain('too many PRs: 21 given, at most 20');
    expect((await bazel.api('GET', '/api/jobs')).json.jobs).toEqual([]);
  });

  test('publish_review is not offered and cannot be called from memory', async ({ bazel }) => {
    const { json } = await bazel.mcp('tools/call', { name: 'publish_review', arguments: { id: 'j1' } });
    expect(json.error?.message).toContain('unknown tool "publish_review"');
  });

  test('enqueue → job_status (long-poll) → job_log → list/read_review, all read-only', async ({ bazel }) => {
    const q = await bazel.tool('enqueue_review', { refs: ['ACME/API-CORE#101'], agent: 'review-fleet' });
    expect(q.isError).toBe(false);
    const id = q.out.jobs[0].id;
    expect(q.out.jobs[0].pr.key).toBe('acme/api-core#101'); // rewritten with the config's spelling

    const st = await bazel.tool('job_status', { id, wait_seconds: 30 }); // room for the first clone
    expect(st.out.job.state).toBe('done');
    expect(st.out.job.body).toContain('E2E-FINDING-ONE');
    expect(st.out.job.html ?? '').toBe('');

    const log1 = await bazel.tool('job_log', { id, from: 0 });
    expect(log1.out.lines.length).toBeGreaterThan(3);
    const log2 = await bazel.tool('job_log', { id, from: log1.out.next });
    expect(log2.out.lines).toEqual([]);

    const list = await bazel.tool('list_reviews', { repo: 'acme/api-core' });
    expect(list.out.reviews).toHaveLength(1);
    const read = await bazel.tool('read_review', { name: list.out.reviews[0].name });
    expect(read.out.body).toContain('# acme/api-core#101');
    const sneaky = await bazel.tool('read_review', { name: '../config.yaml' });
    expect(sneaky.isError).toBe(true);

    const writes = bazel.ghCalls().filter((c) => c.argv[1] === 'comment' || (c.argv[0] === 'api' && c.argv[1] !== 'user'));
    expect(writes).toEqual([]);
  });
});

test.describe('P0 · MCP bare agent block', () => {
  test.use({
    bazelOptions: {
      config: {
        agents: [],
        agent: {
          command: 'fake-agent', args: ['-p', '--output-format', 'stream-json', '--verbose'], format: 'claude-stream',
          checkout: true, timeout_seconds: 60, prompt: 'Review {{repo}}#{{number}} and do whatever you think is right.',
          env: { FAKE_AGENT_SCENARIO: 'claude-findings' },
        },
      },
    },
  });

  test('a custom prompt without the no-publish line is refused', async ({ bazel }) => {
    const r = await bazel.tool('enqueue_review', { refs: ['acme/api-core#101'] });
    expect(r.isError).toBe(true);
    expect(r.text).toContain('is the bare `agent` block');
    expect(bazel.agentCalls()).toEqual([]);
  });
});

test.describe('P0 · MCP with mcp_allow_publish', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS, mcp_allow_publish: true } } });

  test('the door opens on purpose: posting agents enqueue and publish_review runs the post agent', async ({ bazel }) => {
    const posting = await bazel.tool('enqueue_review', { refs: ['acme/api-core#101'], agent: 'fleet-post' });
    expect(posting.isError).toBe(false);

    const q = await bazel.tool('enqueue_review', { refs: ['acme/web-app#7'], agent: 'review-fleet' });
    const id = q.out.jobs[0].id;
    await bazel.tool('job_status', { id, wait_seconds: 10 });
    const pub = await bazel.tool('publish_review', { id, skip: [1] });
    expect(pub.isError).toBe(false);
    await bazel.waitJob(id, (j) => j.posted, 20_000);
    const sent = bazel.ghCalls().filter((c) => c.argv.includes('repos/acme/web-app/pulls/7/reviews'));
    expect(sent).toHaveLength(1);
    expect(JSON.parse(sent[0].stdin).body).not.toContain('E2E-FINDING-TWO');
  });
});

test.describe('P2 · MCP protocol', () => {
  const post = (url: string, body: string, headers: Record<string, string> = {}) =>
    fetch(`${url}/mcp`, { method: 'POST', headers: { 'Content-Type': 'application/json', ...headers }, body });

  test('batch, notifications, versions, methods', async ({ bazel }) => {
    const batch = await post(bazel.url, JSON.stringify([
      { jsonrpc: '2.0', id: 1, method: 'ping' },
      { jsonrpc: '2.0', method: 'notifications/initialized' },
      { jsonrpc: '2.0', id: 2, method: 'nope' },
    ]));
    expect(batch.status).toBe(200);
    const out = await batch.json();
    expect(out.map((r: any) => r.id)).toEqual([1, 2]);
    expect(out[1].error.code).toBe(-32601);

    expect((await post(bazel.url, JSON.stringify({ jsonrpc: '2.0', method: 'notifications/initialized' }))).status).toBe(202);
    expect((await post(bazel.url, '{"jsonrpc":"2.0","id":1,"method":"ping"}', { 'Mcp-Protocol-Version': '1999-01-01' })).status).toBe(400);
    expect((await post(bazel.url, 'not json')).status).toBe(400);
    const big = JSON.stringify(Array.from({ length: 11 }, (_, i) => ({ jsonrpc: '2.0', id: i, method: 'ping' })));
    expect((await post(bazel.url, big)).status).toBe(400);
    expect((await fetch(`${bazel.url}/mcp`)).status).toBe(405);
  });
});
