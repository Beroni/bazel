import fs from 'node:fs';
import path from 'node:path';
import { test, expect } from '../harness/fixtures';
import { expandFindings, tickByFilter, ui } from '../harness/ui';

test.describe('P0 · review', () => {
  test('a mocked claude-stream review runs from the UI to the rendered report', async ({ app, bazel }) => {
    await tickByFilter(app, 'rate limiter');
    await ui.agentSelect(app).selectOption('review-fleet');
    await ui.runButton(app).click();

    // The report lands in the viewer, findings and all.
    const viewer = ui.viewer(app);
    await expect(viewer.getByText('E2E-FINDING-ONE')).toBeVisible({ timeout: 20_000 });
    await expandFindings(viewer);
    await expect(viewer.getByText('E2E-FINDING-TWO')).toBeVisible();
    await expect(ui.findingPicks(viewer)).toHaveCount(2);
    await expect(ui.publishInline(viewer)).toBeVisible();

    // The agent ran inside a clone of the PR, with the task in the prompt…
    const calls = bazel.agentCalls();
    expect(calls).toHaveLength(1);
    expect(calls[0].scenario).toBe('claude-findings');
    expect(calls[0].prompt).toContain('/review-fleet 101');
    expect(calls[0].prompt).toContain('do not publish anything to GitHub');
    expect(path.basename(calls[0].cwd)).toMatch(/^bazel-api-core-101-/);
    // …and the clone is gone afterwards (no --keep).
    expect(bazel.clones()).toEqual([]);

    // The gh calls of a review: clone + checkout, nothing that writes.
    const verbs = bazel.ghCalls().map((c) => c.argv.slice(0, 2).join(' '));
    expect(verbs).toContain('repo clone');
    expect(verbs).toContain('pr checkout');
    expect(verbs).not.toContain('pr comment');
    expect(verbs.filter((v) => v.startsWith('api ') && v !== 'api user')).toEqual([]);

    // Saved to disk with the header and the closed spend.
    const saved = bazel.savedReviews();
    expect(saved).toHaveLength(1);
    expect(saved[0]).toMatch(/^acme-api-core-101-\d{8}-\d{6}\.md$/);
    const md = fs.readFileSync(path.join(bazel.reviewsDir, saved[0]), 'utf8');
    expect(md).toContain('# acme/api-core#101 — Add rate limiter to the public API');
    expect(md).toContain('- Agent: review-fleet');
    // Priced by Bazel's table (claude-opus-5-5), not by the stream's total_cost_usd.
    expect(md).toContain('- Spend: 1,8M tokens (in 12k · out 84k · cache 1,7M) · $2.09');
    expect(md).toContain('E2E-FINDING-TWO');

    // The PR is marked reviewed in the list.
    await ui.filter(app).fill('');
    // "✓ reviewed just now" before #25, "✓ just now" after it, "reviewed now" since #30.
    await expect(ui.reviewTag(ui.prRow(app, 'Add rate limiter to the public API'))).toHaveText(/^(✓ )?(reviewed )?(just now|now|\d+[mhd])/);
  });

  test('the live log signs each line with the agent or lens that wrote it', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    const job = await bazel.waitJob(id, (j) => j.state === 'done');
    const { json: log } = await bazel.api('GET', `/api/jobs/${id}/log?from=0`);
    const by = (agent: string) => log.lines.filter((l: any) => l.agent === agent).map((l: any) => l.text);

    expect(by('review-fleet')).toContain('→ Agent(exploit-digger): adversarial sweep');
    expect(by('review-fleet')).toContain('E2E-MAIN-LINE consolidating the findings');
    expect(by('exploit-digger')).toContain('E2E-LENS-LINE nothing exploitable in bucket.go');
    expect(by('exploit-digger')).toContain('→ Grep exec.Command');
    expect(by('review-fleet').at(-1)).toBe('✓ done in 4s · 1,8M tokens · $2.09');

    // Closed tally (modelUsage, sub-agents included), not the partial one.
    expect(job.tokens).toBe(12000 + 84000 + 1700000 + 4000);
    expect(job.tokens_partial ?? false).toBe(false);
  });

  test('a plain agent: stdout is the report', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/web-app#7', 'plain-lens');
    const job = await bazel.waitJob(id, (j) => j.state === 'done' || j.state === 'failed');
    expect(job.state).toBe('done');
    expect(job.html).toContain('E2E-PLAIN-REPORT');
    expect(job.html).toContain('E2E-PLAIN-FINDING');
    // Plain agents report no spend.
    expect(job.tokens ?? 0).toBe(0);
  });

  // BUG-QA-01: the "✓ done" log line prints the stream's total_cost_usd
  // ($2.41) while the card, the report footer and the saved file price the
  // same tokens with Bazel's table ($2.09). README › Token usage promises one
  // number. Fixed in #27: the table is the single source.
  test('the done line in the log shows the same cost as the card', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    const job = await bazel.waitJob(id, (j) => j.state === 'done');
    const { json: log } = await bazel.api('GET', `/api/jobs/${id}/log?from=0`);
    const done = log.lines.map((l: any) => l.text).find((t: string) => t.startsWith('✓ done'));
    expect(job.cost_usd).toBeCloseTo(2.088, 3);
    expect(done).toContain('$' + job.cost_usd.toFixed(2));
  });
});
