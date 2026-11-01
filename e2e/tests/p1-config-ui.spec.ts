import { test, expect } from '../harness/fixtures';
import { jobTab, openJob, ui } from '../harness/ui';

// The Config page and the corners of the dashboard, driven by hand. These are
// what `make e2e-cover` listed as never reached (issue #10).
const T = 'Add rate limiter to the public API';
const fleet = { name: 'review-fleet', task: '/review-fleet {{number}}', env: { FAKE_AGENT_SCENARIO: 'claude-findings' } };
const plain = { name: 'plain-lens', task: '/exploit-digger {{number}}', command: 'fake-agent', args: [], format: 'plain', env: { FAKE_AGENT_SCENARIO: 'plain-report' } };

test.describe('P1 · config by hand', () => {
  test.use({ bazelOptions: { config: { agents: [fleet, plain] } } });

  test('add a repository, see its PRs, remove it, and they go', async ({ app, bazel }) => {
    bazel.addPR({ repo: 'acme/extra', number: 1, title: 'From the extra repo', headRefName: 'e', headRefOid: 'e1', author: { login: 'z' } });
    await app.getByTestId('nav-config').click();
    await app.getByTestId('repo-input').fill('acme/extra');
    await app.getByTestId('repo-input').press('Enter');
    const row = app.getByTestId('repo-row').filter({ hasText: 'acme/extra' });
    await expect(row).toBeVisible();
    await expect(app.getByTestId('repo-input')).toHaveValue('');
    expect(bazel.configText()).toContain('acme/extra');

    await app.getByTestId('nav-dashboard').click();
    await expect(ui.prRow(app, 'From the extra repo')).toBeVisible();
    await app.getByTestId('nav-config').click();
    await row.getByTestId('repo-remove').click();
    await expect(row).toHaveCount(0);
    await app.getByTestId('nav-dashboard').click();
    await expect(ui.prRow(app, T)).toBeVisible();
    await expect(app.getByText('From the extra repo')).toHaveCount(0);
  });

  test('a ticked PR of a removed repository is not reviewed', async ({ app, bazel }) => {
    await app.getByTestId('pr-row').filter({ hasText: 'Dark mode' }).getByTestId('pr-row-check').check();
    await app.getByTestId('pr-row').filter({ hasText: T }).getByTestId('pr-row-check').check();
    await expect(ui.prCount(app)).toHaveText('5 PRs · 2 ticked');
    await app.getByTestId('nav-config').click();
    await app.getByTestId('repo-row').filter({ hasText: 'acme/web-app' }).getByTestId('repo-remove').click();
    await expect(app.getByTestId('repo-row')).toHaveCount(1);
    await app.getByTestId('nav-dashboard').click();
    await expect(ui.prCount(app)).toHaveText('3 PRs · 1 ticked');
    await ui.runButton(app).click();
    await expect.poll(async () => (await bazel.api('GET', '/api/jobs')).json.jobs.map((j: any) => j.pr.key)).toEqual(['acme/api-core#101']);
  });

  test('skills become agents, the default moves, an agent stops publishing and goes', async ({ app, bazel, dialogs }) => {
    await app.getByTestId('nav-config').click();
    const skill = app.getByTestId('skill-row').filter({ hasText: 'exploit-digger' });
    await skill.getByTestId('skill-use').click();
    const agent = app.getByTestId('agent-row').and(app.locator('[data-agent="exploit-digger"]'));
    await expect(agent).toBeVisible();
    await expect(skill.getByTestId('skill-use')).toBeDisabled();

    await skill.getByTestId('skill-publishes').click();
    const posting = app.getByTestId('agent-row').and(app.locator('[data-agent="exploit-digger-post"]'));
    await expect(posting).toBeVisible();
    expect(bazel.configText()).toContain('--post');

    await agent.getByTestId('agent-default').click();
    await expect(agent).toHaveAttribute('data-default', 'true');
    await app.getByTestId('nav-dashboard').click();
    await expect(ui.agentSelect(app)).toHaveValue('exploit-digger');

    await app.getByTestId('nav-config').click();
    await agent.getByTestId('agent-publishable').click();
    await expect(agent.getByTestId('agent-publishable')).toContainText('output stays here');
    expect((await bazel.api('GET', '/api/agents')).json.agents.find((a: any) => a.name === 'exploit-digger').publishable).toBe(false);

    await agent.getByTestId('agent-remove').click();
    await expect(agent).toHaveCount(0);
    await app.getByTestId('nav-dashboard').click();
    await expect(ui.agentSelect(app).locator('option[value="exploit-digger"]')).toHaveCount(0);
    expect(dialogs.messages).toEqual([]);
  });

  test('a pipeline by click and by drag, with the rules on the way', async ({ app, bazel }) => {
    await app.getByTestId('nav-config').click();
    const tray = app.getByTestId('pipeline-tray');
    const chain = app.getByTestId('pipeline-step');
    const create = app.getByTestId('pipeline-create');
    await expect(create).toBeDisabled();
    await expect(tray.getByRole('button', { name: 'publish' })).toBeDisabled(); // nothing before it yet

    await tray.getByRole('button', { name: '+ review-fleet' }).click();
    await tray.getByRole('button', { name: '+ plain-lens' }).click();
    await expect(chain).toHaveCount(2);
    await expect(tray.getByRole('button', { name: '+ review-fleet' })).toBeDisabled();

    // Steps land on the gaps between cards. No testid on the gaps yet.
    const gaps = app.getByTestId('pipeline-chain').locator('.pipe-gap');
    await chain.nth(1).dragTo(gaps.first());
    await expect(chain.nth(0)).toHaveAttribute('data-step', 'plain-lens');

    await chain.nth(1).getByRole('button', { name: '×' }).click();
    await expect(chain).toHaveCount(1);
    await tray.getByRole('button', { name: '+ review-fleet' }).dragTo(gaps.first());
    await expect(chain.nth(0)).toHaveAttribute('data-step', 'review-fleet');
    await expect(chain).toHaveCount(2);
    await tray.getByRole('button', { name: 'publish' }).click();
    await expect(chain).toHaveCount(3);
    await expect(tray.getByRole('button', { name: '+' }).first()).toBeDisabled(); // closed: publish is the last

    await create.click();
    await expect(ui.banner(app)).toContainText('give the pipeline a name');
    await app.getByTestId('pipeline-name').fill('review-fleet'); // taken: the draft stays
    await app.getByTestId('pipeline-name').press('Enter');
    await expect(ui.banner(app)).toContainText('there is already');
    await expect(chain).toHaveCount(3);

    await app.getByTestId('pipeline-name').fill('by-hand');
    await create.click();
    await expect(chain).toHaveCount(0);
    const row = app.getByTestId('pipeline-row').filter({ hasText: 'by-hand' });
    await expect(row).toBeVisible();
    expect((await bazel.api('GET', '/api/agents')).json.agents.find((a: any) => a.name === 'by-hand').steps).toHaveLength(3);
    await row.getByTestId('pipeline-remove').click();
    await expect(row).toHaveCount(0);
  });
});

