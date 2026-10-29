import fs from 'node:fs';
import path from 'node:path';
import { test, expect } from '../harness/fixtures';
import { Bazel } from '../harness/bazel';
import { ui } from '../harness/ui';

// The gates in front of GitHub. "GitHub" is the fake gh's log; a write is a
// `gh pr comment` or any `gh api` other than `api user`.

const writes = (bz: Bazel) =>
  bz.ghCalls().filter((c) => (c.argv[0] === 'pr' && c.argv[1] === 'comment') || (c.argv[0] === 'api' && c.argv[1] !== 'user'));
const reviewCalls = (bz: Bazel) => bz.ghCalls().filter((c) => c.argv[0] === 'api' && c.argv.some((a) => a.endsWith('/reviews')));
const TITLE = 'Add rate limiter to the public API';

const AGENTS = [
  { name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } },
  { name: 'plain-lens', task: '/exploit-digger {{number}}', command: 'fake-agent', args: [], format: 'plain', env: { FAKE_AGENT_SCENARIO: 'plain-report' } },
  { name: 'history', task: '/history-pr {{number}}', command: 'fake-agent', args: [], format: 'plain', publishable: false, env: { FAKE_AGENT_SCENARIO: 'history-plain' } },
  { name: 'broken', task: '/exploit-digger {{number}}', command: 'fake-agent', args: [], format: 'plain', env: { FAKE_AGENT_SCENARIO: 'fail-stderr' } },
  { name: 'fleet-post', task: '/review-fleet {{number}} --post', posts: true, env: { FAKE_AGENT_SCENARIO: 'claude-findings' } },
];
const PIPELINES = [
  { name: 'read-before-sending', steps: ['review-fleet', 'publish'] },
  { name: 'history-then-review', steps: ['history', 'review-fleet', 'publish'] },
  { name: 'two-lenses', steps: ['review-fleet', 'plain-lens'] },
  { name: 'one-breaks', steps: ['review-fleet', 'broken'] },
];

