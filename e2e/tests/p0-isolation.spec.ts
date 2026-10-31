import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import type { TestInfo } from '@playwright/test';
import { test, expect } from '../harness/fixtures';
import { BIN, goOffline, OUT, safeEnv, SCENARIO_FILES } from '../harness/paths';
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
    expect(run.stderr).toContain('replay directive');
  });
});

test.describe('P0 · the fakes fail closed', () => {
  const state = () => {
    fs.mkdirSync(path.join(OUT, 'runs'), { recursive: true });
    return fs.mkdtempSync(path.join(OUT, 'runs', 'x-'));
  };
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
      ['api', 'user', '--jq', '.login', '--method', 'DELETE'],
      ['api', 'user', '--jq', '.login', '--input', '-'],
      ['api', '-X', 'DELETE', review, '--input', '-'],
      ['pr', 'merge', '101'],
      ['search', 'prs', '--json', 'number', '--', 'repo:acme/api-core'],
      ['pr', 'comment', '101', '--repo', 'acme/api-core', '--body', 'x'],
    ]) {
      const r = gh(args, body);
      expect(r.status, args.join(' ')).toBe(1);
      expect(r.unsupported, args.join(' ')).toBe(true);
    }
  });

  test("gh answers the post script's survey and writes", () => {
    const threads = ['api', 'graphql', '--paginate', '-f', 'owner=acme', '-f', 'repo=api-core', '-F', 'number=101', '-f', 'query=query { reviewThreads }'];
    for (const args of [
      ['api', '-X', 'POST', review, '--input', '-'],
      ['api', 'repos/acme/api-core/pulls/101/comments', '--paginate'],
      ['api', 'repos/acme/api-core/issues/101/comments', '--paginate'],
      [...'api repos/acme/api-core/pulls/101/reviews --paginate'.split(' ')],
      threads,
      ['api', '-X', 'POST', 'repos/acme/api-core/pulls/comments/7/reactions', '-f', 'content=+1'],
    ]) expect(gh(args, body).status, args.join(' ')).toBe(0);

    for (const args of [
      ['api', 'repos/acme/api-core/pulls/1/comments', '--paginate'],
      ['api', '-X', 'POST', 'repos/acme/api-core/pulls/comments/8/reactions', '-f', 'content=+1'],
      ['api', '-X', 'POST', 'repos/acme/api-core/issues/comments/7/reactions', '-f', 'content=+1'],
      ['api', 'graphql', '--paginate', '-f', 'owner=acme', '-f', 'repo=api-core', '-F', 'number=1', '-f', 'query=query { reviewThreads }'],
    ]) {
      const r = gh(args);
      expect(r.status, args.join(' ')).toBe(1);
      expect(r.unsupported, args.join(' ')).toBeFalsy();
    }
    for (const args of [
      ['api', 'graphql', '--paginate', '-f', 'query=query { viewer }'],
      ['api', '-X', 'POST', 'repos/acme/api-core/pulls/comments/7/reactions', '-f', 'content=heart'],
      ['api', 'repos/acme/api-core/pulls/101/files', '--paginate'],
    ]) expect(gh(args).unsupported, args.join(' ')).toBe(true);
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

  const agent = (testInfo: TestInfo, scenario: string, args: string[] = [], input = 'prompt') => {
    fs.writeFileSync(testInfo.outputPath('it.txt'), scenario);
    const dir = state();
    const r = spawnSync(path.join(BIN, 'fake-agent'), args, {
      env: safeEnv({ HOME: dir, FAKE_AGENT_SCENARIOS: testInfo.outputDir, FAKE_AGENT_SCENARIO: 'it', FAKE_AGENT_STATE: dir }),
      input, encoding: 'utf8',
    });
    fs.rmSync(dir, { recursive: true, force: true });
    return r;
  };

  test('a scenario with CRLF fails like one with LF', ({}, testInfo) => {
    const r = agent(testInfo, 'before\r\n#!exit 19\r\nnever\r\n');
    expect(r.status).toBe(19);
    expect(r.stdout).toBe('before\n');
    expect(agent(testInfo, 'before\r\n#!exit 19').status).toBe(19);
  });

  test('a directive with a bad number stops the scenario instead of meaning 0', ({}, testInfo) => {
    for (const line of ['#!exit nineteen', '#!sleep soon', '#!repeat many x']) {
      const r = agent(testInfo, `${line}\n`);
      expect(r.status, line).toBe(2);
      expect(r.stderr, line).toContain('bad number');
    }
  });

  test('the post agent leaves its outcome where the skill\'s script does', ({}, testInfo) => {
    const plugin = testInfo.outputPath('plugin');
    fs.mkdirSync(plugin);
    const review = testInfo.outputPath('review.md');
    fs.writeFileSync(review, 'E2E');
    const dir = state();
    const run = (fixtures: string) => spawnSync(path.join(BIN, 'fake-agent'), ['--plugin-dir', plugin], {
      env: safeEnv({ HOME: dir, FAKE_AGENT_SCENARIOS: SCENARIO_FILES, FAKE_AGENT_SCENARIO: 'post-claude', FAKE_AGENT_STATE: dir, FAKE_GH_FIXTURES: fixtures, FAKE_GH_STATE: dir }),
      input: `/bazel-post-report ${review}\nThe repository acme/api-core is cloned here, PR #101.`, encoding: 'utf8',
    });
    const fixtures = path.join(__dirname, '..', 'fixtures', 'default');
    expect(run(fixtures).status).toBe(0);
    expect(fs.readFileSync(path.join(plugin, 'publish-outcome'), 'utf8').split('\n')[0]).toBe('published');

    fs.cpSync(fixtures, path.join(dir, 'fx'), { recursive: true });
    fs.writeFileSync(path.join(dir, 'fx', 'gh.json'), JSON.stringify({ fail: { api: 'HTTP 502' } }));
    expect(run(path.join(dir, 'fx')).status).toBe(5);
    expect(fs.readFileSync(path.join(plugin, 'publish-outcome'), 'utf8').split('\n')[0]).toBe('failed');
    fs.rmSync(dir, { recursive: true, force: true });
  });
});

test.describe('P0 · the build stays home', () => {
  test('Go cannot reach a VCS for a missing module, whatever the shell says', ({}, testInfo) => {
    const bin = testInfo.outputPath('bin');
    const log = testInfo.outputPath('git.log');
    fs.mkdirSync(bin);
    fs.writeFileSync(path.join(bin, 'git'), `#!/bin/sh\necho "$@" >> "${log}"\nexit 1\n`, { mode: 0o755 });
    const dir = testInfo.outputPath('module');
    fs.mkdirSync(dir);
    fs.writeFileSync(path.join(dir, 'go.mod'), 'module probe\n\ngo 1.25\n');
    const r = spawnSync('go', ['mod', 'download', 'github.com/example/bazel-offline-probe@v1.0.0'], {
      cwd: dir, encoding: 'utf8',
      env: goOffline({ ...process.env, PATH: `${bin}:${process.env.PATH}`, GOENV: 'off', GONOPROXY: '*', GOPRIVATE: '*', GOMODCACHE: testInfo.outputPath('modcache'), GOFLAGS: '-modcacherw' }),
    });
    expect(r.status).not.toBe(0);
    expect(fs.existsSync(log), 'go called git').toBe(false);
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
