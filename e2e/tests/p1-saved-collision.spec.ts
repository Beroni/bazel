import fs from 'node:fs';
import { test, expect } from '../harness/fixtures';

// BUG-QA-02, fixed in #27: two reviews of the same PR finishing in the same
// second used to get the same <repo>-<number>-<YYYYMMDD-HHMMSS>.md, and the
// second overwrote the first. Now O_EXCL with a -2, -3… suffix.
test.describe('P1 · saved reviews', () => {
  test.use({
    bazelOptions: {
      config: {
        agents: [
          { name: 'fleet-gated', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings', FAKE_AGENT_GATE: 'release-gate' } },
          { name: 'plain-gated', task: '/exploit-digger {{number}}', command: 'fake-agent', args: [], format: 'plain', env: { FAKE_AGENT_SCENARIO: 'plain-report', FAKE_AGENT_GATE: 'release-gate' } },
        ],
      },
    },
  });

  test('two reviews of one PR finishing in the same second keep two files', async ({ bazel }) => {
    const a = await bazel.enqueue('acme/api-core#101', 'fleet-gated');
    const b = await bazel.enqueue('acme/api-core#101', 'plain-gated');
    await expect.poll(() => bazel.agentCalls().length, { timeout: 25_000 }).toBe(2);
    // Release right after a second boundary so both land in the same second.
    while (Date.now() % 1000 > 100) await new Promise((r) => setTimeout(r, 5));
    bazel.release('release-gate');
    const ja = await bazel.waitJob(a, (j) => j.state === 'done');
    const jb = await bazel.waitJob(b, (j) => j.state === 'done');

    expect(ja.saved_to).not.toBe(jb.saved_to);
    expect(bazel.savedReviews()).toHaveLength(2);
    expect(fs.readFileSync(ja.saved_to, 'utf8')).toContain('E2E-FINDING-ONE');
    expect(fs.readFileSync(jb.saved_to, 'utf8')).toContain('E2E-PLAIN-REPORT');
  });
});
