import { defineConfig, devices } from '@playwright/test';

// Every test boots its own Bazel (own BAZEL_HOME, HOME, PATH and port), so the
// suite runs fully parallel; 6 workers keep `make e2e` under ~30s on an
// 8-core laptop. Nothing here talks to the network: the browser only ever
// opens 127.0.0.1.
export default defineConfig({
  testDir: './tests',
  outputDir: './.out/results',
  globalSetup: './harness/global-setup.ts',
  fullyParallel: true,
  workers: process.env.E2E_WORKERS ? Number(process.env.E2E_WORKERS) : 6,
  forbidOnly: !!process.env.CI,
  retries: 0,
  // Generous on purpose: the laptop is shared with other agents, and a
  // passing test never waits for these.
  timeout: 60_000,
  expect: { timeout: 10_000 },
  reporter: [['list'], ['html', { outputFolder: './.out/report', open: 'never' }]],
  use: {
    ...devices['Desktop Chrome'],
    headless: true,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    viewport: { width: 1440, height: 900 },
  },
  projects: [{ name: 'chromium' }],
});
