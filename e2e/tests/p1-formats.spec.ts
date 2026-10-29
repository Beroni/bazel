import fs from 'node:fs';
import path from 'node:path';
import { test, expect } from '../harness/fixtures';

const plainArgs = { command: 'fake-agent', args: [] as string[] };
const AGENTS = [
  { name: 'codex', task: 'Review this PR.', ...plainArgs, args: ['exec', '--json', '--ephemeral', '-'], format: 'codex-json', env: { FAKE_AGENT_SCENARIO: 'codex-review' } },
  { name: 'codex-fail', task: 'Review this PR.', ...plainArgs, args: ['exec', '--json', '-'], format: 'codex-json', env: { FAKE_AGENT_SCENARIO: 'codex-turn-failed' } },
  { name: 'grok', task: 'Review this PR.', ...plainArgs, args: ['--prompt-file', '/dev/stdin', '--output-format', 'streaming-json'], format: 'grok-stream', env: { FAKE_AGENT_SCENARIO: 'grok-review' } },
  { name: 'grok-turns', task: 'Review this PR.', ...plainArgs, args: ['--prompt-file', '/dev/stdin'], format: 'grok-stream', env: { FAKE_AGENT_SCENARIO: 'grok-max-turns' } },
  { name: 'example-skill', task: '/example-skill {{number}}', env: { FAKE_AGENT_SCENARIO: 'example-skill' } },
  { name: 'dup-lenses', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'dup-lenses' } },
];

const texts = async (bazel: any, id: string, agent?: string) => {
  const { json } = await bazel.api('GET', `/api/jobs/${id}/log?from=0`);
  return json.lines.filter((l: any) => !agent || l.agent === agent).map((l: any) => l.text);
};

test.describe('P1 · agent output formats', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS } } });

  test('codex-json: commands in the log, last agent_message is the report, cached input counted once', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'codex');
    const job = await bazel.waitJob(id, (j) => j.state === 'done' || j.state === 'failed');
    expect(job.state).toBe('done');
    expect(job.body).toContain('E2E-CODEX-FINDING');
    expect(job.body).not.toContain('duplicate delivery');
    const log = await texts(bazel, id);
    expect(log).toContain('→ git diff main...HEAD E2E-CODEX-CMD');
    expect(log).toContain('  ✗ command failed: E2E-CODEX-CMDERR No such file or directory');
    expect(log).toContain('✓ done · 52k tokens');
    expect(job.tokens).toBe(20000 + 30000 + 2000);
    expect(bazel.agentCalls()[0].argv).toEqual(['exec', '--json', '--ephemeral', '-']);
  });

  test('codex-json: turn.failed fails the job with the turn message', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'codex-fail');
    const job = await bazel.waitJob(id, (j) => j.state === 'failed' || j.state === 'done');
    expect(job.state).toBe('failed');
    expect(job.error).toBe('agent `fake-agent` failed: E2E-CODEX-TURNFAIL usage limit reached');
  });

  test('grok-stream: text joined into the report, grep no-match, cost from end; GROK_HOME isolated and removed', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'grok');
    const job = await bazel.waitJob(id, (j) => j.state === 'done' || j.state === 'failed');
    expect(job.state).toBe('done');
    expect(job.body).toContain('E2E-GROK-REPORT');
    expect(job.body).toContain('E2E-GROK-FINDING');
    const log = await texts(bazel, id);
    expect(log).toContain('→ grep -rn E2E-GROK-GREP .');
    expect(log).toContain('  · grep: no matches');
    expect(log).toContain('→ go vet ./... E2E-GROK-CMD');
    expect(log).toContain('✓ done · 4,4k tokens · $0.07');
    expect(log.some((t: string) => t.includes('E2E-GROK-REPORT'))).toBe(false); // token stream stays out of the log

    const call = bazel.agentCalls()[0];
    const runtime = path.join(bazel.bazelHome, 'grok-runtime');
    expect(call.grok_env.GROK_HOME.startsWith(runtime + '/')).toBe(true);
    expect(call.grok_home_config).toBe(true);
    expect(call.grok_env.GROK_MEMORY).toBe('0');
    expect(call.grok_env.GROK_CLAUDE_SKILLS_ENABLED).toBe('false');
    expect(fs.readdirSync(runtime)).toEqual([]); // the per-run home is gone
  });

  test('grok-stream: max turns becomes one clear error', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'grok-turns');
    const job = await bazel.waitJob(id, (j) => j.state === 'failed' || j.state === 'done');
    expect(job.state).toBe('failed');
    expect(job.error).toBe('Grok reached its configured turn limit before completing the review');
  });

  test('a synthesized spec (fixtures/agent/specs) plays like a recorded stream', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'example-skill');
    const job = await bazel.waitJob(id, (j) => j.state === 'done' || j.state === 'failed');
    expect(job.state).toBe('done');
    expect(job.body).toContain('E2E-SPEC-FINDING');
    expect(await texts(bazel, id, 'senior-code-reviewer')).toContain('E2E-SPEC-LENS precision pass done');
    expect(await texts(bazel, id, 'example-skill')).toContain('  ✗ E2E-SPEC-TOOLERR file does not exist');
    expect(job.tokens).toBe(12500);
    expect(bazel.agentCalls()[0].prompt.startsWith('/example-skill 101')).toBe(true);
  });

  test('two lenses of the same type get two terminals: exploit-digger and exploit-digger 2', async ({ bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'dup-lenses');
    await bazel.waitJob(id, (j) => j.state === 'done');
    expect(await texts(bazel, id, 'exploit-digger')).toContain('E2E-DUP-A');
    expect(await texts(bazel, id, 'exploit-digger 2')).toContain('E2E-DUP-B');
  });
});
