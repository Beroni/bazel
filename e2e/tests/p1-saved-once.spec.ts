import fs from 'node:fs';
import path from 'node:path';
import { test, expect } from '../harness/fixtures';
import { Bazel } from '../harness/bazel';
import { ui } from '../harness/ui';

// BUG-QA-34, fixed in d2b1421: a review that went to the PR could go again from
// the Saved tab after a restart, since the guard against publishing twice lived
// in memory. Now each saved file keeps its own record (<file>.posted), and a file
// from before that, on a PR the index says was posted, asks first.
const AGENTS = [{ name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } }];
const PIPELINES = [{ name: 'read-before-sending', steps: ['review-fleet', 'publish'] }];

const reviewPosts = (bz: Bazel) => bz.ghCalls().filter((c) => c.argv[0] === 'api' && c.argv.some((a) => /\/pulls\/\d+\/reviews$/.test(a))).length;
const comments = (bz: Bazel) => bz.ghCalls().filter((c) => c.argv[0] === 'pr' && c.argv[1] === 'comment').length;
const sent = (bz: Bazel) => reviewPosts(bz) + comments(bz);

async function published(bazel: Bazel, how: 'pipeline' | 'inline' | 'comment') {
  const id = await bazel.enqueue('acme/api-core#101', how === 'pipeline' ? 'read-before-sending' : 'review-fleet');
  await bazel.waitJob(id, (j) => j.state === 'done');
  expect((await bazel.api('POST', `/api/jobs/${id}/${how === 'comment' ? 'post' : 'publish'}`, {})).status).toBeLessThan(300);
  const job = await bazel.waitJob(id, (j) => j.posted || !!j.post_error);
  expect(job.post_error ?? '').toBe('');
  expect(sent(bazel)).toBe(1);
  const [saved] = (await bazel.api('GET', '/api/reviews')).json.reviews;
  return saved.name as string;
}

test.describe('P1 · a saved review goes to the PR once, restart or not', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS, pipelines: PIPELINES } } });

  for (const how of ['pipeline', 'inline', 'comment'] as const) {
    test(`published by ${how}, then a restart: neither Saved door sends it again`, async ({ bazel }) => {
      const name = await published(bazel, how);
      await bazel.restart();
      for (const door of ['publish', 'comment']) {
        const r = await bazel.api('POST', `/api/reviews/${name}/${door}`, {});
        if (r.status === 202) await bazel.waitJob(r.json.id, (j) => j.state !== 'queued' && j.state !== 'running');
        expect(r.status, door).toBe(409);
      }
      expect(sent(bazel)).toBe(1);
    });
  }

  test('the Saved tab says it is on the PR and offers no Publish', async ({ app, bazel }) => {
    await published(bazel, 'pipeline');
    await app.reload();
    await ui.savedTab(app).click();
    await app.getByTestId('saved-row').first().click();
    await expect(app.getByTestId('already-published')).toBeVisible();
    await expect(app.getByTestId('saved-publish')).toHaveCount(0);
  });
});

test.describe('P1 · a saved review from before the record', () => {
  test.use({ bazelOptions: { config: { agents: AGENTS, pipelines: PIPELINES } } });

  // A file written before Bazel kept the record has neither the header line
  // "- Publish record: kept" nor a .posted. When the index says the PR got a
  // review, Bazel cannot tell whether it was this one: it asks.
  async function legacy(bazel: Bazel) {
    const name = await published(bazel, 'inline');
    const file = path.join(bazel.reviewsDir, name);
    fs.rmSync(`${file}.posted`);
    const body = fs.readFileSync(file, 'utf8');
    expect(body).toContain('- Publish record: kept\n');
    fs.writeFileSync(file, body.replace('- Publish record: kept\n', ''));
    await bazel.restart();
    return name;
  }

  test('the API asks (409 with confirm), and sends only with again', async ({ bazel }) => {
    const name = await legacy(bazel);
    for (const door of ['publish', 'comment']) {
      const r = await bazel.api('POST', `/api/reviews/${name}/${door}`, {});
      expect(r.status, door).toBe(409);
      expect(r.json.confirm, door).toBe(true);
    }
    expect(sent(bazel)).toBe(1);
    const yes = await bazel.api('POST', `/api/reviews/${name}/comment`, { again: true });
    expect(yes.status).toBe(200);
    expect(comments(bazel)).toBe(1);
    // and now it has its record: no more asking, a plain no.
    const after = await bazel.api('POST', `/api/reviews/${name}/publish`, { again: true });
    expect(after.status).toBe(409);
    expect(after.json.confirm ?? false).toBe(false);
    expect(sent(bazel)).toBe(2);
  });

  test('the page asks "Send it to the PR again?" after its own confirm: no sends nothing, yes sends once', async ({ app, bazel }) => {
    await legacy(bazel);
    await app.goto(bazel.url + '/'); // the restart moved the server to another port
    await ui.savedTab(app).click();
    await app.getByTestId('saved-row').first().click();
    // Answers in order: the usual "Paste … as a comment?", then the server's question.
    const asked: string[] = [];
    let answers: boolean[] = [];
    app.removeAllListeners('dialog');
    app.on('dialog', (d) => { asked.push(d.message()); return answers.shift() ? d.accept() : d.dismiss(); });

    answers = [true, false];
    await app.getByTestId('saved-comment').click();
    await expect.poll(() => asked.length).toBe(2);
    expect(asked[0]).toContain('Paste this saved review as a comment');
    expect(asked[1]).toContain('Send it to the PR again?');
    expect(sent(bazel)).toBe(1);

    answers = [true, true];
    await app.getByTestId('saved-comment').click();
    await expect.poll(() => comments(bazel)).toBe(1);
    expect(asked.slice(2)).toHaveLength(2);
    expect(asked[3]).toContain('Send it to the PR again?');
    expect(sent(bazel)).toBe(2);
  });

  test('another review of the same PR, with a record next to the first, goes without a question', async ({ bazel }) => {
    await published(bazel, 'inline');
    await bazel.waitJob(await bazel.enqueue('acme/api-core#101', 'review-fleet'), (j) => j.state === 'done');
    const files = (await bazel.api('GET', '/api/reviews')).json.reviews;
    const fresh = files.find((r: any) => !r.posted);
    expect(fresh).toBeTruthy();
    expect((await bazel.api('POST', `/api/reviews/${fresh.name}/comment`, {})).status).toBe(200);
    expect(sent(bazel)).toBe(2);
  });

  test('a record that cannot be read still counts as sent', async ({ bazel }) => {
    const name = await published(bazel, 'comment');
    fs.writeFileSync(path.join(bazel.reviewsDir, `${name}.posted`), '{broken');
    await bazel.restart();
    const r = await bazel.api('POST', `/api/reviews/${name}/comment`, { again: true });
    expect(r.status).toBe(409);
    expect(sent(bazel)).toBe(1);
  });
});

