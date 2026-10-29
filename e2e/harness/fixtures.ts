import { test as base, expect, Page } from '@playwright/test';
import { Bazel, BazelOptions } from './bazel';

export { expect };

type Fixtures = {
  /** Per-test server options; override with test.use({ bazelOptions: … }). */
  bazelOptions: BazelOptions;
  /** A started, isolated Bazel. Its temp root is kept when the test fails. */
  bazel: Bazel;
  /** A page already on the dashboard of `bazel`, accepting every confirm(). */
  app: Page;
};

export const test = base.extend<Fixtures>({
  bazelOptions: [{}, { option: true }],

  bazel: async ({ bazelOptions }, use, testInfo) => {
    const bz = new Bazel(bazelOptions);
    try {
      await bz.start();
      await use(bz);
    } finally {
      await bz.stop();
      const failed = testInfo.status !== testInfo.expectedStatus;
      if (failed) {
        await testInfo.attach('server.log', { body: bz.serverLog(), contentType: 'text/plain' });
        await testInfo.attach('gh-calls.json', { body: JSON.stringify(bz.ghCalls(), null, 2), contentType: 'application/json' });
        await testInfo.attach('agent-calls.json', { body: JSON.stringify(bz.agentCalls(), null, 2), contentType: 'application/json' });
        await testInfo.attach('root', { body: bz.root, contentType: 'text/plain' });
      } else {
        bz.cleanup();
      }
    }
  },

  app: async ({ page, bazel }, use) => {
    // The UI asks before every write to a PR; the tests answer yes and assert
    // the question was asked where it matters.
    page.on('dialog', (d) => d.accept());
    // Belt and braces: the page must never leave 127.0.0.1.
    await page.route(/^(?!http:\/\/127\.0\.0\.1:)/, (route) => route.abort());
    await page.goto(bazel.url + '/');
    await use(page);
  },
});
