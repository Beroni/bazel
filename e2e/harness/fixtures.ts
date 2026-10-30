import { test as base, expect, Page } from '@playwright/test';
import { Bazel, BazelOptions } from './bazel';
import { ui } from './ui';

export { expect };

/**
 * knownBug marks a test that reproduces an open bug: it is expected to fail.
 * When the base under test has the fix, list the id in E2E_FIXED
 * (E2E_FIXED=BUG-QA-12) and the test must pass instead. Once the fix
 * is on origin/master, delete the knownBug line from the test.
 */
export function knownBug(id: string, what: string): void {
  const fixed = (process.env.E2E_FIXED ?? '').split(',').map((x) => x.trim());
  base.fail(!fixed.includes(id), `${id}: ${what}`);
}

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

  // No page of the test may leave the origins of this Bazel. On the context, so
  // that it covers every page, and on the parsed URL: the text of a URL says
  // little about its host (http://127.0.0.1:7777@attacker.invalid/).
  context: async ({ context, bazel }, use) => {
    await context.route('**/*', (route) => {
      const url = route.request().url();
      if (bazel.mayVisit(url)) return route.continue();
      bazel.blocked.push(url);
      return route.abort();
    });
    await use(context);
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
    await page.goto(bazel.url + '/');
    // The PR list loads after /api/state, asynchronously. Acting before it is
    // there (ticking "select all" on an empty list) is a race that only shows
    // under load: wait for the counter to say how many PRs there are.
    await expect(ui.prCount(page)).toHaveText(/^\d+( of \d+)? PRs?( · \d+ ticked)?$/, { timeout: 20_000 });
    await use(page);
  },
});
