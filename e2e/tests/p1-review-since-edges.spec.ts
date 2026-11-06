import { test, expect } from '../harness/fixtures';
import { Bazel } from '../harness/bazel';
import { ME, node, pushed, review } from '../harness/needs';
import { ui } from '../harness/ui';

// feat/incremental-review, beyond p1-review-since: a force push, a restart, a
// review never done, the built-in skills and a chain of reviews of what changed.
const K = 'acme/api-core#101';
const TITLE = 'Add rate limiter to the public API';
const AGENTS = [
  { name: 'review-fleet', task: '/review-fleet {{number}} --since={{since}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } },
  { name: 'slow-lens', task: '/review-fleet {{number}} --since={{since}}', env: { FAKE_AGENT_SCENARIO: 'claude-slow' } },
  { name: 'builtin', task: '/bazel-review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'bazel-review-fleet' } },
  { name: 'plain', task: 'review {{number}}', command: 'fake-agent', args: [], format: 'plain', env: { FAKE_AGENT_SCENARIO: 'plain-report' } },
];
const sinceOf = (prompt: string) => /--since=(\S*)/.exec(prompt)?.[1];
const lastPrompt = (bz: Bazel) => bz.agentCalls().at(-1)!.prompt.split('\n')[0];

async function reviewed(bazel: Bazel, agent = 'review-fleet') {
  await bazel.waitJob(await bazel.enqueue(K, agent), (j) => j.state === 'done');
  return (await bazel.api('GET', '/api/prs')).json.prs.find((p: any) => p.key === K);
}
async function sinceJob(bazel: Bazel, agent = 'review-fleet') {
  const r = await bazel.api('POST', '/api/reviews', { refs: [K], agent, since: true });
  expect(r.status).toBe(202);
  return bazel.waitJob(r.json.jobs[0].id, (j) => j.state === 'done');
}
const logOf = async (bazel: Bazel, id: string) => (await bazel.api('GET', `/api/jobs/${id}/log?from=0`)).json.lines.map((l: any) => l.text).join('\n');
const bodies = async (bazel: Bazel) => Promise.all((await bazel.api('GET', '/api/reviews')).json.reviews.map(async (s: any) => (await bazel.api('GET', `/api/reviews/${s.name}`)).json.body as string));

test.describe('P1 · review changes since the last review, edges', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  test('after a force push the review is a whole one: no since for the agent, the log says why, the file has no Since', async ({ bazel }) => {
    await reviewed(bazel);
    bazel.rewritePR(K);
    expect((await bazel.api('GET', '/api/prs?refresh=1')).json.prs.find((p: any) => p.key === K).changed_since_review).toBe(true);
    const job = await sinceJob(bazel);
    expect(sinceOf(lastPrompt(bazel))).toBe('');
    expect(await logOf(bazel, job.id)).toContain('is not behind this one (a force push, or no clone): reviewing the whole PR');
    expect((await bodies(bazel)).filter((b) => b.includes('- Since:'))).toEqual([]);
  });

  // BUG-QA-35, fixed in feat/incremental-review: after that fallback the job kept
  // the old head as `since`, and its card said "↻ changes since 32d24c4" over a
  // review of the whole PR.
  test('after a force push the card does not say "changes since"', async ({ app, bazel }) => {
    await reviewed(bazel);
    bazel.rewritePR(K);
    await bazel.api('GET', '/api/prs?refresh=1');
    const job = await sinceJob(bazel);
    await app.reload();
    expect(await ui.jobCard(app, TITLE).first().getAttribute('title')).not.toContain('changes since');
    expect(job.since ?? '').toBe('');
  });

  test('nothing changed since the last review: 409, and no agent runs', async ({ bazel }) => {
    await reviewed(bazel);
    const calls = bazel.agentCalls().length;
    const r = await bazel.api('POST', '/api/reviews', { refs: [K], agent: 'review-fleet', since: true });
    expect(r.status).toBe(409);
    expect(JSON.stringify(r.json)).toContain('nothing changed since the last review');
    expect(bazel.agentCalls()).toHaveLength(calls);
    // A whole review of the same head is still yours to ask for.
    expect((await bazel.api('POST', '/api/reviews', { refs: [K], agent: 'review-fleet' })).status).toBe(202);
  });

  test('a restart keeps where the last review was', async ({ bazel }) => {
    const first = await reviewed(bazel);
    await bazel.restart();
    bazel.movePR(K);
    await bazel.api('GET', '/api/prs?refresh=1');
    const job = await sinceJob(bazel);
    expect(job.since).toMatch(/^[0-9a-f]{40}$/);
    expect(sinceOf(lastPrompt(bazel))).toBe(job.since);
    expect(first.changed_since_review ?? false).toBe(false);
  });

  test('a review of what changed waiting at a restart comes back paused, and runs from the same head', async ({ bazel }) => {
    await reviewed(bazel);
    bazel.movePR(K);
    await bazel.api('GET', '/api/prs?refresh=1');
    for (const ref of ['acme/web-app#7', 'acme/web-app#8']) await bazel.waitJob(await bazel.enqueue(ref, 'slow-lens'), (j) => j.state === 'running');
    const r = await bazel.api('POST', '/api/reviews', { refs: [K], agent: 'review-fleet', since: true });
    const since = r.json.jobs[0].since;
    expect(since).toMatch(/^[0-9a-f]{40}$/);
    await bazel.restart();
    const back = (await bazel.api('GET', '/api/jobs')).json.jobs.find((j: any) => j.pr.key === K);
    expect(back.state).toBe('paused');
    expect(back.since).toBe(since);
    await bazel.api('POST', `/api/jobs/${back.id}/resume`);
    await bazel.waitJob(back.id, (j) => j.state === 'done');
    expect(sinceOf(bazel.agentCalls().filter((c) => c.prompt.includes('/review-fleet 101')).at(-1)!.prompt)).toBe(since);
    bazel.release('release-slow');
  });

  test('a PR never reviewed gets a whole review, with nothing to say about it', async ({ bazel }) => {
    const job = await sinceJob(bazel);
    expect(job.since ?? '').toBe('');
    expect(sinceOf(lastPrompt(bazel))).toBe('');
    expect(await logOf(bazel, job.id)).not.toContain('↻');
  });

  test('a built-in review skill gets --since by itself; a task of your own only through {{since}}', async ({ bazel }) => {
    await reviewed(bazel);
    bazel.movePR(K);
    await bazel.api('GET', '/api/prs?refresh=1');
    const job = await sinceJob(bazel, 'builtin');
    expect(lastPrompt(bazel)).toBe(`/bazel-review-fleet 101 --since=${job.since}`);
    bazel.movePR(K); // the builtin review just marked the head it saw
    await bazel.api('GET', '/api/prs?refresh=1');
    await sinceJob(bazel, 'plain');
    expect(lastPrompt(bazel)).toBe('review 101');
  });

  test('a chain: each review of what changed starts at the head of the one before', async ({ bazel }) => {
    await reviewed(bazel);
    bazel.movePR(K);
    await bazel.api('GET', '/api/prs?refresh=1');
    const one = await sinceJob(bazel);
    bazel.movePR(K);
    const head = (await bazel.api('GET', '/api/prs?refresh=1')).json.prs.find((p: any) => p.key === K);
    expect(head.changed_since_review).toBe(true);
    const two = await sinceJob(bazel);
    expect(two.since).not.toBe(one.since);
    expect(await logOf(bazel, two.id)).toContain(`reviewing what changed since ${two.since.slice(0, 12)}`);
  });

  test('Needs you offers it on a reviewed PR that moved, and not on your own', async ({ app, bazel }) => {
    await reviewed(bazel);
    bazel.movePR(K);
    await bazel.api('GET', '/api/prs?refresh=1');
    bazel.setNeeds({
      reviewed: [node(K.split('#')[0], 101, TITLE, 'alice', { reviews: { nodes: [review(ME, 'CHANGES_REQUESTED', 10)] }, commits: pushed(12) })],
      authored: [node('acme/api-core', 102, 'Fix nil pointer on empty config', ME, { commits: pushed(3, 'FAILURE') })],
    });
    expect((await bazel.api('GET', '/api/needs?refresh=1')).status).toBe(200);
    await app.goto(bazel.url + '/#/needs');
    const row = (key: string) => app.locator(`[data-testid=needs-row][data-pr-key="${key}"]`);
    await expect(row(K).getByTestId('review-since')).toBeVisible();
    await expect(row('acme/api-core#102').getByTestId('review-since')).toHaveCount(0);
    await row(K).getByTestId('review-since').click();
    await expect.poll(() => bazel.agentCalls().length).toBe(2);
    expect(sinceOf(lastPrompt(bazel))).toMatch(/^[0-9a-f]{40}$/);
  });
});
