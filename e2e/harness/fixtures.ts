import { test as base, expect, Page } from '@playwright/test';
import { Bazel, BazelOptions } from './bazel';

export { expect };

/** What the page's confirm() dialogs get, and what they said. */
export interface Dialogs {
  /** true = accept (default), false = dismiss. */
  accept: boolean;
  messages: string[];
}

type Fixtures = {
  /** Per-test server options; override with test.use({ bazelOptions: … }). */
  bazelOptions: BazelOptions;
  /** A started, isolated Bazel. Its temp root is kept when the test fails. */
  bazel: Bazel;
  /** Controls and records the page's confirm() dialogs. */
  dialogs: Dialogs;
  /** A page already on the dashboard of `bazel`. */
  app: Page;
};

export const test = base.extend<Fixtures>({
  bazelOptions: [{}, { option: true }],

  bazel: async ({ bazelOptions }, use, testInfo) => {
    const bz = new Bazel(bazelOptions);
    try {
      await bz.start();
      await use(bz);
      // A gh call the fake does not know means Bazel grew a new GitHub
      // interaction that no test covers yet: fail loudly instead of passing.
      const unknown = bz.ghCalls().filter((c) => c.unsupported);
      expect(unknown.map((c) => 'gh ' + c.argv.join(' ')), 'gh calls the fake does not implement').toEqual([]);
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

  dialogs: async ({}, use) => {
    await use({ accept: true, messages: [] });
  },

  app: async ({ page, bazel, dialogs }, use) => {
    // Every write to a PR asks first. Tests accept by default and can set
    // dialogs.accept = false to check that saying no sends nothing.
    page.on('dialog', (d) => {
      dialogs.messages.push(d.message());
      return dialogs.accept ? d.accept() : d.dismiss();
    });
    // Belt and braces: the page must never leave 127.0.0.1.
    await page.route(/^(?!http:\/\/127\.0\.0\.1:)/, (route) => route.abort());
    await page.goto(bazel.url + '/');
    await use(page);
  },
});
