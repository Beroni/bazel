import fs from 'node:fs';
import path from 'node:path';
import { test as base, expect, Page } from '@playwright/test';
import { Bazel, BazelOptions } from './bazel';
import os from 'node:os';
import { COVERAGE } from './paths';
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

  // With E2E_COVERAGE=1 every page records V8's coverage of app.js, and
  // tools/coverage.mjs adds the runs up. Nothing to instrument, nothing to build.
  page: async ({ page }, use, testInfo) => {
    if (!process.env.E2E_COVERAGE) return use(page);
    await page.coverage.startJSCoverage({ resetOnNavigation: false });
    await use(page);
    const app = (await page.coverage.stopJSCoverage()).filter((e) => e.url.endsWith('/static/app.js'));
    fs.mkdirSync(COVERAGE, { recursive: true });
    fs.writeFileSync(path.join(COVERAGE, `${testInfo.testId}-${testInfo.repeatEachIndex}.json`), JSON.stringify(app));
  },

  bazel: async ({ bazelOptions }, use, testInfo) => {
    const bz = new Bazel(bazelOptions);
    // The machine is shared: a failure under load reads differently from one
    // on an idle box, so the load average goes with the timeline.
    const load: string[] = [];
    const sample = () => load.push(`${new Date().toISOString().slice(11, 23)} ${os.loadavg().map((x) => x.toFixed(1)).join(' ')}`);
    sample();
    const sampler = setInterval(sample, 2_000);
    try {
      await bz.start();
      await use(bz);
      // The canary: every process Bazel started, the first gh call at boot
      // included, saw this test's HOME and BAZEL_HOME. Nothing else says it
      // positively: a server on the real ones would look just the same.
      const seen = [...bz.ghCalls(), ...bz.agentCalls()];
      expect(seen.length, 'no gh or agent call to check the isolation with').toBeGreaterThan(0);
      expect(seen.filter((c) => c.home !== bz.home || c.bazel_home !== bz.bazelHome).map((c) => `${c.home} ${c.bazel_home}`), 'isolation broken: a process left the temp root').toEqual([]);
      // A gh call the fake does not know means Bazel grew a new GitHub
      // interaction that no test covers yet: fail loudly instead of passing.
      const unknown = bz.ghCalls().filter((c) => c.unsupported);
      expect(unknown.map((c) => 'gh ' + c.argv.join(' ')), 'gh calls the fake does not implement').toEqual([]);
    } finally {
      clearInterval(sampler);
      await bz.stop();
      const failed = testInfo.status !== testInfo.expectedStatus;
      if (failed) {
        await testInfo.attach('timeline.txt', { body: timeline(bz, load), contentType: 'text/plain' });
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

/**
 * Every gh and agent call of the test, in order, with how long it took, and the
 * load average every 2s: what a failure needs to tell a slow machine from a
 * step that never happened.
 */
function timeline(bz: Bazel, load: string[]): string {
  const ms = (a: string, b: string) => `${Math.round(Date.parse(b) - Date.parse(a))}ms`;
  const rows = [
    ...bz.ghCalls().map((c) => [c.started_at ?? c.at, `gh ${c.argv.slice(0, 4).join(' ')} → ${c.exit} (${c.started_at ? ms(c.started_at, c.at) : '?'})`]),
    ...bz.agentCalls().map((c) => [c.started_at ?? c.at, `agent ${c.scenario} pid ${c.pid} (start→record ${c.started_at ? ms(c.started_at, c.at) : '?'})`]),
  ].sort((a, b) => a[0].localeCompare(b[0]));
  return [...rows.map(([t, e]) => `${t.slice(11, 23)} ${e}`), '', 'load average (1m 5m 15m):', ...load].join('\n') + '\n';
}

