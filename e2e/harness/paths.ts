import path from 'node:path';

export const E2E = path.resolve(__dirname, '..');
export const REPO = path.resolve(E2E, '..');
/** Everything a run writes. E2E_OUT (relative to the repo root) lets two runs coexist. */
export const OUT = process.env.E2E_OUT ? path.resolve(REPO, process.env.E2E_OUT) : path.join(E2E, '.out');
/**
 * The Bazel source tree under test. Defaults to this repo; point it at another
 * checkout (E2E_BAZEL_SRC=../bases/redesign, relative to the repo root) to run
 * this suite against another base without copying e2e/ there. The fakes always
 * come from this repo.
 */
export const BAZEL_SRC = process.env.E2E_BAZEL_SRC ? path.resolve(REPO, process.env.E2E_BAZEL_SRC) : REPO;
export const BIN = path.join(OUT, 'bin');
export const COVERAGE = path.join(OUT, 'coverage');
/**
 * The fake agent under the name `claude`, for tests that need the command name
 * itself to matter. Used by absolute path: `claude` must stay off the server's PATH.
 */
export const FAKE_CLAUDE = path.join(OUT, 'fake-claude', 'claude');
/** The fake agent under a name that says nothing: a wrapper Bazel cannot recognize. */
export const FAKE_WRAPPER = path.join(OUT, 'fake-claude', 'review-wrapper');
export const FIXTURES = path.join(E2E, 'fixtures');
export const SCENARIO_FILES = path.join(FIXTURES, 'agent');
export const SPECS = path.join(SCENARIO_FILES, 'specs');
export const SYNTH = path.join(OUT, 'scenarios');
/** Search path of the fake agent: hand-written scenarios, then synthesized specs. */
export const SCENARIOS = [SCENARIO_FILES, SYNTH].join(':');

/**
 * The environment of `go build`: only what Go already has, a cached toolchain
 * included. GOPROXY=off covers the checksum database too, and Go refuses a
 * toolchain with GOSUMDB off, so that one stays as it is. GONOPROXY and GOPRIVATE
 * would send a module straight to its VCS, past the proxy: GOVCS shuts that door
 * too, whatever the shell or `go env -w` say.
 */
export function goOffline(env: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  return { ...env, GOPROXY: 'off', GONOPROXY: 'none', GOPRIVATE: '', GOVCS: '*:off', GOTOOLCHAIN: 'auto' };
}

// The server's whole PATH: the harness bin first, then the system dirs. No
// Homebrew, no ~/.local/bin — that is where the real gh and claude live.
export const SAFE_PATH = [BIN, '/usr/bin', '/bin', '/usr/sbin', '/sbin'].join(':');

// A dead proxy: anything that tries HTTP from inside the server fails fast
// instead of reaching the internet.
const BLACKHOLE = 'http://127.0.0.1:9';

/**
 * safeEnv is the complete environment of the server and everything it spawns.
 * It starts empty — nothing leaks from the developer's shell (GH_TOKEN,
 * ANTHROPIC_API_KEY, ~/.bazel via HOME…) — and adds only what the harness needs.
 */
export function safeEnv(extra: Record<string, string>): NodeJS.ProcessEnv {
  return {
    PATH: SAFE_PATH,
    LANG: 'en_US.UTF-8',
    TZ: 'UTC',
    BAZEL_NO_SPLASH: '1',
    NO_COLOR: '1',
    CI: '1',
    // gh: even if a real one were reached, it would have no login.
    GH_CONFIG_DIR: '/nonexistent/gh-config',
    GH_PROMPT_DISABLED: '1',
    // git: no user config, no prompts, local file:// remotes only.
    GIT_CONFIG_GLOBAL: '/dev/null',
    GIT_CONFIG_NOSYSTEM: '1',
    GIT_TERMINAL_PROMPT: '0',
    GIT_ALLOW_PROTOCOL: 'file',
    HTTP_PROXY: BLACKHOLE,
    HTTPS_PROXY: BLACKHOLE,
    ALL_PROXY: BLACKHOLE,
    http_proxy: BLACKHOLE,
    https_proxy: BLACKHOLE,
    all_proxy: BLACKHOLE,
    ...extra,
  };
}
