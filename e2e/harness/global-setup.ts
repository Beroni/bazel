import { execFile, execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { chromium } from '@playwright/test';
import { takeLock } from './lock';
import { BAZEL_SRC, BIN, COVERAGE, FAKE_CLAUDE, FAKE_WRAPPER, goOffline, OUT, REPO, SAFE_PATH, SPECS, SYNTH, safeEnv } from './paths';

// Builds Bazel and the two fakes once per run, and lays out the only bin
// directory the server will see on PATH: the fake gh, the fake agent and git.
// The real gh lives next to node and git in Homebrew, so that directory is
// never on the server's PATH — git is linked in by itself.
export default async function globalSetup() {
  if (!fs.existsSync(chromium.executablePath())) {
    throw new Error(`the Chromium this Playwright needs is not installed (${chromium.executablePath()}): run \`make e2e-setup\` once`);
  }
  fs.mkdirSync(OUT, { recursive: true });
  const release = takeLock(path.join(OUT, 'lock'));
  fs.rmSync(BIN, { recursive: true, force: true });
  // A failed test keeps its root for debugging, until the next run.
  fs.rmSync(path.join(OUT, 'runs'), { recursive: true, force: true });
  fs.rmSync(COVERAGE, { recursive: true, force: true });
  fs.mkdirSync(BIN, { recursive: true });

  // The three builds run in parallel; Go's build cache makes a warm run cheap.
  const build = (out: string, pkg: string, cwd = REPO) =>
    new Promise<void>((resolve, reject) =>
      execFile('go', ['build', '-o', `${BIN}/${out}`, pkg], { cwd, env: goOffline(process.env) }, (err, _stdout, stderr) =>
        err ? reject(new Error(`go build ${pkg} in ${cwd} (offline; run \`go mod download\` once if a module is missing): ${stderr || err.message}`)) : resolve()));
  await Promise.all([build('bazel', '.', BAZEL_SRC), build('gh', './e2e/fakes/gh'), build('fake-agent', './e2e/fakes/agent')]);
  if (BAZEL_SRC !== REPO) console.log(`e2e: Bazel under test built from ${BAZEL_SRC}`);

  // Scenario specs (fixtures/agent/specs/*.json) → claude-stream scenarios.
  const { synthAll } = await import('../tools/stream.mjs');
  fs.rmSync(SYNTH, { recursive: true, force: true });
  synthAll(SPECS, SYNTH);

  const git = execFileSync('/bin/sh', ['-c', 'command -v git'], { encoding: 'utf8' }).trim();
  fs.symlinkSync(git, `${BIN}/git`);
  fs.rmSync(path.dirname(FAKE_CLAUDE), { recursive: true, force: true });
  fs.mkdirSync(path.dirname(FAKE_CLAUDE), { recursive: true });
  fs.symlinkSync(`${BIN}/fake-agent`, FAKE_CLAUDE);
  fs.symlinkSync(`${BIN}/fake-agent`, FAKE_WRAPPER);

  // Preflight: under the server's environment `gh` must be the fake, and
  // `claude`/`codex`/`grok` must not resolve at all.
  const env = safeEnv({ HOME: OUT });
  const which = (cmd: string) => {
    try {
      return execFileSync('/bin/sh', ['-c', `command -v ${cmd}`], { env, encoding: 'utf8' }).trim();
    } catch {
      return '';
    }
  };
  if (which('gh') !== `${BIN}/gh`) {
    throw new Error(`preflight: gh resolves to ${which('gh') || 'nothing'} under PATH=${SAFE_PATH}, not the fake`);
  }
  for (const real of ['claude', 'codex', 'grok']) {
    if (which(real)) throw new Error(`preflight: ${real} is reachable at ${which(real)} — the e2e PATH must not see it`);
  }
  const version = execFileSync('gh', ['--version'], { env, encoding: 'utf8' });
  if (!version.includes('fake')) throw new Error('preflight: gh --version is not the fake: ' + version);

  return release;
}
