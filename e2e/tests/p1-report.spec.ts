import { test, expect, knownBug } from '../harness/fixtures';
import { openJob, ui } from '../harness/ui';

const plain = (name: string, scenario: string, extra: Record<string, unknown> = {}) =>
  ({ name, task: `/exploit-digger {{number}}`, command: 'fake-agent', args: [], format: 'plain', env: { FAKE_AGENT_SCENARIO: scenario }, ...extra });
const AGENTS = [
  { name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } },
  { name: 'slow-lens', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-slow' } },
  plain('wrapped', 'wrapped'),
  plain('raw-html', 'html-raw'),
  plain('formats', 'finding-formats'),
  plain('history', 'history-plain', { publishable: false }),
];

test.describe('P1 · report and findings', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  test('a report wrapped in ```markdown is unwrapped before rendering', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'wrapped');
    const job = await bazel.waitJob(id, (j) => j.state === 'done');
    expect(job.html).toMatch(/<h1[^>]*>Wrapped review E2E-WRAPPED<\/h1>/);
    expect(job.html).not.toContain('<pre');
  });

  test('raw HTML shows as escaped text; the finding comment is left out', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'raw-html');
    const job = await bazel.waitJob(id, (j) => j.state === 'done');
    expect(job.html).not.toMatch(/<script/i);
    expect(job.html).not.toMatch(/<div onclick/i);
    expect(job.html).toContain('&lt;script&gt;');
    expect(job.html).not.toContain('finding id=');
    await app.reload();
    await openJob(app, 'Add rate limiter to the public API');
    // What you read is the text that would be published.
    await expect(ui.viewer(app).getByText("<script>alert('E2E-XSS')</script>", { exact: false })).toBeVisible();
  });

  test('bold-numbered findings, human-verification items and cuts each get a report box; skips map to them', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'formats');
    await bazel.waitJob(id, (j) => j.state === 'done');
    await app.reload();
    const viewer = await openJob(app, 'Add rate limiter to the public API');
    await expect(ui.findingPicks(viewer)).toHaveCount(5);
    await ui.findingPicks(viewer).nth(1).uncheck(); // E2E-BOLD-2
    await ui.findingPicks(viewer).nth(2).uncheck(); // E2E-HUMAN-1
    await ui.pasteAsComment(viewer).click();
    await bazel.waitJob(id, (j) => j.posted);
    const [c] = bazel.ghCalls().filter((x) => x.argv[1] === 'comment');
    for (const kept of ['E2E-BOLD-1', 'E2E-HUMAN-2', 'E2E-CUT-1']) expect(c.stdin).toContain(kept);
    for (const gone of ['E2E-BOLD-2', 'E2E-HUMAN-1']) expect(c.stdin).not.toContain(gone);
  });

  test('a non-publishable agent: "stays here", no buttons, and the API refuses both paths', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'history');
    const job = await bazel.waitJob(id, (j) => j.state === 'done');
    expect(job.publishable).toBe(false);
    await app.reload();
    const viewer = await openJob(app, 'Add rate limiter to the public API');
    await expect(ui.notAReview(viewer, 'history')).toBeVisible();
    await expect(ui.publishInline(viewer)).toHaveCount(0);
    await expect(ui.pasteAsComment(viewer)).toHaveCount(0);

    expect((await bazel.api('POST', `/api/jobs/${id}/publish`, {})).status).toBe(409);
    const post = await bazel.api('POST', `/api/jobs/${id}/post`, {});
    expect(post.status).toBeGreaterThanOrEqual(400);
    expect(bazel.ghCalls().filter((c) => c.argv[1] === 'comment' || (c.argv[0] === 'api' && c.argv[1] !== 'user'))).toEqual([]);
  });

  // BUG-QA-05: POST /api/jobs/{id}/post answers 502 Bad Gateway for refusals
  // that are the client's fault ("does not go to the PR", "has not finished"),
  // the same status as a real gh failure. The page shows "failed to publish"
  // either way. Expected a 4xx (409 like /publish). Remove the knownBug line once fixed.
  test('refusing to post a non-publishable or unfinished review is a 4xx, not 502', async ({ bazel }) => {
    knownBug('BUG-QA-05', '/post refusals answer 502');
    const id = await bazel.enqueue('acme/api-core#101', 'history');
    await bazel.waitJob(id, (j) => j.state === 'done');
    expect((await bazel.api('POST', `/api/jobs/${id}/post`, {})).status).toBe(409);
  });
});

test.describe('P1 · tokens and quota', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  test('a running review shows a partial ~count that closes to the full tally', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'slow-lens');
    const running = await bazel.waitJob(id, (j) => j.state === 'running' && j.tokens > 0);
    expect(running.tokens_partial).toBe(true);
    expect(running.tokens).toBe(4120);
    await app.reload();
    const card = ui.jobCard(app, 'Add rate limiter to the public API');
    await expect(card.getByText('~4,1k tokens', { exact: false })).toBeVisible();

    bazel.release('release-slow');
    const done = await bazel.waitJob(id, (j) => j.state === 'done');
    expect(done.tokens_partial ?? false).toBe(false);
    expect(done.tokens).toBe(4200);
    await expect(card.getByText('~4,1k tokens', { exact: false })).toHaveCount(0);
  });

  test('quota from rate_limit_event: hidden on a fresh server, then "claude session 86% · week 16%"', async ({ app, bazel }) => {
    const fresh = (await bazel.api('GET', '/api/state')).json.limits;
    expect(fresh?.session?.utilization ?? 0).toBe(0);
    await expect(app.getByText(/^claude session/)).toHaveCount(0);

    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    await app.reload();
    const quota = app.getByText('claude session 86% · week 16%');
    await expect(quota).toBeVisible();
    await expect(quota).toHaveAttribute('title', /session resets at .*\n.*week resets at/s);
  });

  test('the session total in the top bar adds up the jobs', async ({ app, bazel }) => {
    const a = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    const b = await bazel.enqueue('acme/web-app#7', 'review-fleet');
    await bazel.waitJob(a, (j) => j.state === 'done');
    await bazel.waitJob(b, (j) => j.state === 'done');
    await app.reload();
    await expect(app.getByText('3,6M tokens · $4.18', { exact: true })).toBeVisible();
  });
});

test.describe('P2 · pricing override', () => {
  test.use({
    bazelOptions: {
      config: {
        agents: AGENTS,
        agent: {
          command: 'fake-agent', args: ['-p', '--output-format', 'stream-json', '--verbose'], format: 'claude-stream', checkout: true, timeout_seconds: 60,
          pricing: { 'claude-opus-5-5': { input: 1, output: 1, cache_write: 1, cache_read: 1 } },
        },
      },
    },
  });

  test('agent.pricing in the config reprices the run', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    const job = await bazel.waitJob(id, (j) => j.state === 'done');
    expect(job.cost_usd).toBeCloseTo(1.8, 2); // 1.8M tokens at $1/M
  });
});
