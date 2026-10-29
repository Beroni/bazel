import { ChildProcess, spawn } from 'node:child_process';
import fs from 'node:fs';
import net from 'node:net';
import path from 'node:path';
import { BIN, FIXTURES, OUT, SCENARIOS, safeEnv } from './paths';

/** One recorded call to the fake gh. */
export interface GhCall {
  argv: string[];
  stdin: string;
  cwd: string;
  at: string;
}

/** One recorded run of the fake agent. */
export interface AgentCall {
  scenario: string;
  argv: string[];
  cwd: string;
  prompt: string;
  builtin_skill: boolean;
  at: string;
}

export interface BazelOptions {
  /** Directory under e2e/fixtures with prs.json and gh.json. */
  fixtures?: string;
  /** Replaces keys of the default config (shallow). */
  config?: Record<string, unknown>;
  /** Extra server flags, e.g. ['--jobs', '1']. */
  flags?: string[];
  /** Skills to create under skills_dir (name → description). */
  skills?: Record<string, string>;
}

/** The skills every default Bazel has installed in its fake skills_dir. */
export const DEFAULT_SKILLS: Record<string, string> = {
  'review-fleet': 'Runs a fleet of review lenses over one diff',
  'exploit-digger': 'Adversarial sweep of a diff',
};

/**
 * defaultConfig is the config.yaml every test starts from. It is written as
 * JSON, which yaml.v3 reads as YAML. Each agent picks its recorded scenario
 * through FAKE_AGENT_SCENARIO in its env.
 */
export function defaultConfig(home: string): Record<string, unknown> {
  return {
    repos: ['acme/api-core', 'acme/web-app'],
    authors: [],
    include_drafts: true,
    skills_dir: path.join(home, 'skills'),
    agent: {
      command: 'fake-agent',
      args: ['-p', '--output-format', 'stream-json', '--verbose'],
      format: 'claude-stream',
      checkout: true,
      timeout_seconds: 60,
      env: { FAKE_AGENT_SCENARIO: 'claude-findings' },
    },
    agents: [
      { name: 'review-fleet', description: 'fake fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } },
      { name: 'slow-lens', description: 'hangs until released', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-slow' } },
      { name: 'plain-lens', description: 'plain stdout', task: '/exploit-digger {{number}}', command: 'fake-agent', args: [], format: 'plain', env: { FAKE_AGENT_SCENARIO: 'plain-report' } },
    ],
    pipelines: [],
    post_agent: {
      name: 'bazel-post-report',
      task: '/bazel-post-report {{review_file}}',
      posts: true,
      env: { FAKE_AGENT_SCENARIO: 'post-claude' },
    },
  };
}

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.unref();
    srv.on('error', reject);
    srv.listen(0, '127.0.0.1', () => {
      const { port } = srv.address() as net.AddressInfo;
      srv.close(() => resolve(port));
    });
  });
}

function readJSONL<T>(file: string): T[] {
  if (!fs.existsSync(file)) return [];
  return fs.readFileSync(file, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l) as T);
}

/**
 * Bazel is one isolated server: its own temp root holding HOME, BAZEL_HOME,
 * TMPDIR (where the PR clones go) and the fakes' state, on a free port.
 */
export class Bazel {
  readonly root: string;
  readonly home: string;
  readonly bazelHome: string;
  readonly tmp: string;
  readonly ghState: string;
  readonly agentState: string;
  readonly logFile: string;
  port = 0;
  private proc?: ChildProcess;
  private exited?: Promise<number | null>;

  constructor(private readonly opts: BazelOptions = {}) {
    fs.mkdirSync(path.join(OUT, 'runs'), { recursive: true });
    this.root = fs.mkdtempSync(path.join(OUT, 'runs', 'bz-'));
    this.home = path.join(this.root, 'home');
    this.bazelHome = path.join(this.root, 'bazel-home');
    this.tmp = path.join(this.root, 'tmp');
    this.ghState = path.join(this.root, 'gh');
    this.agentState = path.join(this.root, 'agent');
    this.logFile = path.join(this.root, 'server.log');
    for (const d of [this.home, this.bazelHome, this.tmp, this.ghState, this.agentState]) fs.mkdirSync(d, { recursive: true });
  }

  get url(): string {
    return `http://127.0.0.1:${this.port}`;
  }

  get reviewsDir(): string {
    return path.join(this.bazelHome, 'reviews');
  }

  get configPath(): string {
    return path.join(this.bazelHome, 'config.yaml');
  }

