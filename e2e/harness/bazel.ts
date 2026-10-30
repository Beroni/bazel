import { ChildProcess, spawn } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import net from 'node:net';
import path from 'node:path';
import { BIN, FIXTURES, OUT, SCENARIOS, safeEnv } from './paths';

/** One recorded call to the fake gh. */
export interface GhCall {
  argv: string[];
  stdin: string;
  cwd: string;
  exit: number;
  /** The fake does not implement this call: Bazel started using a new gh command. */
  unsupported: boolean;
  at: string;
}

/** One recorded run of the fake agent. */
export interface AgentCall {
  scenario: string;
  argv: string[];
  cwd: string;
  prompt: string;
  builtin_skill: boolean;
  /** Where bazel-post-report came from: 'plugin', 'clone' or '' (not there). */
  builtin_skill_source: string;
  /** Skills the agent could load from Bazel: --plugin-dir, or the clone on older bases. */
  skills: string[];
  /** Skills in <cwd>/.claude/skills, whoever put them there. */
  clone_skills: string[];
  /** --plugin-dir and the name in its .claude-plugin/plugin.json. */
  plugin_dir: string;
  plugin_name: string;
  /** Confinement of the PR's own Claude Code config. */
  setting_sources_user: boolean;
  strict_mcp_config: boolean;
  /** What is left of .claude, .codex, .grok and .mcp.json in the clone (Lstat). */
  agent_config: string[];
  /** `git status --porcelain` of the clone and its .git/info/exclude. */
  git_status: string;
  git_exclude: string;
  /** GROK_* env Bazel passed, and whether $GROK_HOME/config.toml existed. */
  grok_env: Record<string, string>;
  grok_home_config: boolean;
  at: string;
}

/** gh.json: who is logged in and which gh calls fail (see references/fixtures.md). */
export interface GhSettings {
  user?: string;
  fail?: Record<string, string>;
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
  /** Written as gh.json in this test's copy of the fixtures. */
  gh?: GhSettings;
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
  /** This test's own copy of the gh fixtures: mutate it with updatePR / setGh. */
  readonly fixturesDir: string;
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
    this.fixturesDir = path.join(this.root, 'fixtures');
    fs.cpSync(path.join(FIXTURES, opts.fixtures ?? 'default'), this.fixturesDir, { recursive: true });
    if (opts.gh) this.setGh(opts.gh);
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

  /**
   * Boots the server. The first call writes config.yaml and the skills; with
   * `restart()` the same BAZEL_HOME (config, reviews, index) is reused.
   */
  async start(): Promise<void> {
    if (!fs.existsSync(this.configPath)) this.writeConfig();
    await this.spawn();
  }

  /** Stops the server and boots a new process over the same BAZEL_HOME. */
  async restart(): Promise<void> {
    await this.stop();
    await this.spawn();
  }

  private writeConfig(): void {
    const cfg = { ...defaultConfig(this.home), ...(this.opts.config ?? {}) };
    fs.writeFileSync(this.configPath, JSON.stringify(cfg, null, 2));
    const skills = this.opts.skills ?? DEFAULT_SKILLS;
    for (const [name, description] of Object.entries(skills)) {
      const dir = path.join(this.home, 'skills', name);
      fs.mkdirSync(dir, { recursive: true });
      fs.writeFileSync(path.join(dir, 'SKILL.md'), `---\nname: ${name}\ndescription: ${description}\n---\n\nFake skill for e2e.\n`);
    }
  }