test.describe('P1 · no agents left', () => {
  test.use({ bazelOptions: { config: { agents: [fleet] } } });

  test('removing the last one says what is missing, and the dashboard cannot run', async ({ app }) => {
    await app.getByTestId('nav-config').click();
    await app.getByTestId('agent-row').getByTestId('agent-remove').click();
    await expect(ui.banner(app)).toContainText('No agents configured');
    await app.getByTestId('nav-dashboard').click();
    await ui.selectAll(app).check();
    await expect(ui.runButton(app)).toBeDisabled();
  });
});

test.describe('P1 · corners of the dashboard', () => {
  test.use({ bazelOptions: { config: { agents: [fleet, plain] } } });

  test('publishing a saved review from the page goes through the queue, ticks included', async ({ app, bazel, dialogs }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    await ui.savedTab(app).click();
    await app.getByTestId('saved-row').first().click();
    await ui.findingPicks(ui.viewer(app)).nth(1).uncheck();
    await app.getByTestId('saved-publish').click();
    await expect.poll(() => dialogs.messages.length).toBe(1);
    expect(dialogs.messages[0]).toContain('Publish this saved review on acme/api-core#101');
    await expect.poll(() => bazel.ghCalls().filter((c) => c.argv.some((a) => a.endsWith('/reviews'))).length, { timeout: 20_000 }).toBe(1);
    const [sent] = bazel.ghCalls().filter((c) => c.argv.some((a) => a.endsWith('/reviews')));
    expect(JSON.parse(sent.stdin).body).toContain('E2E-FINDING-ONE');
    expect(JSON.parse(sent.stdin).body).not.toContain('E2E-FINDING-TWO');
    await expect(app.getByRole('tab', { name: /Queue/ })).toHaveAttribute('aria-selected', 'true');
  });

  test('a card that went stale: the 409 explains, and the card catches up', async ({ page, bazel }) => {
    page.on('dialog', (d) => d.accept());
    await page.route('**/api/events', (route) => route.abort()); // this page hears nothing
    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    await page.goto(`${bazel.url}/`);
    await expect(ui.prCount(page)).toHaveText('5 PRs');
    const v = await openJob(page, T);
    await jobTab(page, 'Report');
    expect((await bazel.api('POST', `/api/jobs/${id}/post`, {})).status).toBe(200);
    await ui.pasteAsComment(v).click();
    await expect(ui.banner(page)).toContainText('Nothing new was sent to the PR');
    await expect(ui.alreadyPublished(v)).toBeVisible();
    expect(bazel.ghCalls().filter((c) => c.argv[1] === 'comment')).toHaveLength(1);
  });

  test('the log can be narrowed to one agent, and back', async ({ app, bazel }) => {
    const id = await bazel.enqueue('acme/api-core#101', 'review-fleet');
    await bazel.waitJob(id, (j) => j.state === 'done');
    await app.reload();
    await openJob(app, T);
    await jobTab(app, 'Live log');
    const panes = app.getByTestId('log-pane');
    await panes.and(app.locator('[data-agent="exploit-digger"]')).click();
    await expect(app.getByTestId('log-line').filter({ visible: true }).filter({ hasText: 'E2E-LENS-LINE' })).toBeVisible();
    await expect(app.getByText('E2E-MAIN-LINE')).toBeHidden();
    await panes.and(app.locator('[data-agent="all"]')).click();
    await expect(app.getByText('E2E-MAIN-LINE')).toBeVisible();
  });
});
