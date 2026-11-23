import fs from 'node:fs';
import path from 'node:path';
import { test, expect } from '../harness/fixtures';
import { Bazel } from '../harness/bazel';
import { openJob, ui } from '../harness/ui';

// feat/report-format-check, beyond p1-inline-format: which reports are checked,
// that the check never stands in the way of As comment, and that it follows the
// file on disk. Bazel runs the publisher's reader (parse-report.jq) with the jq
// on its PATH: /usr/bin/jq on macOS.
const K = 'acme/api-core#101';
const TITLE = 'Add rate limiter to the public API';
const plain = (name: string, scenario: string, extra: Record<string, unknown> = {}) =>
  ({ name, task: '/review-fleet {{number}}', command: 'fake-agent', args: [], format: 'plain', env: { FAKE_AGENT_SCENARIO: scenario }, ...extra });
const AGENTS = [
  { name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } },
  { name: 'builtin', task: '/bazel-review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'bazel-review-fleet' } },
  plain('plain', 'plain-report'),
  plain('stays', 'plain-report', { publishable: false }),
];
const PIPELINES = [{ name: 'mixed', steps: ['builtin', 'stays', 'publish'] }];
const comments = (bz: Bazel) => bz.ghCalls().filter((c) => c.argv[0] === 'pr' && c.argv[1] === 'comment');

async function done(bazel: Bazel, agent: string, key = K) {
  const id = await bazel.enqueue(key, agent);
  return bazel.waitJob(id, (j) => j.state === 'done');
}
const savedOf = async (bazel: Bazel, n: number) => (await bazel.api('GET', '/api/reviews')).json.reviews.find((r: any) => r.name.includes(`-${n}-`));

test.describe('P1 · inline format check, edges', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS, pipelines: PIPELINES } } });

  test('a report about another PR is caught too', async ({ bazel }) => {
    expect((await done(bazel, 'builtin', 'acme/api-core#102')).inline_issue).toContain('a report header names another PR');
  });

  test('a report the publisher reads has no warning: the built-in fleet, and a pipeline with a step that stays', async ({ bazel }) => {
    // The built-in scenario's report is about #101, and the reader checks that too.
    expect((await done(bazel, 'builtin')).inline_issue ?? '').toBe('');
    expect((await done(bazel, 'mixed')).inline_issue ?? '').toBe('');
    expect((await bazel.api('GET', '/api/reviews')).json.reviews.filter((r: any) => r.inline_issue)).toEqual([]);
  });

  test('a report that is not names what is missing, for its step', async ({ bazel }) => {
    expect((await done(bazel, 'plain')).inline_issue).toContain('the step "plain" has no report');
  });

  test('a review that does not go to the PR is not checked', async ({ bazel }) => {
    expect((await done(bazel, 'stays')).inline_issue ?? '').toBe('');
  });

  test('As comment goes in spite of the warning, from the job and from Saved, with no question about the format', async ({ app, bazel, dialogs }) => {
    expect((await done(bazel, 'review-fleet')).inline_issue).toBeTruthy();
    const viewer = await openJob(app, TITLE);
    await ui.pasteAsComment(viewer).click();
    await expect.poll(() => comments(bazel).length).toBe(1);
    expect(dialogs.messages.join('\n')).not.toContain('inline publisher');

    await done(bazel, 'review-fleet', 'acme/api-core#102');
    const saved = await savedOf(bazel, 102);
    expect(saved.inline_issue).toBeTruthy();
    expect((await bazel.api('POST', `/api/reviews/${saved.name}/comment`, {})).status).toBe(200);
    expect(comments(bazel)).toHaveLength(2);
    expect(comments(bazel)[1].stdin).toContain('E2E-FINDING-ONE');
  });

  test('Saved marks the row and the Publish confirm says why, and the choice stays yours', async ({ app, bazel, dialogs }) => {
    await done(bazel, 'review-fleet');
    await app.reload();
    await ui.savedTab(app).click();
    const row = app.getByTestId('saved-row').first();
    await expect(row.getByTestId('saved-inline-issue')).toHaveText('not inline');
    await row.click();
    await expect(ui.viewer(app).getByTestId('review-inline-issue')).toContainText('You can still paste it As comment');
    dialogs.accept = false;
    await app.getByTestId('saved-publish').click();
    await expect.poll(() => dialogs.messages.length).toBe(1);
    expect(dialogs.messages[0]).toContain('The inline publisher will likely refuse it');
    expect(bazel.agentCalls().filter((c) => c.scenario === 'post-claude')).toEqual([]);
  });

  test('the warning follows the file: edited out of format it appears, written back it goes', async ({ bazel }) => {
    await done(bazel, 'builtin');
    const saved = await savedOf(bazel, 101);
    const file = path.join(bazel.reviewsDir, saved.name);
    const good = fs.readFileSync(file, 'utf8');
    const read = async () => (await bazel.api('GET', `/api/reviews/${saved.name}`)).json.inline_issue ?? '';
    expect(await read()).toBe('');
    fs.writeFileSync(file, good.replace(/review-fleet/g, 'something-else'));
    expect(await read()).toContain('has no report');
    fs.writeFileSync(file, good);
    fs.utimesSync(file, new Date(), new Date(Date.now() + 2000));
    expect(await read()).toBe('');
  });

  test('the list checks the newest 50; an older one says it when opened', async ({ bazel }) => {
    await done(bazel, 'review-fleet');
    const [saved] = (await bazel.api('GET', '/api/reviews')).json.reviews;
    const body = fs.readFileSync(path.join(bazel.reviewsDir, saved.name), 'utf8');
    for (let i = 0; i < 55; i++) {
      const f = path.join(bazel.reviewsDir, `acme-api-core-101-20260101-0000${String(i).padStart(2, '0')}.md`);
      fs.writeFileSync(f, body);
      const t = new Date(Date.UTC(2026, 0, 1, 0, 0, i));
      fs.utimesSync(f, t, t);
    }
    const list = (await bazel.api('GET', '/api/reviews')).json.reviews;
    expect(list).toHaveLength(56);
    expect(list.filter((r: any) => r.inline_issue)).toHaveLength(50);
    expect(list.at(-1).inline_issue ?? '').toBe('');
    expect((await bazel.api('GET', `/api/reviews/${list.at(-1).name}`)).json.inline_issue).toContain('has no report');
  });
});

test.describe('P1 · inline format check, a post agent of your own', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS, post_agent: { name: 'my-poster', task: '/my-poster {{review_file}}', posts: true, env: { FAKE_AGENT_SCENARIO: 'post-claude' } } } } });

  test('has rules of its own: Bazel does not judge the report for it', async ({ bazel }) => {
    expect((await done(bazel, 'review-fleet')).inline_issue ?? '').toBe('');
  });
});