  async start(): Promise<void> {
    const cfg = { ...defaultConfig(this.home), ...(this.opts.config ?? {}) };
    fs.writeFileSync(this.configPath, JSON.stringify(cfg, null, 2));
    const skills = this.opts.skills ?? DEFAULT_SKILLS;
    for (const [name, description] of Object.entries(skills)) {
      const dir = path.join(this.home, 'skills', name);
      fs.mkdirSync(dir, { recursive: true });
      fs.writeFileSync(path.join(dir, 'SKILL.md'), `---\nname: ${name}\ndescription: ${description}\n---\n\nFake skill for e2e.\n`);
    }

    this.port = await freePort();
    const log = fs.openSync(this.logFile, 'a');
    this.proc = spawn(path.join(BIN, 'bazel'), ['--addr', `127.0.0.1:${this.port}`, '--no-splash', ...(this.opts.flags ?? [])], {
      cwd: this.root,
      env: safeEnv({
        HOME: this.home,
        BAZEL_HOME: this.bazelHome,
        TMPDIR: this.tmp,
        FAKE_GH_FIXTURES: path.join(FIXTURES, this.opts.fixtures ?? 'default'),
        FAKE_GH_STATE: this.ghState,
        FAKE_AGENT_SCENARIOS: SCENARIOS,
        FAKE_AGENT_STATE: this.agentState,
      }),
      stdio: ['ignore', log, log],
    });
    this.exited = new Promise((resolve) => this.proc!.on('exit', (code) => resolve(code)));

    const deadline = Date.now() + 15_000;
    while (Date.now() < deadline) {
      if (this.proc.exitCode !== null) {
        throw new Error(`bazel exited with ${this.proc.exitCode} before serving:\n${this.serverLog()}`);
      }
      try {
        const res = await fetch(`${this.url}/api/state`);
        if (res.ok) return;
      } catch {
        // not listening yet
      }
      await new Promise((r) => setTimeout(r, 50));
    }
    throw new Error(`bazel did not answer on ${this.url} within 15s:\n${this.serverLog()}`);
  }

  async stop(): Promise<void> {
    if (!this.proc || this.proc.exitCode !== null) return;
    this.proc.kill('SIGTERM');
    const timeout = new Promise((r) => setTimeout(r, 7_000, 'timeout'));
    if ((await Promise.race([this.exited, timeout])) === 'timeout') this.proc.kill('SIGKILL');
  }

  /** Removes the temp root. Called only when the test passed. */
  cleanup(): void {
    fs.rmSync(this.root, { recursive: true, force: true });
  }

  serverLog(): string {
    return fs.existsSync(this.logFile) ? fs.readFileSync(this.logFile, 'utf8') : '';
  }

  ghCalls(): GhCall[] {
    return readJSONL<GhCall>(path.join(this.ghState, 'calls.jsonl'));
  }

  agentCalls(): AgentCall[] {
    return readJSONL<AgentCall>(path.join(this.agentState, 'calls.jsonl'));
  }

  /** Lets a scenario blocked on `#!wait <name>` carry on. */
  release(name: string): void {
    fs.writeFileSync(path.join(this.agentState, name), '');
  }

  savedReviews(): string[] {
    if (!fs.existsSync(this.reviewsDir)) return [];
    return fs.readdirSync(this.reviewsDir).filter((f) => f.endsWith('.md'));
  }

  /** PR clones still on disk (Bazel makes them under TMPDIR as bazel-*). */
  clones(): string[] {
    return fs.readdirSync(this.tmp).filter((f) => f.startsWith('bazel-'));
  }

  // --- API helpers (same-origin: no Origin header, like curl) ---

  async api<T = any>(method: string, p: string, body?: unknown): Promise<{ status: number; json: T }> {
    const res = await fetch(this.url + p, {
      method,
      headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const text = await res.text();
    let json: any = text;
    try {
      json = JSON.parse(text);
    } catch {
      // plain-text error
    }
    return { status: res.status, json };
  }

  /** Queues a review through the API and returns the job id. */
  async enqueue(ref: string, agent: string): Promise<string> {
    const { status, json } = await this.api('POST', '/api/reviews', { refs: [ref], agent });
    if (status !== 202) throw new Error(`enqueue ${ref} with ${agent}: ${status} ${JSON.stringify(json)}`);
    return json.jobs[0].id;
  }

  async job(id: string): Promise<any> {
    return (await this.api('GET', `/api/jobs/${id}`)).json;
  }

  /** Polls a job until pred holds; returns the last view. */
  async waitJob(id: string, pred: (j: any) => boolean, timeoutMs = 15_000): Promise<any> {
    const deadline = Date.now() + timeoutMs;
    let last: any;
    while (Date.now() < deadline) {
      last = await this.job(id);
      if (pred(last)) return last;
      await new Promise((r) => setTimeout(r, 100));
    }
    throw new Error(`job ${id} did not reach the expected state; last: ${JSON.stringify(last, null, 2)}`);
  }

  async mcp(method: string, params?: unknown, id: number = 1): Promise<{ status: number; json: any }> {
    const res = await fetch(`${this.url}/mcp`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' },
      body: JSON.stringify({ jsonrpc: '2.0', id, method, params }),
    });
    return { status: res.status, json: await res.json() };
  }
}

