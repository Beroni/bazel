import { test, expect } from '../harness/fixtures';
import { ui } from '../harness/ui';

// "Review changes since last review": a PR that got commits after its review
// is reviewed from the head that review recorded, which the agent gets as
// {{since}}; with no earlier review, the button is not there.
const AGENTS = [
  { name: 'review-fleet', task: '/review-fleet {{number}} --since={{since}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } },
];

test.describe('P1 · review changes since the last review', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  test('a changed PR offers it, and the agent gets the reviewed head', async ({ app, bazel }) => {
    const first = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    const done = await bazel.waitJob(first, (j) => j.state === 'done');
    // The head the first review recorded, from its saved file.
    const [first0] = (await bazel.api('GET', '/api/reviews')).json.reviews;
    const reviewedHead = /- Head sha: ([0-9a-f]{40})/.exec((await bazel.api('GET', `/api/reviews/${first0.name}`)).json.body)![1];
    expect(bazel.agentCalls()[0].prompt).toMatch(/--since=\s/);
    expect(done.since ?? '').toBe('');

    bazel.movePR('acme/api-core#101');
    const { json } = await bazel.api('GET', '/api/prs?refresh=1');
    const moved = json.prs.find((p: any) => p.key === 'acme/api-core#101');
    expect(moved.changed_since_review).toBe(true);

    await app.reload();
    await ui.prRow(app, 'Dark mode toggle in settings').getByText('Dark mode toggle in settings').click();
    await expect(app.getByTestId('review-since')).toHaveCount(0); // never reviewed
    await ui.prRow(app, 'Add rate limiter to the public API').getByText('Add rate limiter to the public API').click();
    await app.getByTestId('review-since').click();
    await expect.poll(() => bazel.agentCalls().length).toBe(2);
    const second = bazel.agentCalls()[1];
    expect(second.prompt).toMatch(/--since=[0-9a-f]{40}/);
    const since = /--since=([0-9a-f]{40})/.exec(second.prompt)![1];
    expect(since).toBe(reviewedHead);
    const jobs = (await bazel.api('GET', '/api/jobs')).json.jobs;
    const job = jobs.find((j: any) => j.id !== first);
    expect(job.since).toBe(since);
    await bazel.waitJob(job.id, (j) => j.state === 'done');
    const saved = (await bazel.api('GET', '/api/reviews')).json.reviews;
    const files = await Promise.all(saved.map((s: any) => bazel.api('GET', `/api/reviews/${s.name}`)));
    expect(files.some((f) => f.json.body.includes(`- Since: ${since}`))).toBe(true);
  });
});
