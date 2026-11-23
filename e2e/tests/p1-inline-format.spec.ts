import { test, expect } from '../harness/fixtures';
import { openJob, ui } from '../harness/ui';

// A report the inline publisher would refuse says so when it lands, with the
// publisher's own reader: in the Report, on the card and in Saved. As comment
// stays as it is.
const TITLE = 'Add rate limiter to the public API';

test.describe('P1 · inline format check', () => {
  test('a report out of format warns before Publish, and As comment still goes', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    const job = await bazel.waitJob(id, (j) => j.state === 'done');
    expect(job.inline_issue).toContain('no review-fleet header');

    await app.reload();
    const card = ui.jobCard(app, TITLE);
    await expect(card).toHaveAttribute('data-inline-issue', 'true');
    const viewer = await openJob(app, TITLE);
    await expect(viewer.getByTestId('review-inline-issue')).toContainText('not in the format Publish (inline) accepts');
    await expect(viewer.getByTestId('paste-comment')).toBeEnabled();
    app.removeAllListeners('dialog');
    let asked = '';
    app.once('dialog', (d) => { asked = d.message(); void d.dismiss(); });
    await ui.publishInline(viewer).click();
    await expect.poll(() => asked).toContain('inline publisher will likely refuse it');

    const [saved] = (await bazel.api('GET', '/api/reviews')).json.reviews;
    expect(saved.inline_issue).toContain('no review-fleet header');
    expect((await bazel.api('GET', `/api/reviews/${saved.name}`)).json.inline_issue).toContain('no review-fleet header');
  });
});
