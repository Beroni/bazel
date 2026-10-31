import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { test, expect } from '../harness/fixtures';
import { BIN, OUT, safeEnv } from '../harness/paths';
import { takeLock } from '../harness/lock';
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

  test('the command line says so too, and writes nothing', ({}, testInfo) => {
    const raw = testInfo.outputPath('capture.jsonl');
    fs.writeFileSync(raw, '#!sh touch /tmp/e2e-pwned\n');
    const run = spawnSync('node', [path.join(__dirname, '..', 'tools', 'stream.mjs'), 'scrub', raw], { encoding: 'utf8' });
    expect(run.status).not.toBe(0);
    expect(run.stdout).toBe('');
  });
});

test.describe('P0 · the fakes fail closed', () => {
  const state = () => fs.mkdtempSync(path.join(OUT, 'x-state-'));
  const gh = (args: string[], input = '', settings?: object) => {
    const dir = state();
    let fixtures = path.join(__dirname, '..', 'fixtures', 'default');
    if (settings) {
      fs.cpSync(fixtures, path.join(dir, 'fixtures'), { recursive: true });
      fixtures = path.join(dir, 'fixtures');
      fs.writeFileSync(path.join(fixtures, 'gh.json'), JSON.stringify(settings));
    }
    const env = safeEnv({ HOME: dir, FAKE_GH_FIXTURES: fixtures, FAKE_GH_STATE: dir });
    const r = spawnSync(path.join(BIN, 'gh'), args, { env, input, encoding: 'utf8' });
    const calls = fs.readFileSync(path.join(dir, 'calls.jsonl'), 'utf8').trim().split('\n').map((l) => JSON.parse(l));
    fs.rmSync(dir, { recursive: true, force: true });
    return { status: r.status, stdout: r.stdout, stderr: r.stderr, unsupported: calls.at(-1).unsupported };
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

  test('gh pr list lists what it says: state, author, newest first, and the limit', () => {
    const list = (...flags: string[]) => JSON.parse(gh(['pr', 'list', '--repo', 'acme/api-core', '--json', 'number,state', ...flags]).stdout);
    expect(list().map((p: any) => p.number)).toEqual([103, 102, 101]);
    expect(list('--limit', '2').map((p: any) => p.number)).toEqual([103, 102]);
    expect(list('--state', 'closed')).toEqual([]);
    expect(list('--state', 'all')).toHaveLength(3);
    expect(list()[0].state).toBe('OPEN');
    expect(list('--author', 'ALICE').map((p: any) => p.number)).toEqual([101]);
  });

  test('a query with a space is one phrase, as gh quotes it', () => {
    const search = (...words: string[]) => JSON.parse(gh(['search', 'prs', '--json', 'number', '--', ...words]).stdout).length;
    expect(search('repo:acme/api-core', 'repo:acme/web-app')).toBe(5);
    expect(search('repo:acme/api-core repo:acme/web-app')).toBe(0);
  });

  test('every read can fail on purpose, and pr diff takes only what Bazel sends', () => {
    expect(gh(['auth', 'status'], '', { fail: { auth: 'not logged in' } }).status).toBe(1);
    expect(gh(['api', 'user', '--jq', '.login'], '', { fail: { user: 'HTTP 401' } }).status).toBe(1);
    expect(gh(['pr', 'diff', '101', '--repo', 'acme/api-core'], '', { fail: { 'diff:acme/api-core#101': 'HTTP 502' } }).status).toBe(1);
    expect(gh(['pr', 'diff', '101', '--repo', 'acme/api-core', '--name-only']).unsupported).toBe(true);
  });

  test('the head gh lists is the commit the clone gets', ({}, testInfo) => {
    const dir = state();
    const env = safeEnv({ HOME: dir, GIT_CONFIG_NOSYSTEM: '1', FAKE_GH_FIXTURES: path.join(__dirname, '..', 'fixtures', 'default'), FAKE_GH_STATE: dir });
    const run = (cmd: string, args: string[], cwd?: string) => spawnSync(cmd, args, { env, cwd, encoding: 'utf8' });
    const [pr] = JSON.parse(run(path.join(BIN, 'gh'), ['pr', 'view', '101', '--repo', 'acme/api-core', '--json', 'headRefOid']).stdout.replace(/^/, '[').replace(/$/, ']'));
    const clone = testInfo.outputPath('clone');
    expect(run(path.join(BIN, 'gh'), ['repo', 'clone', 'acme/api-core', clone]).status).toBe(0);
    expect(run(path.join(BIN, 'gh'), ['pr', 'checkout', '101'], clone).status).toBe(0);
    expect(run('git', ['rev-parse', 'HEAD'], clone).stdout.trim()).toBe(pr.headRefOid);
    fs.rmSync(dir, { recursive: true, force: true });
  });

  test('the agent replays a line of any size, to the end of the scenario', ({}, testInfo) => {
    fs.writeFileSync(testInfo.outputPath('long.txt'), `before\n${'x'.repeat(2 << 20)}\nafter\n#!exit 19\nnever\n`);
    const dir = state();
    const r = spawnSync(path.join(BIN, 'fake-agent'), [], {
      env: safeEnv({ HOME: dir, FAKE_AGENT_SCENARIOS: testInfo.outputDir, FAKE_AGENT_SCENARIO: 'long', FAKE_AGENT_STATE: dir }),
      input: 'prompt', encoding: 'utf8', maxBuffer: 1 << 23,
    });
    fs.rmSync(dir, { recursive: true, force: true });
    expect(r.status).toBe(19);
    expect(r.stdout.endsWith('after\n')).toBe(true);
    expect(r.stdout.length).toBeGreaterThan(2 << 20);
  });
});

test.describe('P0 · one run per output directory', () => {
  test('a second run is refused while the first is alive, and a dead one does not count', ({}, testInfo) => {
    const lock = testInfo.outputPath('lock');
    const release = takeLock(lock);
    expect(fs.readFileSync(lock, 'utf8')).toBe(String(process.pid));
    expect(() => takeLock(lock)).toThrow(`another e2e run (pid ${process.pid})`);
    release();
    expect(fs.existsSync(lock)).toBe(false);

    const dead = spawnSync('/usr/bin/true', []).pid;
    fs.writeFileSync(lock, String(dead));
    takeLock(lock)();
  });
});
