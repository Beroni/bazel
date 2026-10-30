import fs from 'node:fs';
import { test, expect } from '../harness/fixtures';
import { ui } from '../harness/ui';

test.describe('P1 · saved reviews', () => {
  test('after a restart the saved tab lists the review and both publish paths still work', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    await bazel.restart();
    await app.goto(bazel.url + '/');
    await ui.savedTab(app).click();
    await expect(app.getByText('Add rate limiter to the public API').first()).toBeVisible();

    const [saved] = (await bazel.api('GET', '/api/reviews')).json.reviews;
    const one = await bazel.api('GET', `/api/reviews/${saved.name}`);
    expect(one.json.html).toContain('E2E-FINDING-ONE');

    const c = await bazel.api('POST', `/api/reviews/${saved.name}/comment`, { skip: [0] });
    expect(c.status).toBe(200);
    const [comment] = bazel.ghCalls().filter((x) => x.argv[1] === 'comment');
    expect(comment.argv).toEqual(['pr', 'comment', '101', '--repo', 'acme/api-core', '--body-file', '-']);
    expect(comment.stdin).toContain('E2E-FINDING-TWO');
    expect(comment.stdin).not.toContain('E2E-FINDING-ONE');
    expect(comment.stdin).not.toContain('- Head sha:'); // the header stays out, the review goes

    const p = await bazel.api('POST', `/api/reviews/${saved.name}/publish`, { skip: [1] });
    expect(p.status).toBe(202);
    await bazel.waitJob(p.json.id, (j) => j.state === 'done', 20_000);
    const [review] = bazel.ghCalls().filter((x) => x.argv.includes('repos/acme/api-core/pulls/101/reviews'));
    expect(JSON.parse(review.stdin).body).not.toContain('E2E-FINDING-TWO');
    // The saved file is untouched.
    expect(fs.readFileSync(`${bazel.reviewsDir}/${saved.name}`, 'utf8')).toContain('E2E-FINDING-TWO');
  });

  test('saved review names cannot escape the reviews directory', async ({ bazel }) => {
    fs.mkdirSync(bazel.reviewsDir, { recursive: true });
    for (const name of ['..%2Fconfig.yaml', '..%2F..%2Fhome%2Fx.md', '.index.json', 'config.yaml', '%2E%2E%2Fconfig.yaml']) {
      expect((await bazel.api('GET', `/api/reviews/${name}`)).status, name).toBe(404);
      expect((await bazel.api('POST', `/api/reviews/${name}/comment`, {})).status, name).toBe(400);
      expect((await bazel.api('POST', `/api/reviews/${name}/publish`, {})).status, name).toBe(400);
    }
    expect(bazel.ghCalls().filter((c) => c.argv[1] === 'comment' || c.argv[0] === 'api' && c.argv[1] !== 'user')).toEqual([]);
  });
});

