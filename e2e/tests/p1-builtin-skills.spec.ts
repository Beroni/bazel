import path from 'node:path';
import { test, expect } from '../harness/fixtures';
import { tickByFilter, ui } from '../harness/ui';

// The skills that ship inside Bazel (internal/skills/builtin). The fake agent
// plays what each skill prints (specs/bazel-*.json); these tests check what
// Bazel does around it: the task, the plugin the skills arrive in, the lens
// terminals, the findings the page offers, and what may be published.
const BUILTIN = [
  'bazel-exploit-digger',
  'bazel-history-pr',
  'bazel-lazy-senior-dev',
  'bazel-post-report',
  'bazel-review-fleet',
  'bazel-senior-code-reviewer',
];

const AGENTS = [
  // Kept so the agent selector is found the same way as everywhere else.
  { name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } },
  { name: 'bazel-review-fleet', task: '/bazel-review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'bazel-review-fleet' } },
  { name: 'bazel-history-pr', task: '/bazel-history-pr {{number}}', publishable: false, env: { FAKE_AGENT_SCENARIO: 'bazel-history-pr' } },
  // Codex does not load Claude Code skills: Bazel has to inline this one.
  {
    name: 'codex-fleet', task: '/bazel-review-fleet {{number}}', command: 'fake-agent',
    args: ['exec', '--json', '--ephemeral', '-'], format: 'codex-json', env: { FAKE_AGENT_SCENARIO: 'codex-review' },
  },
];

const texts = async (bazel: any, id: string, agent?: string) => {
  const { json } = await bazel.api('GET', `/api/jobs/${id}/log?from=0`);
  return json.lines.filter((l: any) => !agent || l.agent === agent).map((l: any) => l.text);
};

test.describe('P1 · built-in skills', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  test('bazel-review-fleet: slash task, every built-in in the plugin, one terminal per lens, four findings', async ({ app, bazel }) => {
    await tickByFilter(app, 'rate limiter');
    await ui.agentSelect(app).selectOption('bazel-review-fleet');
    await ui.runButton(app).click();

    const viewer = ui.viewer(app);
    await expect(viewer.getByText('E2E-BUILTIN-FINDING-ONE bucket refills past its capacity')).toBeVisible({ timeout: 20_000 });
    // 2 findings + 1 cut + 1 needs-human item, each with its own tick.
    await expect(ui.findingPicks(viewer)).toHaveCount(4);
    await expect(ui.publishInline(viewer)).toBeVisible();

    const call = bazel.agentCalls()[0];
    expect(call.prompt.startsWith('/bazel-review-fleet 101')).toBe(true);
    expect(call.prompt).toContain('do not publish anything to GitHub');
    expect([...call.skills].sort()).toEqual(BUILTIN);
    expect(call.builtin_skill_source).toBe('plugin');

    const id = (await bazel.api('GET', '/api/jobs')).json.jobs[0].id;
    expect(await texts(bazel, id, 'bazel:bazel-senior-code-reviewer')).toContain('E2E-BUILTIN-LENS-PRECISION done');
    expect(await texts(bazel, id, 'bazel:bazel-exploit-digger')).toContain('E2E-BUILTIN-LENS-RECALL done');
    expect(await texts(bazel, id, 'bazel:bazel-lazy-senior-dev')).toContain('E2E-BUILTIN-LENS-SUBTRACTION done');

    // A review never writes to GitHub on its own.
    const writes = bazel.ghCalls().filter((c) => c.argv[0] === 'api' && c.argv.includes('-X'));
    expect(writes).toEqual([]);
  });

  test('bazel-history-pr: materialized, shown, never publishable', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'bazel-history-pr');
    const job = await bazel.waitJob(id, (j) => j.state === 'done' || j.state === 'failed');
    expect(job.state).toBe('done');
    expect(job.body).toContain('E2E-BUILTIN-HISTORY');
    expect(job.publishable).toBe(false);
    expect(bazel.agentCalls()[0].skills).toContain('bazel-history-pr');
    expect(await texts(bazel, id, 'bazel-history-pr')).toContain('E2E-BUILTIN-HISTORY-LINE collected 3 commits');
  });

  test('a codex step gets the built-in skill inlined, with its arguments and the plugin path', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'codex-fleet');
    const job = await bazel.waitJob(id, (j) => j.state === 'done' || j.state === 'failed');
    expect(job.state).toBe('done');

    const call = bazel.agentCalls()[0];
    expect(call.prompt.startsWith('/bazel-review-fleet')).toBe(false);
    expect(call.prompt).toContain('Run the `bazel-review-fleet` skill below.');
    expect(call.prompt).toContain('Arguments: 101');
    expect(call.prompt).toContain('# Bazel Review Fleet');
    expect(call.prompt).not.toContain('allowed-tools:');
    // The skill body points at files in Bazel's own folder, outside the clone.
    const plugin = /`<plugin>` in them is (\S+):/.exec(call.prompt)?.[1] ?? '';
    expect(plugin.startsWith(path.join(bazel.bazelHome, 'runtime') + path.sep)).toBe(true);
    expect(call.prompt).toContain(`${plugin}/skills/bazel-review-fleet/`);
  });
});
