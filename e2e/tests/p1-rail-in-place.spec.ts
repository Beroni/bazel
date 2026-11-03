import { test, expect } from '../harness/fixtures';
import { ui } from '../harness/ui';

// BUG-QA-28: while a job runs the rail updates every second. The cards must stay
// the same nodes, or a click whose press and release straddle an update is lost.
test.describe('P1 · the rail updates in place', () => {
  test('a running job ticks without replacing any card, and a straddling click opens', async ({ app, bazel }) => {
    const done = await bazel.enqueue('acme/web-app#7', 'review-fleet');
    await bazel.waitJob(done, (j) => j.state === 'done');
    const running = await bazel.enqueue('acme/api-core#101', 'slow-lens');
    await bazel.waitJob(running, (j) => j.state === 'running');
    await app.reload();
    const card = ui.jobCard(app, 'Dark mode toggle in settings');
    await expect(card).toBeVisible();
    const replaced = await app.evaluate(() => new Promise<{ removed: number; ticks: number }>((resolve) => {
      const rail = document.querySelector('[data-testid=job-card]')!.parentElement!;
      let removed = 0;
      let ticks = 0;
      const o = new MutationObserver((records) => {
        for (const r of records) {
          ticks++;
          for (const n of r.removedNodes) if ((n as Element).dataset?.testid === 'job-card') removed++;
        }
      });
      o.observe(rail, { childList: true, subtree: true, attributes: true, characterData: true });
      setTimeout(() => { o.disconnect(); resolve({ removed, ticks }); }, 2500);
    }));
    expect(replaced.ticks).toBeGreaterThan(0); // it did update
    expect(replaced.removed).toBe(0);

    const box = (await card.boundingBox())!;
    await app.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    await app.mouse.down();
    await app.waitForTimeout(1200); // across at least one update
    await app.mouse.up();
    await expect(ui.viewer(app)).toHaveAttribute('data-job-id', done);
    bazel.release('release-slow');
  });

  test('keyboard focus stays on the card it was on', async ({ app, bazel }) => {
    const running = await bazel.enqueue('acme/api-core#101', 'slow-lens');
    await bazel.waitJob(running, (j) => j.state === 'running');
    await app.reload();
    const card = ui.jobCard(app, 'Add rate limiter to the public API');
    await card.focus();
    await app.waitForTimeout(1500);
    await expect(card).toBeFocused();
    bazel.release('release-slow');
  });
});