test.describe('P0 · publish step waits for you', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS, pipelines: PIPELINES } } });

  test('a pipeline ending in publish stops waiting, sends nothing, then publishes only what stays ticked', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'read-before-sending');
    const job = await bazel.waitJob(id, (j) => j.state === 'done');
    expect(job.awaits_publish).toBe(true);
    expect(job.posted).toBe(false);
    expect(writes(bazel)).toEqual([]);
    expect(bazel.agentCalls().map((c) => c.scenario)).toEqual(['claude-findings']);

    await app.reload();
    const card = ui.jobCard(app, TITLE);
    await expect(card.getByRole('button', { name: 'choose what goes' })).toBeVisible();
    await expect(card.getByRole('button', { name: 'keep it here' })).toBeVisible();
    await expect(ui.publishInline(card)).toHaveCount(0); // no one-click publish from the card

    await card.getByRole('button', { name: 'choose what goes' }).click();
    const viewer = ui.viewer(app);
    await expect(ui.findingPicks(viewer)).toHaveCount(2);
    await expect(viewer.getByRole('button', { name: 'keep it here' })).toBeVisible();
    await ui.findingPicks(viewer).nth(1).uncheck();
    await ui.publishInline(viewer).click();

    const done = await bazel.waitJob(id, (j) => j.posted || !!j.post_error, 20_000);
    expect(done.post_error ?? '').toBe('');
    expect(done.awaits_publish ?? false).toBe(false);
    const sent = reviewCalls(bazel);
    expect(sent).toHaveLength(1);
    const body = JSON.parse(sent[0].stdin).body;
    expect(body).toContain('E2E-FINDING-ONE');
    expect(body).not.toContain('E2E-FINDING-TWO');
    expect(writes(bazel)).toHaveLength(1);
  });

  test('"keep it here" ends the wait and nothing reaches GitHub', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'read-before-sending');
    await bazel.waitJob(id, (j) => j.state === 'done' && j.awaits_publish);
    await app.reload();
    const card = ui.jobCard(app, TITLE);
    await card.getByRole('button', { name: 'keep it here' }).click();
    await bazel.waitJob(id, (j) => !j.awaits_publish);
    await expect(card.getByRole('button', { name: 'choose what goes' })).toHaveCount(0);

    await new Promise((r) => setTimeout(r, 300));
    expect(writes(bazel)).toEqual([]);
    expect(bazel.savedReviews()).toHaveLength(1); // the review itself stays
    expect((await bazel.api('POST', `/api/jobs/${id}/dismiss`)).status).toBe(409);
  });

  test('a non-review step stays in Bazel: shown apart, never published', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'history-then-review');
    const job = await bazel.waitJob(id, (j) => j.state === 'done');
    expect(job.awaits_publish).toBe(true);
    expect(job.context_html).toContain('E2E-HISTORY');
    expect(job.publish_html).toContain('E2E-FINDING-ONE');
    expect(job.publish_html).not.toContain('E2E-HISTORY');
    // The saved file has both; what goes out has only the review.
    expect(fs.readFileSync(job.saved_to, 'utf8')).toContain('E2E-HISTORY');

    await app.reload();
    await ui.jobCard(app, TITLE).getByRole('button', { name: 'choose what goes' }).click();
    const viewer = ui.viewer(app);
    await expect(viewer.getByText('stays in Bazel — not published')).toBeVisible();
    await expect(viewer.getByText('goes to the PR')).toBeVisible();
    await expect(ui.findingPicks(viewer)).toHaveCount(2); // ticks only on the review part

    await ui.pasteAsComment(viewer).click();
    await bazel.waitJob(id, (j) => j.posted);
    const [comment] = bazel.ghCalls().filter((c) => c.argv[1] === 'comment');
    expect(comment.stdin).toContain('E2E-FINDING-ONE');
    expect(comment.stdin).not.toContain('E2E-HISTORY');
  });

  test('a pipeline runs its steps over one clone and stacks the reports', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'two-lenses');
    const job = await bazel.waitJob(id, (j) => j.state === 'done');
    expect(job.pipeline).toBe(true);
    expect(job.awaits_publish ?? false).toBe(false); // no publish step: a normal finished review
    expect(bazel.ghCalls().filter((c) => c.argv[1] === 'clone')).toHaveLength(1);
    const cwds = new Set(bazel.agentCalls().map((c) => c.cwd));
    expect(cwds.size).toBe(1);
    expect(job.html).toMatch(/<h2[^>]*>review-fleet<\/h2>/);
    expect(job.html).toMatch(/<h2[^>]*>plain-lens<\/h2>/);
    const md = fs.readFileSync(job.saved_to, 'utf8');
    expect(md).toMatch(/- Agent: two-lenses — review-fleet \(\d+s\) → plain-lens \(\d+s\)|- Agent: .*review-fleet.*plain-lens/);
  });

  test('a failing step becomes a section with its error; the rest carries on', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'one-breaks');
    const job = await bazel.waitJob(id, (j) => j.state === 'done' || j.state === 'failed');
    expect(job.state).toBe('done');
    expect(job.body).toContain('E2E-FINDING-ONE');
    expect(job.body).toContain('> ✗ failed: agent `fake-agent` failed: E2E-STDERR boom: model overloaded');
    expect(job.steps.map((s: any) => s.state)).toEqual(['done', 'failed']);
  });
});

test.describe('P0 · publishing failures and cancel', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  test('gh pr comment failing leaves the review publishable, and a retry goes through once', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    bazel.setGh({ fail: { comment: 'HTTP 403: Resource not accessible by integration' } });
    const first = await bazel.api('POST', `/api/jobs/${id}/post`, { skip: [] });
    expect(first.status).toBe(502);
    const failed = await bazel.job(id);
    expect(failed.posted).toBe(false);
    expect(failed.post_error).toContain('HTTP 403');

    bazel.setGh({});
    const second = await bazel.api('POST', `/api/jobs/${id}/post`, { skip: [] });
    expect(second.status).toBe(200);
    const comments = bazel.ghCalls().filter((c) => c.argv[1] === 'comment');
    expect(comments.map((c) => c.exit)).toEqual([1, 0]);
    expect((await bazel.job(id)).posted).toBe(true);
  });
});

test.describe('P0 · post agent failures', () => {
  test.use({
    bazelOptions: {
      config: {
        agents: AGENTS,
        post_agent: { name: 'bazel-post-report', task: '/bazel-post-report {{review_file}}', posts: true, env: { FAKE_AGENT_SCENARIO: 'post-fail' } },
      },
    },
  });

  test('the post agent failing sets post_error, keeps the review, and a retry does not stack steps', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    expect((await bazel.api('POST', `/api/jobs/${id}/publish`, { skip: [] })).status).toBe(202);
    const failed = await bazel.waitJob(id, (j) => j.state === 'done' && !!j.post_error, 20_000);
    expect(failed.post_error).toContain('E2E-POST-FAIL');
    expect(failed.posted).toBe(false);
    expect(failed.html).toContain('E2E-FINDING-ONE'); // the review is still on the card
    expect(failed.steps.map((s: any) => s.name)).toEqual(['review-fleet', 'bazel-post-report']);

    expect((await bazel.api('POST', `/api/jobs/${id}/publish`, { skip: [] })).status).toBe(202);
    const again = await bazel.waitJob(id, (j) => j.state === 'done' && !!j.post_error && bazel.agentCalls().length === 3, 20_000);
    expect(again.steps.map((s: any) => s.name)).toEqual(['review-fleet', 'bazel-post-report']);
    expect(reviewCalls(bazel)).toEqual([]);
    // publish/ copies do not pile up after failed runs.
    const pub = path.join(bazel.reviewsDir, 'publish');
    expect(fs.existsSync(pub) ? fs.readdirSync(pub) : []).toEqual([]);
  });
});

