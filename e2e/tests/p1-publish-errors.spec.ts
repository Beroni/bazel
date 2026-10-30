import { test, expect } from '../harness/fixtures';
import { openJob, ui } from '../harness/ui';

// R1: a failed "paste as a comment" (gh fails → POST /post answers 502) showed
// "failed to publish: …" in the banner, and the refresh that follows the job
// event (upsertJob → loadPRs → banner(state.warning)) wiped it within
// 0.4–3 s. R1b: the button came back labelled "publish to the PR" instead of
// its own label. Fixed in #27: user action errors become an alert that only goes on ✕ or on the next action.
test.describe('P1 · a failed publish stays on screen', () => {
  test.use({ bazelOptions: { config: { agents: [{ name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } }] } } });

  test('the error survives the refresh that follows, the button keeps its label, ✕ clears it', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    bazel.setGh({ fail: { comment: 'HTTP 403: Resource not accessible by integration' } });
    const viewer = await openJob(app, 'Add rate limiter to the public API');
    const btn = ui.pasteAsComment(viewer);
    const label = (await btn.textContent())!.trim();

    // The refresh that used to wipe the banner is the PR reload after the job
    // event; wait for it (or 3.5 s, the slowest wipe seen) instead of guessing.
    const refreshed = app.waitForResponse((r) => r.url().includes('/api/prs'), { timeout: 3_500 }).catch(() => null);
    await btn.click();
    await expect(ui.banner(app)).toContainText('HTTP 403', { timeout: 20_000 });
    await bazel.waitJob(id, (j) => !!j.post_error);
    await refreshed;
    await app.waitForTimeout(300);

    // The refresh is over: whatever is on screen now is what stays. No retry window.
    await expect(ui.banner(app)).toBeVisible({ timeout: 1_000 });
    await expect(ui.banner(app)).toContainText('HTTP 403', { timeout: 1_000 });
    await expect(btn).toHaveText(label, { timeout: 1_000 });
    expect(bazel.ghCalls().filter((c) => c.argv[1] === 'comment').map((c) => c.exit)).toEqual([1]);

    await ui.bannerDismiss(app).click();
    await expect(ui.banner(app)).not.toContainText('HTTP 403');
  });
});