  private async spawn(): Promise<void> {
    this.port = await freePort();
    const log = fs.openSync(this.logFile, 'a');
    this.proc = spawn(path.join(BIN, 'bazel'), ['--addr', `127.0.0.1:${this.port}`, '--no-splash', ...(this.opts.flags ?? [])], {
      cwd: this.root,
      env: safeEnv({
        HOME: this.home,
        BAZEL_HOME: this.bazelHome,
        TMPDIR: this.tmp,
        FAKE_GH_FIXTURES: this.fixturesDir,
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
    // Jobs still in flight at stop time (a clone being written) can race the
    // removal for a moment after the server exits.
    fs.rmSync(this.root, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 });
  }

  // --- fixtures, mid-test ---

  /** Rewrites gh.json in this test's fixtures (the fake gh rereads it on every call). */
  setGh(settings: GhSettings): void {
    fs.writeFileSync(path.join(this.fixturesDir, 'gh.json'), JSON.stringify({ user: 'e2e-user', ...settings }, null, 2));
  }

  /** Patches one PR of prs.json ("owner/repo#n"), e.g. a new headRefOid. */
  updatePR(key: string, patch: Record<string, unknown>): void {
    const file = path.join(this.fixturesDir, 'prs.json');
    const prs = JSON.parse(fs.readFileSync(file, 'utf8'));
    const pr = prs.find((p: any) => `${p.repo}#${p.number}` === key);
    if (!pr) throw new Error(`no fixture PR ${key}`);
    Object.assign(pr, patch);
    fs.writeFileSync(file, JSON.stringify(prs, null, 2));
  }

  /** Adds a PR to this test's prs.json (same shape as the fixture file). */
  addPR(pr: Record<string, unknown>): void {
    const file = path.join(this.fixturesDir, 'prs.json');
    const prs = JSON.parse(fs.readFileSync(file, 'utf8'));
    prs.push({ baseRefName: 'main', isDraft: false, additions: 1, deletions: 0, changedFiles: 1, updatedAgo: '3h', reviewDecision: '', body: '', ...pr });
    fs.writeFileSync(file, JSON.stringify(prs, null, 2));
  }

  /** Reads config.yaml back (Bazel writes YAML; the harness writes JSON). */
  configText(): string {
    return fs.readFileSync(this.configPath, 'utf8');
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

  /** Per-review plugin folders left in <BAZEL_HOME>/runtime. */
  runtimes(): string[] {
    const dir = path.join(this.bazelHome, 'runtime');
    return fs.existsSync(dir) ? fs.readdirSync(dir) : [];
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
  async waitJob(id: string, pred: (j: any) => boolean, timeoutMs = 25_000): Promise<any> {
    const deadline = Date.now() + timeoutMs;
    let last: any;
    while (Date.now() < deadline) {
      last = await this.job(id);
      if (pred(last)) return last;
      await new Promise((r) => setTimeout(r, 100));
    }
    throw new Error(`job ${id} did not reach the expected state; last: ${JSON.stringify(last, null, 2)}`);
  }

  /**
   * Opens /api/events and collects the SSE `data:` payloads until close().
   * The first one is always {type: "hello", data: {instance}}.
   */
  events(): { list: any[]; next: (pred: (e: any) => boolean, ms?: number) => Promise<any>; close: () => void } {
    const ctrl = new AbortController();
    const list: any[] = [];
    (async () => {
      try {
        const res = await fetch(`${this.url}/api/events`, { signal: ctrl.signal });
        const reader = res.body!.getReader();
        const dec = new TextDecoder();
        let buf = '';
        for (;;) {
          const { value, done } = await reader.read();
          if (done) return;
          buf += dec.decode(value, { stream: true });
          let i;
          while ((i = buf.indexOf('\n\n')) >= 0) {
            const chunk = buf.slice(0, i);
            buf = buf.slice(i + 2);
            for (const line of chunk.split('\n')) if (line.startsWith('data: ')) list.push(JSON.parse(line.slice(6)));
          }
        }
      } catch {
        // aborted or server gone
      }
    })();
    const next = async (pred: (e: any) => boolean, ms = 10_000) => {
      const deadline = Date.now() + ms;
      while (Date.now() < deadline) {
        const hit = list.find(pred);
        if (hit) return hit;
        await new Promise((r) => setTimeout(r, 25));
      }
      throw new Error('no SSE event matched; got: ' + JSON.stringify(list.map((e) => e.type)));
    };
    return { list, next, close: () => ctrl.abort() };
  }

  /** A raw request with a chosen Host header (fetch does not let you set it). */
  rawRequest(method: string, p: string, headers: Record<string, string>, body?: string): Promise<{ status: number; text: string }> {
    return new Promise((resolve, reject) => {
      const req = http.request({ host: '127.0.0.1', port: this.port, method, path: p, headers }, (res) => {
        let text = '';
        res.on('data', (c) => (text += c));
        res.on('end', () => resolve({ status: res.statusCode ?? 0, text }));
      });
      req.on('error', reject);
      if (body) req.write(body);
      req.end();
    });
  }

  async mcp(method: string, params?: unknown, id: number = 1): Promise<{ status: number; json: any }> {
    const res = await fetch(`${this.url}/mcp`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' },
      body: JSON.stringify({ jsonrpc: '2.0', id, method, params }),
    });
    return { status: res.status, json: await res.json() };
  }

  /** tools/call → the tool's text, structured output and isError. */
  async tool(name: string, args: Record<string, unknown> = {}): Promise<{ isError: boolean; text: string; out: any }> {
    const { json } = await this.mcp('tools/call', { name, arguments: args });
    if (json.error) throw new Error(`MCP protocol error on ${name}: ${JSON.stringify(json.error)}`);
    const r = json.result;
    return { isError: !!r.isError, text: r.content?.[0]?.text ?? '', out: r.structuredContent };
  }
}