test.describe('P0 · cancel a publication', () => {
  test.use({
    bazelOptions: {
      config: {
        agents: AGENTS,
        post_agent: {
          name: 'bazel-post-report', task: '/bazel-post-report {{review_file}}', posts: true,
          env: { FAKE_AGENT_SCENARIO: 'post-claude', FAKE_AGENT_GATE: 'release-post' },
        },
      },
    },
  });

  test('cancelling the post agent mid-run: card back to done, post_error says so, nothing posted', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    await bazel.api('POST', `/api/jobs/${id}/publish`, { skip: [1] });
    await bazel.waitJob(id, (j) => j.state === 'running' && j.publishing);
    await expect.poll(() => bazel.agentCalls().length).toBe(2);

    expect((await bazel.api('POST', `/api/jobs/${id}/cancel`)).status).toBe(200);
    const back = await bazel.waitJob(id, (j) => j.state === 'done');
    expect(back.post_error).toBe('publishing canceled');
    expect(back.posted).toBe(false);
    expect(back.html).toContain('E2E-FINDING-TWO');

    bazel.release('release-post');
    await new Promise((r) => setTimeout(r, 300));
    expect(reviewCalls(bazel)).toEqual([]);
    const pub = path.join(bazel.reviewsDir, 'publish');
    expect(fs.existsSync(pub) ? fs.readdirSync(pub) : []).toEqual([]);
  });

  test('after a successful inline publish the trimmed copy in publish/ is gone', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    await bazel.api('POST', `/api/jobs/${id}/publish`, { skip: [0] });
    await expect.poll(() => bazel.agentCalls().length).toBe(2);
    const reviewFile = bazel.agentCalls()[1].prompt.match(/\/bazel-post-report\s+(\S+)/)![1];
    expect(fs.existsSync(reviewFile)).toBe(true); // exists while the post agent runs
    bazel.release('release-post');
    await bazel.waitJob(id, (j) => j.posted);
    expect(fs.existsSync(reviewFile)).toBe(false);
  });
});

test.describe('P0 · the page asks before writing', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  test('dismissing the confirm sends nothing, on both publish buttons', async ({ app, bazel, dialogs }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    await app.reload();
    await ui.jobCard(app, TITLE).click();
    const viewer = ui.viewer(app);
    dialogs.accept = false;
    await ui.publishInline(viewer).click();
    await ui.pasteAsComment(viewer).click();
    await expect.poll(() => dialogs.messages.length).toBe(2);
    await new Promise((r) => setTimeout(r, 300));
    expect(writes(bazel)).toEqual([]);
    expect(bazel.agentCalls()).toHaveLength(1);
  });

  test('an agent that publishes on its own asks before running, and its review warns before a comment', async ({ app, bazel, dialogs }) => {
    await ui.filter(app).fill('rate limiter');
    await ui.selectAll(app).check();
    await ui.agentSelect(app).selectOption('fleet-post');
    dialogs.accept = false;
    await ui.runButton(app).click();
    await expect.poll(() => dialogs.messages.length).toBe(1);
    expect(dialogs.messages[0]).toContain('The agent "fleet-post" publishes the review straight to acme/api-core#101');
    expect((await bazel.api('GET', '/api/jobs')).json.jobs).toEqual([]);

    dialogs.accept = true;
    await ui.runButton(app).click();
    const viewer = ui.viewer(app);
    await expect(viewer.getByText('E2E-FINDING-ONE')).toBeVisible({ timeout: 20_000 });
    expect(bazel.agentCalls()[0].prompt).toContain('/review-fleet 101 --post');

    dialogs.accept = false;
    await ui.pasteAsComment(viewer).click();
    await expect.poll(() => dialogs.messages.length).toBe(3);
    expect(dialogs.messages[2]).toContain('already published this review on acme/api-core#101. Publish it again, as a comment?');
    expect(writes(bazel)).toEqual([]);
  });
});