test.describe('P1 · config through the API', () => {
  test('repos: add, persist, remove; junk refused', async ({ bazel }) => {
    const add = await bazel.api('POST', '/api/repos', { repo: 'acme/new-repo' });
    expect(add.status).toBe(200);
    expect(add.json.repos).toContain('acme/new-repo');
    expect(bazel.configText()).toContain('acme/new-repo');
    expect((await bazel.api('POST', '/api/repos', { repo: 'ACME/new-repo' })).json.added).toBe(false);
    expect((await bazel.api('DELETE', '/api/repos/acme/new-repo')).json.removed).toBe(true);
    expect(bazel.configText()).not.toContain('acme/new-repo');
    expect((await bazel.api('POST', '/api/repos', { repo: 'just-a-name' })).status).toBe(400);
  });

  // BUG-QA-03, fixed in #27: handleRepoAdd only checked that the text had a "/" somewhere.
  // "acme/" was stored as "acme" and a PR URL as "acme/web-app/pull/7": both
  // reached config.yaml and every `gh search prs` as a bogus repo:… term, and
  // the second could not be removed. Now: 400 unless it reduces to owner/name.
  test('a repo must be exactly owner/name', async ({ bazel }) => {
    const before = (await bazel.api('GET', '/api/repos')).json.repos;
    const file = bazel.configText();
    for (const repo of ['acme/', 'https://github.com/acme/web-app/pull/7', '/web-app', 'a/b/c']) {
      expect((await bazel.api('POST', '/api/repos', { repo })).status, repo).toBe(400);
    }
    expect((await bazel.api('GET', '/api/repos')).json.repos).toEqual(before);
    expect(bazel.configText()).toBe(file);
  });

  test('agents from installed skills: use, ⇧ publishes, and the refusals', async ({ bazel }) => {
    const use = await bazel.api('POST', '/api/agents', { skill: '/exploit-digger' });
    expect(use.status).toBe(201);
    expect(use.json.added).toBe('exploit-digger');
    const posts = await bazel.api('POST', '/api/agents', { skill: 'exploit-digger', posts: true });
    expect(posts.json.added).toBe('exploit-digger-post');
    const view = posts.json.agents.find((a: any) => a.name === 'exploit-digger-post');
    expect(view.posts).toBe(true);
    expect(bazel.configText()).toMatch(/task: \/exploit-digger \{\{number\}\} --post/);

    expect((await bazel.api('POST', '/api/agents', { skill: 'exploit-digger' })).status).toBe(400); // already there
    expect((await bazel.api('POST', '/api/agents', { skill: 'not-installed' })).status).toBe(400);
    expect((await bazel.api('POST', '/api/agents', { skill: 'bazel-post-report' })).status).toBe(400);

    const skills = (await bazel.api('GET', '/api/skills')).json;
    expect(skills.skills.map((s: any) => s.name)).toEqual(expect.arrayContaining(['bazel-post-report', 'review-fleet', 'exploit-digger']));
    expect(skills.post_skill).toBe('bazel-post-report');
  });

  test('a skill installed while running shows up without a restart', async ({ bazel }) => {
    fs.mkdirSync(`${bazel.home}/skills/history-pr`, { recursive: true });
    fs.writeFileSync(`${bazel.home}/skills/history-pr/SKILL.md`, '---\nname: history-pr\ndescription: The story of a PR. Long text.\n---\n');
    const add = await bazel.api('POST', '/api/agents', { skill: 'history-pr' });
    expect(add.status).toBe(201);
    expect(add.json.agents.find((a: any) => a.name === 'history-pr').description).toBe('The story of a PR');
  });

  test('missing skills are flagged; the built-in one never is', async ({ bazel }) => {
    fs.rmSync(`${bazel.home}/skills/exploit-digger`, { recursive: true });
    const { json } = await bazel.api('GET', '/api/agents');
    const plain = json.agents.find((a: any) => a.name === 'plain-lens');
    expect(plain.skills).toEqual([{ name: 'exploit-digger', installed: false, builtin: false, step: 'plain-lens' }]);
    const fleet = json.agents.find((a: any) => a.name === 'review-fleet');
    expect(fleet.skills[0].installed).toBe(true);
  });

  test('make default, publishable toggle and remove', async ({ bazel }) => {
    await bazel.api('POST', '/api/agents/plain-lens/default');
    expect((await bazel.api('GET', '/api/state')).json.agents[0].name).toBe('plain-lens');
    expect(bazel.configText()).toContain('default: plain-lens');
    // No agent named → the default runs.
    const q = await bazel.api('POST', '/api/reviews', { refs: ['acme/web-app#7'] });
    expect(q.json.jobs[0].agent).toBe('plain-lens');

    const off = await bazel.api('POST', '/api/agents/plain-lens/publishable', { publishable: false });
    expect(off.json.agents.find((a: any) => a.name === 'plain-lens').publishable).toBe(false);
    expect((await bazel.api('POST', '/api/agents/nobody/default')).status).toBe(404);

    const rm = await bazel.api('DELETE', '/api/agents/plain-lens');
    expect(rm.json.removed).toBe(true);
    expect(bazel.configText()).not.toContain('default: plain-lens');
  });

  test('pipeline rules are enforced on the way in', async ({ bazel }) => {
    await bazel.api('POST', '/api/agents', { skill: 'exploit-digger', posts: true });
    const bad: Array<[string[], string]> = [
      [['publish', 'review-fleet'], 'cannot be the first step'],
      [['review-fleet', 'publish', 'publish'], 'publishes at most once'],
      [['review-fleet', 'publish', 'plain-lens'], 'publish has to be the last step'],
      [['exploit-digger-post', 'publish'], 'posts to the PR on its own'],
      [['review-fleet', 'review-fleet'], 'is in this pipeline twice'],
      [['review-fleet', 'exploit-digger'], 'is not an agent'],
      [[], 'needs at least one agent'],
    ];
    for (const [steps, msg] of bad) {
      const r = await bazel.api('POST', '/api/pipelines', { name: 'p-' + steps.join('-'), steps });
      expect(r.status, steps.join(',')).toBe(400);
      expect(r.json.error ?? JSON.stringify(r.json), steps.join(',')).toContain(msg);
    }
    expect((await bazel.api('POST', '/api/pipelines', { name: 'review-fleet', steps: ['plain-lens'] })).status).toBe(400); // name taken
    const ok = await bazel.api('POST', '/api/pipelines', { name: 'gate', steps: ['review-fleet', 'Publish'] });
    expect(ok.status).toBe(201);
    expect(ok.json.agents.find((a: any) => a.name === 'gate').steps).toEqual(['review-fleet', 'publish']);
    expect((await bazel.api('DELETE', '/api/pipelines/gate')).json.removed).toBe(true);
  });

  test('config download is the file on disk', async ({ bazel }) => {
    const res = await fetch(`${bazel.url}/api/config/file`);
    expect(res.headers.get('content-disposition')).toBe('attachment; filename="config.yaml"');
    expect(await res.text()).toBe(bazel.configText());
  });
});

