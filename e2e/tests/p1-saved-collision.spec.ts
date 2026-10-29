import fs from 'node:fs';
import { test, expect } from '../harness/fixtures';

// BUG-QA-02: store.Save names the file <repo>-<number>-<YYYYMMDD-HHMMSS>.md
// and writes it with os.WriteFile. Two reviews of the same PR that finish in
// the same second (two agents, --jobs 2) get the same path: the second
// overwrites the first, and both cards point at one file. Remove test.fail()
// once fixed.
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
    test.fail(true, 'BUG-QA-02: same-second reviews of one PR overwrite each other');
    const a = await bazel.enqueue('acme/api-core#101', 'fleet-gated');
    const b = await bazel.enqueue('acme/api-core#101', 'plain-gated');
    await expect.poll(() => bazel.agentCalls().length).toBe(2);
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
