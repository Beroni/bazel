import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { test, expect } from '../harness/fixtures';
import { BIN, SCENARIOS, SYNTH, safeEnv } from '../harness/paths';
import { scrub } from '../tools/stream.mjs';

// The fences of the harness itself. A fence nobody tests is a sentence in a doc.

test.describe('P0 · the browser stays home', () => {
  test('a URL that only looks local is aborted, on any page of the context', async ({ app, bazel, context }) => {
    // A navigation: Chromium drops subresources with credentials by itself, so
    // an <img> would pass here without the filter having seen anything.
    const sneaky = `http://127.0.0.1:${bazel.port}@attacker.invalid/probe`;
    await expect(app.goto(sneaky)).rejects.toThrow();
    expect(bazel.blocked).toHaveLength(1);
    expect(new URL(bazel.blocked[0]).hostname).toBe('attacker.invalid');

    const other = await context.newPage();
    await other.goto(`${bazel.url}/`);
    await other.evaluate(() => { new Image().src = 'http://example.invalid/x.png'; });
    await expect.poll(() => bazel.blocked).toHaveLength(2);
  });

  test('what may be visited is decided on the parsed origin', async ({ bazel }) => {
    expect(bazel.mayVisit(`${bazel.url}/api/state`)).toBe(true);
    for (const url of [`http://127.0.0.1:${bazel.port}@attacker.invalid/`, `http://user:pw@127.0.0.1:${bazel.port}/`,
      `https://127.0.0.1:${bazel.port}/`, `http://127.0.0.1:${bazel.port + 1}/`, `http://localhost:${bazel.port}/`]) {
      expect(bazel.mayVisit(url), url).toBe(false);
    }
  });
});

test.describe('P0 · a capture is data', () => {
  test('scrub refuses a replay directive', () => {
    for (const line of ['#!sh touch /tmp/e2e-pwned', '  #!sh touch /tmp/e2e-pwned', '#!exit 0']) {
      expect(() => scrub(`{"type":"system"}\n${line}\n`), line).toThrow('replay directive');
    }
  });

  test('the command line says so too, and writes nothing', () => {
    const raw = path.join(SYNTH, 'x-capture.jsonl');
    fs.mkdirSync(SYNTH, { recursive: true });
    fs.writeFileSync(raw, '#!sh touch /tmp/e2e-pwned\n');
    const run = spawnSync('node', [path.join(__dirname, '..', 'tools', 'stream.mjs'), 'scrub', raw], { encoding: 'utf8' });
    expect(run.status).not.toBe(0);
    expect(run.stdout).toBe('');
    fs.rmSync(raw);
  });
});

test.describe('P0 · the fakes fail closed', () => {
  const state = () => fs.mkdtempSync(path.join(SYNTH, '..', 'x-state-'));
  const gh = (args: string[], input = '') => {
    const dir = state();
    const env = safeEnv({ HOME: dir, FAKE_GH_FIXTURES: path.join(__dirname, '..', 'fixtures', 'default'), FAKE_GH_STATE: dir });
    const r = spawnSync(path.join(BIN, 'gh'), args, { env, input, encoding: 'utf8' });
    const calls = fs.readFileSync(path.join(dir, 'calls.jsonl'), 'utf8').trim().split('\n').map((l) => JSON.parse(l));
    fs.rmSync(dir, { recursive: true, force: true });
    return { status: r.status, stderr: r.stderr, unsupported: calls.at(-1).unsupported };
  };
  const review = 'repos/acme/api-core/pulls/101/reviews';
  const body = JSON.stringify({ event: 'COMMENT', body: 'x' });

  test('gh answers only what it models', () => {
    expect(gh(['api', '--method', 'POST', review, '--input', '-'], body).status).toBe(0);
    for (const args of [
      ['api', '--method', 'DELETE', 'repos/acme/api-core'],
      ['api', '--method', 'DELETE', review, '--input', '-'],
      ['api', 'repos/acme/api-core/pulls/101/reviews'],
      ['api', 'user'],
      ['api', '-X', 'POST', review, '--input', '-'],
      ['pr', 'merge', '101'],
      ['pr', 'comment', '101', '--repo', 'acme/api-core', '--body', 'x'],
    ]) {
      const r = gh(args, body);
      expect(r.status, args.join(' ')).toBe(1);
      expect(r.unsupported, args.join(' ')).toBe(true);
    }
  });

  test('gh refuses what GitHub would', () => {
    expect(gh(['api', '--method', 'POST', 'repos/acme/api-core/pulls/999/reviews', '--input', '-'], body).status).toBe(1);
    expect(gh(['api', '--method', 'POST', review, '--input', '-'], '{"event":"COMMENT"}').status).toBe(1);
    expect(gh(['pr', 'comment', '999', '--repo', 'acme/api-core', '--body-file', '-'], 'x').status).toBe(1);
    expect(gh(['pr', 'comment', '101', '--repo', 'acme/api-core', '--body-file', '-'], '').status).toBe(1);
    expect(gh(['repo', 'clone', 'evil/repo', '/nonexistent']).status).toBe(1);
  });

  test('gh knows the --json fields of each command, from gh --help', () => {
    expect(gh(['pr', 'list', '--repo', 'acme/api-core', '--json', 'headRefOid']).status).toBe(0);
    const search = gh(['search', 'prs', '--json', 'headRefOid', '--', 'repo:acme/api-core']);
    expect(search.status).toBe(1);
    expect(search.stderr).toContain('Unknown JSON field: "headRefOid"');
    expect(gh(['search', 'prs', '--json', 'number,title', '--', 'repo:acme/api-core']).status).toBe(0);
    expect(gh(['pr', 'view', '101', '--repo', 'acme/api-core', '--json', 'nope']).status).toBe(1);
  });

  test('the agent replays a line of any size, to the end of the scenario', () => {
    fs.mkdirSync(SYNTH, { recursive: true });
    fs.writeFileSync(path.join(SYNTH, 'x-long.txt'), `before\n${'x'.repeat(2 << 20)}\nafter\n#!exit 19\nnever\n`);
    const dir = state();
    const r = spawnSync(path.join(BIN, 'fake-agent'), [], {
      env: safeEnv({ HOME: dir, FAKE_AGENT_SCENARIOS: SCENARIOS, FAKE_AGENT_SCENARIO: 'x-long', FAKE_AGENT_STATE: dir }),
      input: 'prompt', encoding: 'utf8', maxBuffer: 1 << 23,
    });
    fs.rmSync(dir, { recursive: true, force: true });
    expect(r.status).toBe(19);
    expect(r.stdout.endsWith('after\n')).toBe(true);
    expect(r.stdout.length).toBeGreaterThan(2 << 20);
  });
});