test.describe('P1 · server guard', () => {
  test('a non-local Host is refused on the page, the API and /mcp', async ({ bazel }) => {
    for (const p of ['/', '/api/state', '/mcp']) {
      const r = await bazel.rawRequest(p === '/mcp' ? 'POST' : 'GET', p, { Host: 'evil.example:80', 'Content-Type': 'application/json' }, p === '/mcp' ? '{"jsonrpc":"2.0","id":1,"method":"ping"}' : undefined);
      expect(r.status, p).toBe(403);
    }
    expect((await bazel.rawRequest('GET', '/api/state', { Host: `localhost:${bazel.port}` })).status).toBe(200);
  });

  test('a cross-origin POST to the API is refused before it queues anything', async ({ bazel }) => {
    const r = await fetch(`${bazel.url}/api/reviews`, {
      method: 'POST', headers: { 'Content-Type': 'application/json', Origin: 'http://127.0.0.1:1' },
      body: JSON.stringify({ refs: ['acme/api-core#101'], agent: 'review-fleet' }),
    });
    expect(r.status).toBe(403);
    expect((await bazel.api('GET', '/api/jobs')).json.jobs).toEqual([]);
  });

  // SEC-06, fixed in #30: Origin does not cover GET, so a page on another site can still make
  // the browser fire one. It cannot read the answer (no CORS), and a POST is
  // refused, but `<img src=…/api/prs?refresh=1>` runs `gh search prs` with the
  // user's login: blind, and only spends quota. A client that sends no such
  // header (curl, MCP) goes on as before.
  test('Sec-Fetch-Site cross-site or same-site is refused on /api, and only there', async ({ bazel }) => {
    for (const site of ['cross-site', 'same-site']) {
      expect((await bazel.rawRequest('GET', '/api/state', { Host: `127.0.0.1:${bazel.port}`, 'Sec-Fetch-Site': site })).status, site).toBe(403);
      expect((await bazel.rawRequest('POST', '/api/reviews', { Host: `127.0.0.1:${bazel.port}`, 'Sec-Fetch-Site': site, 'Content-Type': 'application/json' },
        JSON.stringify({ refs: ['acme/api-core#101'], agent: 'review-fleet' }))).status, site).toBe(403);
    }
    expect(bazel.agentCalls()).toEqual([]);
    for (const site of ['same-origin', 'none']) {
      expect((await bazel.rawRequest('GET', '/api/state', { Host: `127.0.0.1:${bazel.port}`, 'Sec-Fetch-Site': site })).status, site).toBe(200);
    }
    expect((await bazel.rawRequest('GET', '/', { Host: `127.0.0.1:${bazel.port}`, 'Sec-Fetch-Site': 'cross-site' })).status).toBe(200);
  });

  test('the page loads nothing from outside 127.0.0.1', async ({ page, bazel }) => {
    const outside: string[] = [];
    page.on('request', (r) => { if (!r.url().startsWith(bazel.url) && !r.url().startsWith('data:')) outside.push(r.url()); });
    await page.goto(bazel.url + '/');
    await expect(ui.prCount(page)).toHaveText('5 PRs');
    await page.goto(bazel.url + '/#/config');
    await expect(page.getByRole('heading', { name: 'watched repositories' })).toBeVisible();
    expect(outside).toEqual([]);
  });
});
