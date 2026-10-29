import { test, expect, knownBug } from '../harness/fixtures';
import { ui } from '../harness/ui';

const plain = (name: string, scenario: string, extra: Record<string, unknown> = {}) =>
  ({ name, task: '/exploit-digger {{number}}', command: 'fake-agent', args: [], format: 'plain', env: { FAKE_AGENT_SCENARIO: scenario }, ...extra });
const AGENTS = [
  { name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } },
  { name: 'slow-lens', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-slow' } },
  { name: 'timeout-lens', task: '/review-fleet {{number}}', timeout_seconds: 1, env: { FAKE_AGENT_SCENARIO: 'claude-slow' } },
  plain('broken', 'fail-stderr'),
  plain('flood', 'log-flood'),
  { name: 'ghost', task: '/review-fleet {{number}}', command: 'no-such-agent', args: [], format: 'plain' },
];

test.describe('P1 · queue', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  test('--jobs 2 (default): a third review waits in the queue until a slot frees', async ({ bazel }) => {
    const ids = [
      await bazel.enqueue('acme/api-core#101', 'slow-lens'),
      await bazel.enqueue('acme/api-core#102', 'slow-lens'),
      await bazel.enqueue('acme/web-app#7', 'slow-lens'),
    ];
    await expect.poll(() => bazel.agentCalls().length).toBe(2);
    const states = await Promise.all(ids.map(async (id) => (await bazel.job(id)).state));
    expect(states.sort()).toEqual(['queued', 'running', 'running']);
    bazel.release('release-slow');
    for (const id of ids) await bazel.waitJob(id, (j) => j.state === 'done');
    expect(bazel.agentCalls()).toHaveLength(3);
  });

  test('same PR + same agent while live → the same job; another agent → a new one', async ({ bazel }) => {
    const a = await bazel.enqueue('acme/api-core#101', 'slow-lens');
    const b = await bazel.enqueue('acme/api-core#101', 'slow-lens');
    const c = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    expect(b).toBe(a);
    expect(c).not.toBe(a);
    bazel.release('release-slow');
  });

  test('bad refs are reported one by one without sinking the good ones', async ({ bazel }) => {
    const mixed = await bazel.api('POST', '/api/reviews', { refs: ['acme/api-core#101', 'not-a-ref', 'acme/api-core#999'], agent: 'review-fleet' });
    expect(mixed.status).toBe(202);
    expect(mixed.json.jobs).toHaveLength(1);
    expect(mixed.json.errors).toHaveLength(2);
    expect(mixed.json.errors[0]).toContain('invalid reference');
    expect(mixed.json.errors[1]).toContain('acme/api-core#999');
    const none = await bazel.api('POST', '/api/reviews', { refs: ['nope'], agent: 'review-fleet' });
    expect(none.status).toBe(400);
    const unknownAgent = await bazel.api('POST', '/api/reviews', { refs: ['acme/api-core#101'], agent: 'nobody' });
    expect(unknownAgent.status).toBe(400);
  });

  test('✕ on a running card cancels it and drops it from the server', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/web-app#7', 'slow-lens');
    await bazel.waitJob(id, (j) => j.state === 'running');
    await app.reload();
    await ui.removeButton(ui.jobCard(app, 'Dark mode toggle in settings')).click(); // confirm accepted
    await expect.poll(async () => (await bazel.api('GET', `/api/jobs/${id}`)).status).toBe(404);
    expect((await bazel.api('GET', '/api/jobs')).json.jobs).toEqual([]);
    bazel.release('release-slow');
    await new Promise((r) => setTimeout(r, 300));
    expect(bazel.savedReviews()).toEqual([]);
  });

  // BUG-QA-04: Manager.Remove deletes a running job and broadcasts job_gone,
  // but the worker then hits finish() → publish(job) and broadcasts a "job"
  // event for the job it no longer has. The page upserts it back: a ghost
  // "canceled" card whose ✕ answers 404. The comment in Remove says marking
  // the state "prevents it from republishing", but finish never checks.
  // Expected: no "job" event for an id after its job_gone.
  test('a removed job does not come back as a ghost card', async ({ app, bazel }) => {
    knownBug('BUG-QA-04', 'job event after job_gone resurrects the card');
    const sse = bazel.events();
    const id = await bazel.enqueue('acme/web-app#7', 'slow-lens');
    await bazel.waitJob(id, (j) => j.state === 'running');
    await app.reload();
    const card = ui.jobCard(app, 'Dark mode toggle in settings');
    await ui.removeButton(card).click();
    await sse.next((e) => e.type === 'job_gone' && e.data.id === id);
    await new Promise((r) => setTimeout(r, 500));
    sse.close();
    const gone = sse.list.findIndex((e) => e.type === 'job_gone' && e.data.id === id);
    const after = sse.list.slice(gone + 1).filter((e) => e.type === 'job' && e.data.id === id);
    expect(after.map((e) => e.data.state)).toEqual([]);
    await expect(card).toHaveCount(0);
  });

  test('dropping a finished card keeps its markdown on disk', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    expect((await bazel.api('DELETE', `/api/jobs/${id}`)).status).toBe(200);
    expect(bazel.savedReviews()).toHaveLength(1);
  });

  test('timeout_seconds: the agent is killed and the job says why', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'timeout-lens');
    const job = await bazel.waitJob(id, (j) => j.state === 'failed', 10_000);
    expect(job.error).toBe('`timeout-lens` blew past its 1s timeout');
    expect(bazel.clones()).toEqual([]);
  });

  test('an agent exiting non-zero fails with its stderr; stderr lines are marked in the log', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'broken');
    const job = await bazel.waitJob(id, (j) => j.state === 'failed');
    expect(job.error).toBe('agent `fake-agent` failed: E2E-STDERR boom: model overloaded');
    const { json } = await bazel.api('GET', `/api/jobs/${id}/log?from=0`);
    expect(json.lines.find((l: any) => l.text.includes('E2E-STDERR')).stream).toBe('stderr');
    expect(json.lines.find((l: any) => l.text.includes('E2E-PARTIAL')).stream).toBe('stdout');
  });

  test('an agent command missing from PATH fails before running', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'ghost');
    const job = await bazel.waitJob(id, (j) => j.state === 'failed');
    expect(job.error).toBe('agent `no-such-agent` not found in PATH');
  });

  test('the log is a 500-line window: dropped, next and from', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'flood');
    await bazel.waitJob(id, (j) => j.state === 'done');
    const { json: all } = await bazel.api('GET', `/api/jobs/${id}/log?from=0`);
    expect(all.lines).toHaveLength(500);
    expect(all.dropped).toBe(all.next - 500);
    expect(all.next).toBe(650);
    expect(all.dropped).toBe(150);
    expect(all.lines[0].text).toBe('E2E-LINE 151');
    expect(all.lines.at(-1).text).toBe('E2E-LINE 650');
    const { json: tail } = await bazel.api('GET', `/api/jobs/${id}/log?from=${all.next - 1}`);
    expect(tail.lines).toHaveLength(1);
    expect(tail.next).toBe(all.next);
    expect((await bazel.api('GET', `/api/jobs/${id}/log?from=-1`)).status).toBe(400);
  });

  test('a restart: new SSE instance, empty queue, reviews still on disk', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    const before = bazel.events();
    const hello1 = await before.next((e) => e.type === 'hello');
    before.close();

    await bazel.restart();
    const after = bazel.events();
    const hello2 = await after.next((e) => e.type === 'hello');
    after.close();
    expect(hello2.data.instance).not.toBe(hello1.data.instance);
    expect((await bazel.api('GET', '/api/jobs')).json.jobs).toEqual([]);
    const saved = (await bazel.api('GET', '/api/reviews')).json.reviews;
    expect(saved).toHaveLength(1);
    expect(saved[0].agent).toBe('review-fleet');
    const prs = (await bazel.api('GET', '/api/prs')).json.prs;
    expect(prs.find((p: any) => p.key === 'acme/api-core#101').reviewed).toBe(true);
  });
});

test.describe('P1 · no agents', () => {
  test.use({ bazelOptions: { config: { agents: [] } } });

  test('with no agents the run button stays disabled and says why', async ({ app }) => {
    await ui.selectAll(app).check();
    await expect(ui.runButton(app)).toBeDisabled();
    await expect(ui.runButton(app)).toHaveAttribute('title', /no agents configured/);
  });
});
