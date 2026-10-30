// Bazel's interface. No framework and no CDN: the server is local and the page
// has to open offline, so everything it uses is embedded in the binary.
'use strict';

const state = {
  me: '',
  prs: [],
  repoErrors: [],
  jobs: [],          // newest first
  saved: [],
  repos: [],
  agents: [],         // picker choices: agents and pipelines
  limits: null,       // Claude quota: session (5h) and week (7d)
  skills: [],         // skills installed on the machine
  pipeSteps: [],      // steps the pipeline builder is putting together
  pipeName: '',       // name the builder will give the pipeline
  warning: '',        // config warning that holds for the whole session
  skillsDir: '',
  agent: '',          // name of what runs in the next review
  selected: new Set(),
  scope: 'all',
  filter: '',
  ownerFilter: '',    // '' = every owner; else the owner, lower case
  repoFilter: '',     // '' = every repo (of that owner); else its repoKey
  reviewFilter: 'all', // all | none | reviewed | changed
  hideDrafts: false,
  prsHidden: false,   // PR list collapsed, to read the review full screen
  tab: 'queue',
  activeJob: null,
  activeSaved: null,
  activePR: null,      // key of the PR open in the right-hand pane
  bodies: {},        // job id -> review html
  splits: {},        // job id -> {pub, ctx} when only part goes to the PR
  picks: {},         // 'job:<id>' | 'saved:<name>' -> Set of unticked findings
  logs: {},          // job id -> {next, lines, dropped, live, busy}
  views: {},         // job id -> tab open in its detail (see jobView)
  details: {},       // PR key -> its page (see lazy), read again after a refresh
  histories: {},     // PR key -> its history (see lazy)
  logFilter: {},     // job id -> agent picked in the live log, '' = all
  instance: null,    // id of the server process; changes when it restarts
};

const $ = (sel) => document.querySelector(sel);

// clearViewer empties the right panel and forgets the signature of what was
// drawn in it. Without that forgetting, going back to the queue tab fell into
// the renderViewer shortcut — "already drawn" — and the panel kept what the
// other tab had written.
const clearViewer = () => {
  const v = $('#viewer');
  v.innerHTML = '';
  delete v.dataset.sig;
  delete v.dataset.jobId;
  delete v.dataset.saved;
  return v;
};
// docPanel frames what is not a job — a PR, the saved list, a saved review —
// in the same panel the job detail uses, and returns the part that scrolls.
const docPanel = () => {
  const box = el('div', 'job doc');
  const body = el('div', 'job-body');
  box.append(body);
  clearViewer().append(box);
  return body;
};
// tid tags an element for the end-to-end tests — the data-testid contract in
// .claude/skills/bazel-e2e/references/selectors.md — and sets the attributes
// that say which item it is. Nothing changes on screen.
const tid = (node, id, data = {}) => {
  node.dataset.testid = id;
  for (const [k, v] of Object.entries(data)) node.dataset[k] = String(v);
  return node;
};
// operable makes a clickable element reachable and usable by keyboard: in the
// tab order, announced as a button with its name, and Enter or Space doing what
// the click does. For rows that hold other controls, where a real <button>
// can't wrap them.
const operable = (node, name, fn) => {
  node.tabIndex = 0;
  node.setAttribute('role', 'button');
  node.setAttribute('aria-label', name);
  node.addEventListener('keydown', (e) => {
    if (e.target === node && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); fn(); }
  });
  return node;
};
// prLink is the PR's key as a link to it on GitHub, in a new tab.
const prLink = (pr) => {
  const a = el('a', null, pr.key);
  a.href = pr.url;
  a.target = '_blank';
  a.rel = 'noreferrer';
  return a;
};
const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
};

async function api(path, opts = {}) {
  const headers = { 'Content-Type': 'application/json', ...(opts.headers || {}) };
  // Every write names the server instance this page knows: after a restart the
  // server refuses an action meant for a job of the old process, and says so.
  if (opts.method && opts.method !== 'GET' && state.instance) headers['X-Bazel-Instance'] = state.instance;
  const res = await fetch(path, { ...opts, headers });
  const body = await res.json().catch(() => ({}));
  if (!res.ok) {
    const err = new Error(body.error || `${res.status} ${res.statusText}`);
    err.status = res.status;
    // The page is from another server process: say so, and bring the queue,
    // the PRs and the saved list up to date before anything else is tried.
    if (res.status === 409 && body.stale) {
      err.stale = true;
      // Not the page's warning: once resynced the page is no longer stale, and
      // loadPRs puts back whatever warning the page already had. With the
      // instance adopted meanwhile, adoptInstance would do nothing and this
      // would stay up, so the resync is asked for directly.
      banner('the server restarted — this page is stale; bringing it up to date');
      api('/api/state')
        .then((st) => (st.instance === state.instance ? resync() : adoptInstance(st.instance)))
        .catch(() => {});
    }
    throw err;
  }
  return body;
}

// waiting says the card ended on a `publish` step and is waiting for you to
// choose what goes to the PR. It only holds for a finished, publishable review:
// when it failed, or when the agent's output stays in Bazel, there is nothing
// to choose and nothing to wait for.
function waiting(job) {
  return job.state === 'done' && job.awaits_publish === true && job.publishable !== false
    && !job.posted && !job.publishing;
}

// fmtTokens abbreviates the count the way the server does: a fleet review
// burns millions of tokens, and "1,8M" reads better than seven digits.
function fmtTokens(n) {
  if (!n || n < 0) return '0';
  if (n < 1000) return String(n);
  const [v, suf] = n < 1e6 ? [n / 1000, 'k'] : [n / 1e6, 'M'];
  return v.toFixed(1).replace('.0', '').replace('.', ',') + suf;
}

// gasto is the line for what the review cost. The ~ marks the partial count of
// a review in progress: it only sees the agent's main conversation, and the
// lenses it ran as sub-agents come in at the close. An agent that doesn't speak
// stream-json reports nothing, and then there is no line at all.
function gasto(job) {
  const partes = [];
  if (job.tokens) partes.push((job.tokens_partial ? '~' : '') + fmtTokens(job.tokens) + ' tokens');
  if (job.cost_usd) partes.push('$' + job.cost_usd.toFixed(2));
  return partes.join(' · ');
}

// gastoTitle explains the number where it shows up.
function gastoTitle(job) {
  return job.tokens_partial
    ? 'partial count — it closes when the agent finishes, with what its lenses spent'
    : "tokens this review's agents consumed, sub-agents included";
}

// The banner carries two kinds of message. Routine ones — the config warning,
// a repository error — are rewritten by every refresh of the list. An error
// from an action you took (a publish that failed) is an alert: it stays until
// you dismiss it or act again, because the refresh that follows a failed
// publish used to wipe it before anyone could read it.
function banner(msg) {
  state.bannerMsg = msg || '';
  drawBanner();
}

function alertError(msg) {
  state.alert = msg || '';
  drawBanner();
}

function clearAlert() {
  if (state.alert) alertError('');
}

function drawBanner() {
  const b = $('#banner');
  const msg = state.alert || state.bannerMsg;
  b.textContent = '';
  b.hidden = !msg;
  if (!msg) return;
  b.classList.toggle('alert', !!state.alert);
  b.append(el('span', null, msg));
  if (state.alert) {
    const x = el('button', 'banner-x', '✕');
    x.title = 'dismiss';
    x.setAttribute('aria-label', 'dismiss this message');
    x.addEventListener('click', clearAlert);
    b.append(x);
  }
}

// --- loading ---

async function boot() {
  wire();
  try {
    togglePRs(!!localStorage.getItem('bazel.prsHidden'));
  } catch (err) {
    togglePRs(false);
  }
  connect();
  try {
    const st = await api('/api/state');
    adoptInstance(st.instance);
    state.me = st.me;
    state.jobs = st.jobs || [];
    $('#who').textContent = '@' + st.me;
    $('#who').title = `config: ${st.config_path}\nreviews: ${st.reviews_dir}\nagent: ${st.agent.command} ${(st.agent.args || []).join(' ')}\nconcurrent reviews: ${st.concurrency}`;
    state.limits = st.limits && st.limits.at && !st.limits.at.startsWith('0001') ? st.limits : null;
    renderQuota();
    applyAgents(st.agents || []);
    state.repos = st.repos || [];
    renderRepoFilters();
    // A command written in the template runs along with every agent you pick,
    // and nothing in the result gives it away — only the spend at the end.
    if (st.stray_command) {
      state.warning = `Your agent.prompt has no {{task}} and runs ${st.stray_command} on its own —`
        + ` every agent you pick runs two commands, the one you chose and that one. Fix it in "config".`;
    } else if (!state.repos.length) {
      state.warning = 'No repositories watched — go to "config" and add one (owner/repo).';
    } else if (!selectableAgents().length) {
      state.warning = 'No agents configured — go to "config" and build your list out of the installed skills.';
    }
    banner(state.warning);
    renderJobs();
  } catch (err) {
    banner('Could not reach the server: ' + err.message);
    return;
  }
  // The page is only chosen after the state has arrived: opening the tab right
  // at #/config needs the agent list to draw anything.
  await route();
  loadPRs(false);
}

async function loadPRs(force) {
  const list = $('#pr-list');
  if (!state.prs.length) list.innerHTML = '<p class="empty">consultando o GitHub…</p>';
  $('#refresh').disabled = true;
  try {
    const data = await api('/api/prs' + (force ? '?refresh=1' : ''));
    state.prs = data.prs || [];
    state.prsLoaded = true;
    // A fresh list may mean new commits, checks or reviews: the PR pages and
    // histories read so far are read again when next drawn (review F3). The
    // server's own cache keeps that cheap.
    state.details = {};
    state.histories = {};
    // The repos on the Config page say "N open"; the count is this list.
    if (!$('#page-config').hidden) renderRepos();
    state.repoErrors = data.repo_errors || [];
    // With no repository error the banner goes back to the config warning, if
    // there is one: clearing here wiped exactly what startup had to say.
    banner(state.repoErrors.length
      ? state.repoErrors.map((e) => `⚠ ${e.repo}: ${e.error}`).join('\n')
      : state.warning);
    // The repository filter counts come from here, so it is built before the
    // list.
    renderRepoFilters();
    renderPRs();
  } catch (err) {
    list.innerHTML = '';
    const p = el('p', 'empty', 'failed: ' + err.message);
    list.append(p);
    // With no list there is nothing a ticked "select all" could have selected.
    if (!state.prsLoaded) {
      const box = $('#select-all');
      box.checked = false;
      box.indeterminate = false;
    }
  } finally {
    $('#refresh').disabled = false;
  }
}

// --- server events ---

function connect() {
  const src = new EventSource('/api/events');
  src.onopen = () => { $('#conn').classList.add('on'); $('#conn').dataset.state = 'open'; };
  src.onerror = () => { $('#conn').classList.remove('on'); $('#conn').dataset.state = 'closed'; };
  src.onmessage = (msg) => {
    let ev;
    try { ev = JSON.parse(msg.data); } catch { return; }
    if (ev.type === 'hello') adoptInstance(ev.data.instance);
    if (ev.type === 'job') upsertJob(ev.data);
    if (ev.type === 'limits') { state.limits = ev.data; renderQuota(); }
    // Another tab took this job off the queue: it goes away here too.
    if (ev.type === 'job_gone') dropJob(ev.data.id);
  };
}

// adoptInstance keeps the id of the server process. Jobs do not outlive it, so
// with a different id everything the page cached by job id (review body,
// split, ticks, log) belongs to a job that is gone. It forgets all that and
// redoes what depended on it.
function adoptInstance(id) {
  if (!id || id === state.instance) return;
  const anterior = state.instance;
  state.instance = id;
  if (anterior === null) return; // first time we hear from the server
  state.bodies = {};
  state.splits = {};
  state.logs = {};
  // Details and histories may name jobs of the old server (a history being
  // written), so they are fetched again rather than waiting on loadPRs.
  state.details = {};
  state.histories = {};
  // The tab and the log filter are per job id too: a new j1 must not open on
  // the old j1's tab, nor hide its log behind the old one's agent.
  state.views = {};
  state.logFilter = {};
  for (const key of Object.keys(state.picks)) {
    if (key.startsWith('job:')) delete state.picks[key];
  }
  state.jobs = [];
  state.activeJob = null;
  if (state.tab === 'queue' && !state.activePR && !state.activeSaved) clearViewer();
  renderJobs();
  resync();
}

// resync redoes what the server restart made stale: the session queue, the PR
// list with its badges, and the Saved tab.
async function resync() {
  try {
    const st = await api('/api/state');
    state.jobs = st.jobs || [];
    state.limits = st.limits && st.limits.at && !st.limits.at.startsWith('0001') ? st.limits : null;
    renderQuota();
    applyAgents(st.agents || []);
    renderJobs();
    if (state.tab === 'queue') renderViewer();
  } catch (err) {
    banner('Could not reach the server: ' + err.message);
    return;
  }
  loadPRs(false);
  if (state.tab === 'saved') loadSaved();
}

function upsertJob(job) {
  const i = state.jobs.findIndex((j) => j.id === job.id);
  if (i >= 0) state.jobs[i] = job;
  else state.jobs.unshift(job);

  // The review body doesn't come in the event; fetch it when it is ready.
  if (job.state === 'done' && !state.bodies[job.id]) {
    if (state.activeJob === job.id || (!state.activeJob && !state.activePR)) openJob(job.id);
  }
  renderJobs();
  if (state.activeJob === job.id) renderViewer();
  // A job of this PR that just ended changes its page (a review in the
  // timeline) or its history: both are read again.
  if (job.state !== 'queued' && job.state !== 'running') delete state.details[job.pr.key];
  if (job.history && job.state !== 'queued' && job.state !== 'running') {
    delete state.histories[job.pr.key];
    if (state.activeJob && jobView(state.jobs.find((x) => x.id === state.activeJob) || {}) === 'history') {
      delete $('#viewer').dataset.sig;
      renderViewer();
    }
  }
  syncReviewingTags();

  // A finished review changes the PR's badge in the list: reload from the
  // server cache (which costs no GitHub call) so the ✓ shows up right away.
  if ((job.state === 'done' || job.posted) && !state.reloadingPRs) {
    state.reloadingPRs = true;
    loadPRs(false).finally(() => { state.reloadingPRs = false; });
  }
}

// dropJob forgets a job: it leaves the queue and, if it was the one open, the
// right panel goes back to the start.
function dropJob(id) {
  const antes = state.jobs.length;
  state.jobs = state.jobs.filter((j) => j.id !== id);
  if (state.jobs.length === antes) return;
  delete state.bodies[id];
  delete state.splits[id];
  delete state.picks[pickKey('job', id)];
  delete state.logs[id];
  syncReviewingTags();
  delete state.views[id];
  delete state.logFilter[id];
  if (state.activeJob === id) {
    state.activeJob = null;
    clearViewer();
  }
  renderJobs();
  if (state.tab === 'queue') renderViewer();
}

// removeJob takes the job off the queue on the server. A review still alive
// dies with it, and that is worth a question: it is work in progress going away.
async function removeJob(id, btn) {
  const job = state.jobs.find((j) => j.id === id);
  if (job && (job.state === 'queued' || job.state === 'running')) {
    if (!confirm(`This review is still running. Dropping it from the queue cancels it.\n\nDrop it anyway?`)) return;
  }
  btn.disabled = true;
  try {
    await api('/api/jobs/' + encodeURIComponent(id), { method: 'DELETE' });
    dropJob(id);
  } catch (err) {
    banner(err.message);
    btn.disabled = false;
  }
}

// --- actions ---

function startReview() {
  clearAlert();
  return reviewRefs(markedPRs());
}

async function reviewRefs(refs) {
  if (!refs.length) return;
  // Picking an agent that publishes is writing on someone else's PR: it is worth
  // a question first, the same ceremony as the "publish to the PR" button.
  const chosen = state.agents.find((a) => a.name === state.agent);
  if (chosen && chosen.posts) {
    const alvo = refs.length > 1 ? `${refs.length} PRs` : refs[0];
    if (!confirm(`The agent "${chosen.name}" publishes the review straight to ${alvo}. Run it anyway?`)) return;
  }
  $('#run').disabled = true;
  try {
    const res = await api('/api/reviews', { method: 'POST', body: JSON.stringify({ refs, agent: state.agent }) });
    (res.jobs || []).forEach(upsertJob);
    if (res.errors && res.errors.length) banner(res.errors.join('\n'));
    if (res.jobs && res.jobs.length) {
      state.tab = 'queue';
      state.activeJob = res.jobs[0].id;
      state.activeSaved = null;
      state.activePR = null;
    }
    state.selected.clear();
    renderPRs();
    renderTabs();
    renderJobs();
    renderViewer();
  } catch (err) {
    banner(err.message);
  } finally {
    updateReviewButton();
  }
}

function openPR(key) {
  const pr = state.prs.find((p) => p.key === key);
  if (!pr) return;
  state.activePR = key;
  state.activeJob = null;
  state.activeSaved = null;
  state.tab = 'queue';
  renderTabs();
  renderPRs();
  renderJobs();

  const v = docPanel();

  // The head reads like a job's: repo #n · author, then the title. Draft and
  // approvals ride along as small tags.
  const head = el('div', 'viewer-head');
  const grow = el('div', 'grow');
  const kicker = el('div', 'kicker');
  const link = prLink(pr);
  link.textContent = `${pr.repo} #${pr.number}`;
  kicker.append(link, document.createTextNode(' · ' + pr.author));
  if (pr.draft) kicker.append(el('span', 'tag draft', 'draft'));
  if (pr.review_decision === 'APPROVED') kicker.append(el('span', 'tag approved', 'approved'));
  if (pr.review_decision === 'CHANGES_REQUESTED') kicker.append(el('span', 'tag changes', 'changes requested'));
  grow.append(kicker, el('h2', null, pr.title));
  head.append(grow);
  v.append(head);

  // The same page the Overview of a job shows: description, timeline, files,
  // reviewers, labels and checks. Reviews of this PR in the session are in its
  // timeline, and open from there.
  renderPRPage(v, pr, null, () => { if (state.activePR === key) openPR(key); });
  v.scrollTop = 0;
}

async function openJob(id) {
  state.activePR = null;
  state.activeJob = id;
  state.activeSaved = null;
  renderPRs();
  renderJobs();
  renderViewer();
  const job = state.jobs.find((j) => j.id === id);
  if (!job || !job.has_body || state.bodies[id]) return;
  try {
    const full = await api('/api/jobs/' + encodeURIComponent(id));
    state.bodies[id] = full.html;
    if (full.publish_html) state.splits[id] = { pub: full.publish_html, ctx: full.context_html || '' };
    if (state.activeJob === id) renderViewer();
  } catch (err) {
    banner(err.message);
  }
}

async function cancelJob(id) {
  try { await api(`/api/jobs/${encodeURIComponent(id)}/cancel`, { method: 'POST' }); }
  catch (err) {
    banner(err.message);
    if (err.status === 404) dropJob(id);
  }
}

// publishJob tells the agent to publish the review on screen. It is the "I read
// it and approve" path: unlike postJob, which is Bazel pasting the markdown as a
// comment, here the post skill runs and the review goes out with inline comments.
//
// Publishing happens on this same card — the card goes back to running with the
// post agent's step at the end — and not on a second card in the queue.
async function publishJob(id, btn) {
  const job = state.jobs.find((j) => j.id === id);
  if (!job) return;
  const skip = skipList(pickKey('job', id));
  if (!confirm(`Publish this review on ${job.pr.key} with inline comments?\n\nThe post agent runs in this same card, publishing what you have just read.${skipNote(skip)}`)) return;
  const label = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = 'publishing…'; }
  clearAlert();
  try {
    const view = await api(`/api/jobs/${encodeURIComponent(id)}/publish`, { method: 'POST', body: JSON.stringify({ skip }) });
    upsertJob(view);
    state.activeJob = view.id;
    state.tab = 'queue';
    renderJobs();
    renderViewer();
  } catch (err) {
    const recusa = err.status === 409 || err.status === 404;
    alertError(recusa ? conflictNote(err) : err.message);
    if (recusa) refreshJob(id);
    if (btn) { btn.disabled = false; btn.textContent = label; }
  }
}

// conflictNote explains a 409 on publish: another publication in flight, or a
// review that already went to the PR. It is not a gh failure, and saying
// "failed" would scare someone who just clicked twice.
function conflictNote(err) {
  return `Nothing new was sent to the PR: ${err.message}.`;
}

// refreshJob fetches the job as the server sees it now. After a 409 the card on
// screen is stale: someone published, or the publication is still running.
async function refreshJob(id) {
  try {
    upsertJob(await api('/api/jobs/' + encodeURIComponent(id)));
  } catch (err) {
    // A 404 means the server no longer has the job (removed in another tab,
    // or a restart): the card goes, the same way job_gone takes it. Any other
    // failure leaves it; the banner already said what matters.
    if (err.status === 404) dropJob(id);
  }
}

// dismissJob is "keep it here": the review stays in Bazel, the card stops
// waiting, and nothing goes to the PR. The saved review stays in the Saved tab.
async function dismissJob(id, btn) {
  if (btn) btn.disabled = true;
  clearAlert();
  try {
    upsertJob(await api(`/api/jobs/${encodeURIComponent(id)}/dismiss`, { method: 'POST' }));
  } catch (err) {
    const recusa = err.status === 409 || err.status === 404;
    alertError(recusa ? `Could not keep it here: ${err.message}.` : err.message);
    if (recusa) refreshJob(id);
    if (btn) btn.disabled = false;
  }
}

async function postJob(id, btn) {
  const job = state.jobs.find((j) => j.id === id);
  if (!job) return;
  const skip = skipList(pickKey('job', id));
  const aviso = job.posts
    ? `The agent "${job.agent}" already published this review on ${job.pr.key}. Publish it again, as a comment?`
    : `Publish this review as a comment on ${job.pr.key}?`;
  if (!confirm(aviso + skipNote(skip))) return;
  const label = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = 'publishing…'; }
  clearAlert();
  try {
    upsertJob(await api(`/api/jobs/${encodeURIComponent(id)}/post`, { method: 'POST', body: JSON.stringify({ skip }) }));
  } catch (err) {
    // 409 and 404 are Bazel turning the request down (busy, already posted,
    // not finished, not publishable, gone): the card on screen is stale. Only
    // a 502 is GitHub failing to take the comment.
    const recusa = err.status === 409 || err.status === 404;
    alertError(recusa ? conflictNote(err) : 'failed to publish: ' + err.message);
    if (recusa) refreshJob(id);
    if (btn) { btn.disabled = false; btn.textContent = label; }
  }
}

async function loadSaved() {
  const rail = $('#rail');
  rail.innerHTML = '';
  try {
    const data = await api('/api/reviews');
    state.saved = data.reviews || [];
  } catch (err) {
    banner(err.message);
    state.saved = [];
  }
  renderSaved();
}

async function openSaved(name) {
  state.activeSaved = name;
  state.activeJob = null;
  state.activePR = null;
  renderSaved();
  try {
    const data = await api('/api/reviews/' + encodeURIComponent(name));
    const v = docPanel();
    $('#viewer').dataset.saved = name;
    const entry = state.saved.find((x) => x.name === name);
    const head = el('div', 'viewer-head');
    const grow = el('div', 'grow');
    grow.append(el('div', 'kicker', [entry && entry.agent, data.name].filter(Boolean).join(' · ')),
      el('h2', null, (entry && entry.title) || data.name));
    const back = el('button', 'btn small ghost', '← all saved');
    back.addEventListener('click', () => { state.activeSaved = null; renderSaved(); });
    head.append(grow, back);
    v.append(head);
    const md = tid(el('div', 'md'), 'review-body');
    md.innerHTML = data.html;
    v.append(md);
    const key = pickKey('saved', name);
    const total = decorateFindings(md, key, !entry || entry.publishable !== false);

    // A saved review stays publishable: closing Bazel before sending it to the
    // PR can't cost the agent's work.
    if (entry && entry.repo && entry.publishable === false) {
      const nota = el('p', 'dim');
      nota.style.marginTop = '28px';
      nota.textContent = `This is not a review — what "${entry.agent}" produces stays in Bazel.`;
      v.append(nota);
    } else if (entry && entry.repo) {
      const bar = el('div', 'viewer-actions');
      const pub = tid(el('button', 'btn primary', '↗ Publish'), 'saved-publish');
      pub.title = `runs the post skill and publishes to ${entry.repo}#${entry.number} with inline comments`;
      pub.addEventListener('click', () => publishSaved(name, pub));
      const cm = tid(el('button', 'btn ghost', 'As comment'), 'saved-comment');
      cm.title = `pastes this review as a single comment on ${entry.repo}#${entry.number}`;
      cm.addEventListener('click', () => commentSaved(name, cm));
      bar.append(total ? pickSummary(key, total) : el('span', 'dim', 'no findings to pick'), el('div', 'spacer'), cm, pub);
      v.append(bar);
    }
    v.scrollTop = 0;
  } catch (err) {
    banner(err.message);
  }
}

// publishSaved tells the publishing agent to take to the PR a review that is on
// disk — one from a previous session, opened here in the saved list.
async function publishSaved(name, btn) {
  const entry = state.saved.find((x) => x.name === name);
  const alvo = entry ? `${entry.repo}#${entry.number}` : 'the PR';
  const skip = skipList(pickKey('saved', name));
  if (!confirm(`Publish this saved review on ${alvo} with inline comments?\n\nThe agent runs to publish what is in the file — it does not review again.${skipNote(skip)}`)) return;
  btn.disabled = true;
  clearAlert();
  try {
    const job = await api('/api/reviews/' + encodeURIComponent(name) + '/publish', { method: 'POST', body: JSON.stringify({ skip }) });
    upsertJob(job);
    state.tab = 'queue';
    state.activeJob = job.id;
    state.activeSaved = null;
    renderTabs();
    renderJobs();
    renderViewer();
  } catch (err) {
    alertError(err.message);
    btn.disabled = false;
  }
}

// commentSaved pastes the saved review on the PR, with no agent.
async function commentSaved(name, btn) {
  const entry = state.saved.find((x) => x.name === name);
  const alvo = entry ? `${entry.repo}#${entry.number}` : 'the PR';
  const skip = skipList(pickKey('saved', name));
  if (!confirm(`Paste this saved review as a comment on ${alvo}?` + skipNote(skip))) return;
  btn.disabled = true;
  const antes = btn.textContent;
  btn.textContent = 'publishing…';
  clearAlert();
  try {
    await api('/api/reviews/' + encodeURIComponent(name) + '/comment', { method: 'POST', body: JSON.stringify({ skip }) });
    btn.textContent = '✓ commented on ' + alvo;
    loadPRs(false);
  } catch (err) {
    alertError('failed to publish: ' + err.message);
    btn.disabled = false;
    btn.textContent = antes;
  }
}

// --- the checkbox of each finding ---
//
// Each review finding arrives from the server in a <section class="finding"
// data-finding="N">. The checkbox is the decision to take it to the PR: unticked,
// the finding drops out of what any of the publish buttons send — that is how a
// false positive gets left behind without anyone editing the markdown. The N is
// the same index the server uses to cut, and the choice lives in state.picks
// to survive the panel being redrawn.
const pickKey = (kind, id) => kind + ':' + id;

function skipped(key) {
  if (!state.picks[key]) state.picks[key] = new Set();
  return state.picks[key];
}

const skipList = (key) => [...skipped(key)].sort((a, b) => a - b);

// skipNote is the reminder in the publish confirm: what was left unticked doesn't go.
function skipNote(skip) {
  // What goes, said in the confirm: the ticked findings with their full
  // bodies (folded or not), and the summary and other sections of the report.
  const md = document.querySelector('#viewer .md.compact');
  let goes = '';
  if (md) {
    const all = md.querySelectorAll('section.finding').length;
    const extra = md.querySelectorAll('.rmore h2, .rmore h3').length;
    goes = `\n\nGoes to the PR: ${all - skip.length} of ${all} finding${all === 1 ? '' : 's'} (whole bodies)`
      + (md.querySelector('.rsummary') ? ', the summary' : '')
      + (extra ? ` and ${extra} more section${extra === 1 ? '' : 's'} of the report` : '') + '.';
  }
  if (!skip.length) return goes;
  return goes + `\n${skip.length} unticked finding${skip.length === 1 ? '' : 's'} will be left out.`;
}

// decorateFindings puts the checkbox on each finding of the review drawn in md
// and returns how many findings there are.
// findingHead pulls a finding's title node and severity out of its section:
// the ### heading, the bold line that opens a numbered finding, or the first
// line of a doubt's list item. The severity is the word the agent puts in the
// heading (high, medium, low…); it becomes the row's dot.
const SEVERITIES = [
  ['crit', /\b(critical|blocker)\b/i],
  ['high', /\b(high|major|severe)\b/i],
  ['med', /\b(medium|moderate)\b/i],
  ['low', /\b(low|minor|nit|info)\b/i],
];
function findingHead(sec) {
  const node = sec.querySelector(':scope > h1, :scope > h2, :scope > h3, :scope > h4')
    || sec.querySelector(':scope > p:first-of-type > strong:first-child')
    || sec.querySelector(':scope > ul > li, :scope > ol > li');
  let text = node ? node.textContent.replace(/\s+/g, ' ').trim() : '';
  if (node && node.tagName === 'LI') text = text.split(/(?<=[.?!])\s/)[0];
  const sev = (SEVERITIES.find(([, re]) => re.test(text)) || ['none'])[0];
  // "1. Title · medium" → "Title": the number and the severity have their own place.
  const title = text.replace(/^\d+[.)]\s*/, '').replace(/\s*[·—–-]\s*(critical|blocker|high|major|severe|medium|moderate|low|minor|nit|info)\s*$/i, '').trim();
  return { node, title: title || 'finding', sev };
}

// findingTitle is a short " (title)" for a finding's accessible name.
function findingTitle(t) {
  if (t.length > 60) t = t.slice(0, 59).trimEnd() + '…';
  return t ? ` (${t})` : '';
}

// decorateFindings turns each finding of a rendered report into one compact
// row, the way the design lists them: the "report" checkbox (unticked, it is
// left out of what goes to the PR), a severity dot and the title; a click on
// the title opens the body (the first one starts open). The section keeps its
// data-finding index, the one publish receives in skip.
function decorateFindings(md, key, publishable = true) {
  const secs = [...md.querySelectorAll('section.finding')];
  const out = skipped(key);
  secs.forEach((sec, n) => {
    const idx = Number(sec.dataset.finding);
    const { node, title, sev } = findingHead(sec);
    const label = el('label', 'finding-pick');
    const box = document.createElement('input');
    box.type = 'checkbox';
    box.checked = !out.has(idx);
    tid(box, 'finding-pick', { findingIndex: idx });
    // Every box looks the same; the accessible name says which finding it is.
    box.setAttribute('aria-label', `include finding ${idx + 1}${findingTitle(title)} in what goes to the PR`);
    box.title = 'untick to leave this finding out of what goes to the PR';
    label.append(box);

    const body = el('div', 'fbody');
    for (const child of [...sec.childNodes]) body.append(child);
    // The title moves to the row; a list item keeps its text in the body,
    // since the title is only its first sentence.
    if (node && node.tagName !== 'LI') {
      const holder = node.tagName === 'STRONG' ? node.parentElement : node;
      if (node.tagName === 'STRONG') node.remove();
      else holder.remove();
      if (holder !== node && !holder.textContent.trim()) holder.remove();
    }
    const head = el('div', 'fhead');
    const toggle = tid(el('button', 'ftitle', title), 'finding-toggle');
    toggle.setAttribute('aria-expanded', String(n === 0));
    toggle.addEventListener('click', () => {
      const open = !sec.classList.contains('open');
      sec.classList.toggle('open', open);
      sec.dataset.open = String(open);
      toggle.setAttribute('aria-expanded', String(open));
    });
    head.append(label, el('span', 'sev ' + sev), toggle);
    if (sev !== 'none') head.lastChild.title = sev;
    sec.append(head, body);
    sec.classList.add('frow');
    sec.classList.toggle('open', n === 0);
    tid(sec, 'finding-row', { findingIndex: idx, severity: sev, open: n === 0 });
    sec.classList.toggle('skipped', out.has(idx));
    box.addEventListener('change', () => {
      if (box.checked) out.delete(idx); else out.add(idx);
      sec.classList.toggle('skipped', !box.checked);
      refreshPickSummary();
    });
  });
  compactReport(md, publishable);
  return secs.length;
}

// compactReport lays the report out like the design: the prose summary on
// top, then each group of findings (Findings, Cuts, Needs human verification)
// as a short list under a small label. Whatever else the report has (coverage,
// verdict…) stays below, folded, so nothing it said is lost.
function compactReport(md, publishable) {
  const kids = [...md.children];
  const firstSec = kids.findIndex((k) => k.matches('section.finding'));
  if (firstSec < 0) return;
  const groupOf = new Map();
  let h2 = null;
  for (const k of kids) {
    if (/^H[12]$/.test(k.tagName)) h2 = k;
    else if (k.matches('section.finding') && h2) groupOf.set(h2, true);
  }
  let start = kids.findIndex((k) => groupOf.has(k));
  if (start < 0 || start > firstSec) start = firstSec;
  const summary = el('div', 'rsummary');
  const rest = el('div', 'rrest');
  kids.slice(0, start).forEach((k) => summary.append(k));
  let inGroup = false;
  const groups = el('div', 'rgroups');
  for (const k of kids.slice(start)) {
    if (/^H[12]$/.test(k.tagName)) {
      inGroup = groupOf.has(k);
      if (inGroup) { k.className = 'rgroup-label'; groups.append(k); } else rest.append(k);
    } else if (k.matches('section.finding') || inGroup) {
      groups.append(k);
    } else {
      rest.append(k);
    }
  }
  md.innerHTML = '';
  md.classList.add('compact');
  if (summary.childNodes.length) md.append(summary);
  md.append(groups);
  if (rest.childNodes.length) {
    // Whatever the report says besides the findings goes to the PR with them:
    // when it does, the fold is open and says so (review F5).
    const more = tid(el('details', 'rmore'), 'review-more', { publishes: !!publishable });
    more.open = !!publishable;
    const sum = el('summary', null, 'More from the report');
    if (publishable) sum.append(el('span', 'rmore-goes', 'also goes to the PR'));
    more.append(sum, rest);
    md.append(more);
  }
}

// pickSummary is the line next to the publish buttons: how many findings go,
// how many stay, and the shortcuts to tick or untick all of them at once.
function pickSummary(key, total) {
  const s = tid(el('span', 'pick-summary'), 'pick-summary');
  s.dataset.key = key;
  s.dataset.total = total;
  const txt = el('span', 'dim');
  const all = tid(el('button', 'btn small ghost', 'all'), 'finding-pick-all');
  all.title = 'tick every finding';
  const none = tid(el('button', 'btn small ghost', 'none'), 'finding-pick-none');
  none.title = 'untick every finding';
  const setAll = (on) => {
    document.querySelectorAll('#viewer section.finding input[type=checkbox]').forEach((box) => {
      if (box.checked !== on) { box.checked = on; box.dispatchEvent(new Event('change')); }
    });
  };
  all.addEventListener('click', () => setAll(true));
  none.addEventListener('click', () => setAll(false));
  s.append(txt, all, none);
  refreshPickSummary(s);
  return s;
}

function refreshPickSummary(s = document.querySelector('#viewer .pick-summary')) {
  if (!s) return;
  const total = Number(s.dataset.total);
  const out = skipped(s.dataset.key).size;
  const kept = total - out;
  const txt = s.firstChild;
  // "N of M findings", the design's footer: how many go out of how many.
  txt.textContent = `${kept} of ${total} finding${total === 1 ? '' : 's'}`;
  txt.title = !kept ? 'every finding unticked — only the rest of the review goes out'
    : out ? `${out} left out of what goes to the PR` : 'every finding goes to the PR';
  s.children[1].disabled = !out;
  s.children[2].disabled = !kept;
}

// --- render ---

function visiblePRs() {
  const q = state.filter.toLowerCase();
  return state.prs.filter((pr) => {
    if (state.scope === 'mine' && !pr.mine) return false;
    if (state.ownerFilter && repoOwner(pr.repo).toLowerCase() !== state.ownerFilter) return false;
    if (state.repoFilter && repoKey(pr.repo) !== state.repoFilter) return false;
    if (!matchesReview(pr)) return false;
    if (state.hideDrafts && pr.draft) return false;
    if (!q) return true;
    return `${pr.repo} #${pr.number} ${pr.title} ${pr.author}`.toLowerCase().includes(q);
  });
}

// markedPRs is what the "run" button will run: what is ticked **and**
// visible. Filtering doesn't untick anything — the tick waits for the filter to
// go away — but reviewing a PR that left the screen would be a surprise.
function markedPRs() {
  return visiblePRs().filter((pr) => state.selected.has(pr.key)).map((pr) => pr.key);
}

// matchesReview separates what Bazel already reviewed from what it hasn't seen
// yet. "changed" is the case that matters: reviewed, and with a new commit
// afterwards — the saved review is no longer about this code.
function matchesReview(pr) {
  switch (state.reviewFilter) {
    case 'none': return !pr.reviewed;
    case 'reviewed': return !!pr.reviewed && !pr.changed_since_review;
    case 'changed': return !!pr.changed_since_review;
    default: return true;
  }
}

// repoOwner and repoName split "owner/repo" into the two halves behind the two
// selectors. A repository with no slash has no owner to offer: it keeps only
// its name and stays out of the owner selector.
function repoOwner(repo) {
  const i = repo.indexOf('/');
  return i > 0 ? repo.slice(0, i) : '';
}

function repoName(repo) {
  const i = repo.indexOf('/');
  return i > 0 ? repo.slice(i + 1) : repo;
}

// repoKey is how two spellings of one repository meet: the config keeps what
// was typed ("beroni/bazel"), and GitHub's search answers with the canonical
// one ("Beroni/bazel"). Owners and repositories are compared by it, and the
// filters keep it as their value.
const repoKey = (repo) => repo.toLowerCase();

// fillSelect rebuilds a filter selector: the "any" option, then one option per
// [value, label] pair, keeping the current choice and the "on" highlight.
function fillSelect(sel, anyLabel, options, current) {
  sel.innerHTML = '';
  const any = el('option', null, anyLabel);
  any.value = '';
  sel.append(any);
  for (const [value, label] of options) {
    const o = el('option', null, label);
    o.value = value;
    sel.append(o);
  }
  sel.value = current;
  sel.classList.toggle('on', !!current);
}

// renderRepoFilters builds the two selectors — owner and repository — from
// what is watched and how many PRs each one has right now. The repository one
// only lists the chosen owner's repositories, and each selector hides when a
// single option is left: there is nothing to choose.
function renderRepoFilters() {
  // One entry per repository, whatever its case. The watched ones set the
  // order; a PR from a repository that left the list still shows up, or it
  // would become a filter impossible to undo. The label is the spelling a PR
  // came with (GitHub's), and the config's only while no PR has.
  const repos = new Map(); // key -> {label, count, seen}
  for (const repo of state.repos) {
    if (!repos.has(repoKey(repo))) repos.set(repoKey(repo), { label: repo, count: 0, seen: false });
  }
  for (const pr of state.prs) {
    const r = repos.get(repoKey(pr.repo)) || { count: 0 };
    Object.assign(r, { label: pr.repo, count: r.count + 1, seen: true });
    repos.set(repoKey(pr.repo), r);
  }

  // Owners the same way: one per owner, labelled as GitHub spells it when any
  // of its repositories brought a PR.
  const owners = new Map(); // key -> {label, count, seen}
  for (const r of repos.values()) {
    const owner = repoOwner(r.label);
    if (!owner) continue;
    const o = owners.get(owner.toLowerCase()) || { label: owner, count: 0, seen: false };
    if (r.seen && !o.seen) Object.assign(o, { label: owner, seen: true });
    o.count += r.count;
    owners.set(owner.toLowerCase(), o);
  }

  const donoSel = $('#owner-filter');
  donoSel.hidden = owners.size < 2;
  // The chosen owner stays chosen even with no open PR left; it only drops
  // when its repositories are no longer watched.
  if (donoSel.hidden || (state.ownerFilter && !owners.has(state.ownerFilter))) {
    state.ownerFilter = '';
  }
  if (!donoSel.hidden) {
    fillSelect(donoSel, `any owner (${state.prs.length})`,
      [...owners].map(([k, o]) => [k, `${o.label} (${o.count})`]), state.ownerFilter);
  }

  // With a single owner in play — chosen, or because it is the only one — the
  // repository name is enough: repeating the owner in every option only eats
  // width.
  const doDono = [...repos].filter(([, r]) =>
    !state.ownerFilter || repoOwner(r.label).toLowerCase() === state.ownerFilter);
  const soNome = !!state.ownerFilter || owners.size < 2;
  const sel = $('#repo-filter');
  sel.hidden = doDono.length < 2;
  if (sel.hidden) {
    // An owner with a single repository hides the selector, but a repository
    // filter of that owner is kept: the hidden selector means the same thing,
    // and dropping it would bring back, under "any owner", the ticked PRs it
    // was holding out of view. With no owner selector either there is no
    // control left to turn the filter off, so then it goes.
    if (state.repoFilter && (donoSel.hidden || !doDono.some(([k]) => k === state.repoFilter))) {
      state.repoFilter = '';
    }
    return;
  }
  if (state.repoFilter && !doDono.some(([k]) => k === state.repoFilter)) state.repoFilter = '';

  const total = doDono.reduce((n, [, r]) => n + r.count, 0);
  fillSelect(sel, `any repo (${total})`,
    doDono.map(([k, r]) => [k, `${soNome ? repoName(r.label) : r.label} (${r.count})`]), state.repoFilter);
}

// renderAgents builds the picker with the agents and pipelines from config.yaml.
// selectableAgents are the ones that can review. The publishing agent also
// comes from the server — it shows up in the config — but running it on a PR
// with no finished review wouldn't do anything.
// defaultAgent is config.yaml's default: the server lists the choices with
// it first. It is not state.agent, which is only what the dashboard picker
// has selected right now.
function defaultAgent() {
  const first = selectableAgents()[0];
  return first ? first.name : '';
}

function selectableAgents() {
  return state.agents.filter((a) => !a.publisher);
}

// applyAgents stores the list that came from the server and keeps the picker's
// choice valid: whoever was removed can't stay selected, and the first in the
// list is the default.
//
// adotaPadrao swaps the current choice for the new first one. It is what makes
// the "make default" button take effect on the very next review: without it the
// file would change and the picker would stay on the previous agent.
function applyAgents(list, adotaPadrao) {
  state.agents = list;
  const opcoes = selectableAgents();
  if (adotaPadrao || !opcoes.some((a) => a.name === state.agent)) {
    state.agent = opcoes.length ? opcoes[0].name : '';
  }
  renderAgents();
  updateReviewButton();
}

function renderAgents() {
  const sel = $('#agent');
  sel.innerHTML = '';
  const opcoes = selectableAgents();
  if (!opcoes.length) { sel.hidden = true; return; }
  sel.hidden = false;
  for (const a of opcoes) {
    const opt = el('option', null, a.name + (a.pipeline ? ' ⛓' : '')
      + (a.posts ? ' ⇧ publishes' : '') + (a.publishable === false ? ' · stays here' : ''));
    opt.value = a.name;
    opt.title = [a.description, a.pipeline ? (a.steps || []).join(' → ') : '']
      .filter(Boolean).join('\n');
    sel.append(opt);
  }
  sel.value = state.agent;
  sel.title = agentTitle(state.agent);
}

function agentTitle(name) {
  const a = state.agents.find((x) => x.name === name);
  if (!a) return 'which agent runs over the ticked PRs';
  return [
    a.description,
    a.pipeline ? 'pipeline: ' + (a.steps || []).join(' → ') : '',
    a.posts ? '⇧ this agent publishes the review to the PR on its own' : '',
    a.publishable === false ? 'what it produces is not a review — it stays in Bazel' : '',
  ].filter(Boolean).join('\n') || a.name;
}

// stepsBox is a job's list of steps: who finished, whose turn it is and who
// hasn't even started.
function stepsBox(job) {
  const box = el('div', 'steps');
  if (job.cloning) box.append(stepRow({ name: 'cloning the repository…', state: 'running' }));
  for (const st of job.steps || []) {
    box.append(stepRow(st));
  }
  return box;
}

function stepRow(st) {
  const row = tid(el('div', 'step ' + st.state), 'job-step', { step: st.name, state: st.state });
  row.append(el('span', 'dot'));
  row.append(el('span', 'step-name', st.name));
  const secs = stepSeconds(st);
  if (secs) row.append(el('span', 'step-time', secs + 's'));
  if (st.error) row.title = st.error;
  return row;
}

// stepSeconds counts the running step's time from started_at, so the clock
// moves between one server event and the next.
function stepSeconds(st) {
  if (st.state === 'running' && st.started_at) {
    return Math.max(0, Math.round((Date.now() - new Date(st.started_at)) / 1000));
  }
  return st.seconds || 0;
}

// --- agent log ---
//
// The log doesn't come over SSE: a chatty agent writes thousands of lines and
// each one would wake every open browser. The page keeps the `next` and fetches
// only what is missing, once a second while the review runs.

const maxLogLines = 500;

function logState(id) {
  if (!state.logs[id]) {
    state.logs[id] = { next: 0, lines: [], dropped: 0, live: true, busy: false, agents: [] };
  }
  return state.logs[id];
}

// noteAgents keeps the list of who has already spoken, in the order they showed
// up — it is what gives each terminal its color.
function noteAgents(st, lines) {
  for (const l of lines) {
    const who = l.agent || 'agent';
    if (!st.agents.includes(who)) st.agents.push(who);
  }
}

async function pollLog(id) {
  const st = logState(id);
  if (st.busy) return;
  st.busy = true;
  try {
    const data = await api(`/api/jobs/${encodeURIComponent(id)}/log?from=${st.next}`);
    st.next = data.next;
    st.live = data.live;
    st.dropped = data.dropped || 0;
    const lines = data.lines || [];
    if (lines.length) {
      st.lines.push(...lines);
      if (st.lines.length > maxLogLines) st.lines = st.lines.slice(-maxLogLines);
      noteAgents(st, lines);
      appendLog(id, lines);
    }
  } catch {
    // The log is secondary: failing to fetch it can't break the screen.
  } finally {
    st.busy = false;
  }
}

// --- the live log: one column of agents, one stream of lines ---
//
// The fleet starts its lenses inside the same process, in parallel. The log
// keeps one stream, every line signed and colored by whoever wrote it, and a
// column on the left with "All" and each agent: picking one shows only its
// lines, so a line of reasoning can be followed without the others.

const maxTermLines = 500;

// agentName is the name a line is signed with. An unsigned line belongs to the
// step's agent — the server already sends that name filled in.
function agentName(l) {
  return l.agent || 'agent';
}

// agentColor gives each agent its color from the order it first spoke in.
function agentColor(id, who) {
  return 'c' + (Math.max(0, logState(id).agents.indexOf(who)) % 6);
}

function logLineEl(id, l) {
  const who = agentName(l);
  const line = tid(el('div', 'log-line ' + (l.stream === 'stderr' ? 'stderr' : 'stdout')), 'log-line',
    { stream: l.stream === 'stderr' ? 'stderr' : 'stdout' });
  line.dataset.agent = who;
  const name = el('span', 'who ' + agentColor(id, who), who);
  name.title = who;
  line.append(name, el('span', 'txt', l.text));
  return line;
}

// logFilter is the agent picked in the column, or '' for all of them.
function logFilter(id) {
  return state.logFilter[id] || '';
}

// pushLine writes a line to the stream and keeps the reader at the end if they
// were already there.
function pushLine(wrap, id, l) {
  const atEnd = wrap.scrollHeight - wrap.scrollTop - wrap.clientHeight < 30;
  const line = logLineEl(id, l);
  const f = logFilter(id);
  if (f && f !== agentName(l)) line.hidden = true;
  wrap.append(line);
  while (wrap.childElementCount > maxTermLines) wrap.firstElementChild.remove();
  if (atEnd) wrap.scrollTop = wrap.scrollHeight;
}

// logTerminals builds the stream with what the page already has on hand.
function logTerminals(id) {
  const wrap = el('div', 'log-stream');
  wrap.id = 'log-terms';
  wrap.dataset.job = id;
  const st = logState(id);
  for (const l of st.lines) pushLine(wrap, id, l);
  if (!st.lines.length) wrap.append(el('p', 'log-empty', 'waiting for the agent to speak…'));
  return wrap;
}

// logAgents is the left column: All, then each agent with its line count.
function logAgents(id) {
  const col = el('div', 'log-agents');
  col.id = 'log-agents';
  col.dataset.job = id;
  fillLogAgents(col, id);
  return col;
}

function fillLogAgents(col, id) {
  const st = logState(id);
  const count = new Map();
  for (const l of st.lines) count.set(agentName(l), (count.get(agentName(l)) || 0) + 1);
  // Rebuilt on every poll with new lines: the agent button that had the
  // keyboard focus gets it back (review F7).
  const had = col.contains(document.activeElement) ? document.activeElement.dataset.agent : null;
  col.innerHTML = '';
  const pick = (who) => {
    state.logFilter[id] = who;
    const wrap = $('#log-terms');
    if (wrap) {
      for (const line of wrap.querySelectorAll('.log-line')) line.hidden = !!who && line.dataset.agent !== who;
      wrap.classList.toggle('one', !!who);
      wrap.scrollTop = wrap.scrollHeight;
    }
    fillLogAgents(col, id);
  };
  const item = (who, name, n, cor) => {
    const b = tid(el('button', 'log-agent' + (logFilter(id) === who ? ' on' : '')), 'log-pane', { agent: who || 'all' });
    b.append(el('span', 'dot ' + cor), el('span', 'log-agent-name', name), el('span', 'log-agent-n', String(n)));
    b.title = name;
    b.addEventListener('click', () => pick(who));
    col.append(b);
  };
  item('', 'All', st.lines.length, 'all');
  for (const who of st.agents) item(who, who, count.get(who) || 0, agentColor(id, who));
  const foot = el('p', 'log-count dim', logCountLabel(id));
  foot.id = 'log-count';
  col.append(foot);
  if (had) {
    const b = [...col.querySelectorAll('.log-agent')].find((x) => x.dataset.agent === had);
    if (b) b.focus({ preventScroll: true });
  }
}

// appendLog sends the new lines to the stream and refreshes the column.
function appendLog(id, lines) {
  const wrap = $('#log-terms');
  if (!wrap || wrap.dataset.job !== id) return;
  const vazio = wrap.querySelector('.log-empty');
  if (vazio) vazio.remove();
  for (const l of lines) pushLine(wrap, id, l);
  const col = $('#log-agents');
  if (col && col.dataset.job === id) fillLogAgents(col, id);
}

// logCountLabel is the line count under the agent column, with the older
// lines the 500-line window dropped.
function logCountLabel(id) {
  const st = logState(id);
  const n = st.next;
  return `${n} line${n === 1 ? '' : 's'}` + (st.dropped ? ` · ${st.dropped} older dropped` : '');
}

function logPanel(job) {
  const wrap = tid(el('div', 'logview'), 'live-log');
  const stream = logTerminals(job.id);
  if (logFilter(job.id)) stream.classList.add('one');
  wrap.append(logAgents(job.id), stream);
  requestAnimationFrame(() => { stream.scrollTop = stream.scrollHeight; });
  return wrap;
}

// syncFilterMenu lights the filter icon when one of the filters it hides is on,
// so a narrowed list never looks like a short one.
function syncFilterMenu() {
  const on = state.scope !== 'all' || state.hideDrafts || !!state.ownerFilter || !!state.repoFilter;
  const b = $('#filter-btn');
  b.classList.toggle('on', on);
  b.title = on ? 'filters on — click to see or change them' : 'more filters: whose PRs, drafts, owner and repository';
}

function renderStats() {
  syncSelectAll();
  syncFilterMenu();
  const total = state.prs.length;
  const shown = visiblePRs().length;
  const marked = markedPRs().length;
  // With a filter on, the number that matters is the slice's — but the total
  // stays in view, otherwise the hint that a filter is hiding PRs disappears.
  const conta = shown === total
    ? `${total} PR${total === 1 ? '' : 's'}`
    : `${shown} of ${total} PRs`;
  $('#stats').textContent = conta + (marked ? ` · ${marked} ticked` : '');
  $('#sel-count').textContent = `${marked} selected`;
}

// syncSelectAll makes the "select all" box say what is true: checked only when
// there are visible PRs and every one of them is ticked, half-checked when some
// are. It runs on every render of the list once the list has arrived, so a box
// ticked while it was still loading (it selected nothing) clears itself when
// the PRs come in, instead of staying checked over an empty selection
// (BUG-QA-06). Before the first answer there is no list to derive it from.
function syncSelectAll() {
  if (!state.prsLoaded) return;
  const box = $('#select-all');
  const shown = visiblePRs();
  const ticked = shown.filter((pr) => state.selected.has(pr.key)).length;
  box.checked = shown.length > 0 && ticked === shown.length;
  box.indeterminate = ticked > 0 && ticked < shown.length;
}

function renderPRs() {
  // Until the first answer the list says it is loading (or why it failed); a
  // job event, the filter or "select all" drawing it then would say "no open
  // PRs" instead. loadPRs draws it when the list lands.
  if (!state.prsLoaded) return;
  const list = $('#pr-list');
  const prs = visiblePRs();
  // A rebuild must not drop the keyboard focus: note which row's title or
  // checkbox had it and give it back afterwards.
  const had = list.contains(document.activeElement) ? document.activeElement : null;
  const hadRow = had && had.closest('.pr') ? had.closest('.pr').dataset.prKey : null;
  const hadBox = !!(had && had.type === 'checkbox');
  list.innerHTML = '';
  queueMicrotask(() => {
    if (!hadRow) return;
    const row = [...list.querySelectorAll('.pr')].find((r) => r.dataset.prKey === hadRow);
    const target = row && row.querySelector(hadBox ? 'input[type=checkbox]' : '.title');
    if (target) target.focus({ preventScroll: true });
  });
  if (!prs.length) {
    list.append(el('p', 'empty', state.prs.length ? 'nothing matches the filter' : 'no open PRs'));
    renderStats();
    updateReviewButton();
    return;
  }
  for (const pr of prs) {
    const row = el('div', 'pr'
      + (state.selected.has(pr.key) ? ' sel' : '')
      + (state.activePR === pr.key ? ' active' : ''));
    const box = el('input');
    box.type = 'checkbox';
    box.checked = state.selected.has(pr.key);
    box.setAttribute('aria-label', `tick ${pr.key}`);
    tid(box, 'pr-row-check');
    tid(row, 'pr-row', { prKey: pr.key, selected: state.selected.has(pr.key) });
    box.addEventListener('click', (e) => e.stopPropagation());
    box.addEventListener('change', () => toggle(pr.key, box.checked));
    // The row opens, the checkbox ticks: whoever clicks the PR wants to read it,
    // and ticking for review has a target of its own.
    row.addEventListener('click', () => openPR(pr.key));

    // Two lines, as the design draws them: the title, then the repository and
    // number with one state tag on the right — reviewing, changed or
    // reviewed. Everything else about the PR (author, size, age, draft,
    // approvals) is in the row's tooltip and in its Overview.
    const main = el('div', 'pr-main');
    const sub = el('div', 'pr-sub');
    sub.append(el('span', 'repo', `${pr.slug} #${pr.number}`));
    const tag = prStateTag(pr);
    if (tag) sub.append(tag);
    // The title is the keyboard way in: the row's click opens the PR, and the
    // checkbox next to it stays its own control.
    const title = operable(el('span', 'title', pr.title), `open ${pr.key}: ${pr.title}`, () => openPR(pr.key));
    row.title = [
      pr.title,
      `${pr.key} · @${pr.author} · +${pr.additions} −${pr.deletions} · ${pr.changed_files} files · ${pr.age}`,
      [pr.draft && 'draft', pr.mine && 'yours',
        pr.review_decision === 'APPROVED' && 'approved', pr.review_decision === 'CHANGES_REQUESTED' && 'changes requested']
        .filter(Boolean).join(' · '),
    ].filter(Boolean).join('\n');
    main.append(title, sub);
    row.append(box, main);
    list.append(row);
  }
  renderStats();
  updateReviewButton();
}

// ago turns "2h" into "2h ago"; "just now" is already a phrase and would get
// a crooked "ago".
const ago = (age) => (age === 'just now' ? age : age + ' ago');

// reviewTag is the PR's history in the list: whether it was reviewed, when, and
// — what matters most — whether it got a new commit after that. A reviewed PR
// that changed is worth a warning, not a ✓: what is on GitHub is no longer what
// the agent read.
function reviewTag(pr) {
  if (!pr.reviewed) return null;
  if (pr.changed_since_review) {
    const t = tid(el('span', 'tag stale', 'changed'), 'pr-review-tag', { state: 'changed' });
    // A stale review that already went to the PR is the case worth flagging.
    t.title = `reviewed ${ago(pr.reviewed_at)} by ${pr.review_agent || 'an agent'}`
      + (pr.review_posted ? ', published on the PR' : '')
      + ', and the PR got new commits afterwards';
    return t;
  }
  const t = tid(el('span', 'tag reviewed', 'reviewed ' + (pr.reviewed_at === 'just now' ? 'now' : pr.reviewed_at)),
    'pr-review-tag', { state: pr.review_posted ? 'published' : 'reviewed' });
  t.title = `reviewed ${ago(pr.reviewed_at)} by ${pr.review_agent || 'an agent'}`
    + (pr.review_posted ? ', and published on the PR' : '');
  return t;
}

// selectVisible is "select all": it ticks, or unticks, only the PRs the filter
// shows. Ticks on PRs a filter is hiding stay as they are — filtering never
// unticks anything, and neither does a box that only sees the visible list.
function selectVisible(on) {
  for (const pr of visiblePRs()) {
    if (on) state.selected.add(pr.key); else state.selected.delete(pr.key);
  }
}

// syncReviewingTags redraws the PR list when the set of PRs with a live job
// changes, so a row says "reviewing" while one runs over it — and only then:
// the list is not rebuilt on every job event.
function syncReviewingTags() {
  const sig = state.jobs.filter(reviewing).map((j) => j.pr.key).sort().join(' ');
  if (sig === state.liveSig) return;
  state.liveSig = sig;
  renderPRs();
}

// reviewing tells a review still queued or running from the rest; the job
// that writes a PR's history is not a review of it.
const reviewing = (j) => !j.history && (j.state === 'queued' || j.state === 'running');

// prStateTag is the one tag a PR row carries: "reviewing" while a job of this
// session runs over it, else what reviewTag says (changed, or reviewed with
// its age).
function prStateTag(pr) {
  const live = state.jobs.some((j) => j.pr.key === pr.key && reviewing(j));
  if (live) return tid(el('span', 'tag reviewing', 'reviewing'), 'pr-review-tag', { state: 'reviewing' });
  return reviewTag(pr);
}

function toggle(key, on) {
  if (on) state.selected.add(key); else state.selected.delete(key);
  renderPRs();
}

function updateReviewButton() {
  const n = markedPRs().length;
  const btn = $('#run');
  const semAgente = !selectableAgents().length;
  btn.disabled = n === 0 || semAgente;
  btn.title = semAgente
    ? 'no agents configured — go to "config" and build the list out of your skills'
    : 'runs the chosen agent over the ticked PRs';
  btn.textContent = n > 1 ? `▶ Review ${n}` : '▶ Review';
}

// togglePRs collapses the left list down to the hamburger strip and brings it
// back. It is what gives the whole screen to the review, and the choice is kept
// in the browser — collapsing again on every opened tab would be repeated work.
function togglePRs(hide) {
  state.prsHidden = hide;
  document.querySelector('main').classList.toggle('no-prs', hide);
  const b = $('#toggle-prs');
  b.title = hide ? 'show the PR list' : 'hide the list (gives the review the full window)';
  b.setAttribute('aria-expanded', hide ? 'false' : 'true');
  try {
    localStorage.setItem('bazel.prsHidden', hide ? '1' : '');
  } catch (err) {
    // Browser without storage: the choice holds only for this session.
  }
}

function renderTabs() {
  document.querySelectorAll('.tab').forEach((t) => {
    t.classList.toggle('on', t.dataset.tab === state.tab);
    t.setAttribute('aria-selected', String(t.dataset.tab === state.tab));
  });
  if (state.tab === 'saved') loadSaved(); else renderJobs();
}

// renderQuota shows the Claude quota — the same windows as `/usage`: five
// hours and seven days. It isn't queried: it rides along on the stream of
// whoever is running, so what is on screen is the last reading, and the title
// says when it is from.
function renderQuota() {
  renderQuotaParts();
  const box = $('#quota');
  box.dataset.level = box.querySelector('.hot') ? 'crit' : box.querySelector('.warn') ? 'warn' : 'ok';
  // The bar carries only "N% of session used"; the rest lives in the popover.
  const short = $('#quota-short');
  const l = state.limits;
  const pct = l && l.session ? Math.round((l.session.utilization || 0) * 100) : null;
  short.textContent = pct === null ? 'Claude usage' : `${pct}% of session used`;
  short.className = 'meter-short' + (pct >= 90 ? ' hot' : pct >= 75 ? ' warn' : '');
}

function renderQuotaParts() {
  const box = $('#quota');
  const l = state.limits;
  if (!l) { box.hidden = true; return; }
  box.hidden = false;
  box.textContent = '';
  box.append(document.createTextNode('claude '));
  box.append(quotaPart('session', l.session), document.createTextNode(' · '), quotaPart('week', l.week));

  const quando = l.at ? new Date(l.at).toLocaleTimeString() : '';
  box.title = [
    'Claude usage, as of the last agent that ran' + (quando ? ' (' + quando + ')' : ''),
    resetLine('session', l.session),
    resetLine('week', l.week),
    l.status && l.status !== 'allowed' ? 'status: ' + l.status : '',
  ].filter(Boolean).join('\n');
}

// quotaPart colors the window as it fills up: yellow at 75%, red at 90% —
// the number alone doesn't warn that the wall is near.
function quotaPart(nome, w) {
  const pct = Math.round((w && w.utilization ? w.utilization : 0) * 100);
  const cls = pct >= 90 ? 'hot' : pct >= 75 ? 'warn' : '';
  return el('span', cls, `${nome} ${pct}%`);
}

function resetLine(nome, w) {
  if (!w || !w.resets_at) return '';
  return `${nome} resets at ${new Date(w.resets_at).toLocaleString()}`;
}

// renderSpend adds up what this session's reviews consumed. It sits in the top
// bar because it's the question asked without opening anything: what it cost.
function renderSpend() {
  const tokens = state.jobs.reduce((n, j) => n + (j.tokens || 0), 0);
  const cost = state.jobs.reduce((n, j) => n + (j.cost_usd || 0), 0);
  const box = $('#spend');
  if (!tokens && !cost) { box.hidden = true; return; }
  box.hidden = false;
  const parcial = state.jobs.some((j) => j.tokens_partial);
  box.dataset.partial = String(parcial);
  box.textContent = [tokens ? (parcial ? '~' : '') + fmtTokens(tokens) + ' tokens' : '',
    cost ? '$' + cost.toFixed(2) : ''].filter(Boolean).join(' · ');
  const n = state.jobs.filter((j) => j.tokens || j.cost_usd).length;
  box.title = `spend of ${n} review${n === 1 ? '' : 's'} in this session`;
}

function renderJobs() {
  const active = state.jobs.filter((j) => j.state === 'queued' || j.state === 'running').length;
  $('#queue-count').textContent = String(state.jobs.length);
  renderSpend();
  document.title = active ? `(${active}) Bazel` : 'Bazel';
  if (state.tab !== 'queue') return;

  const rail = $('#rail');
  const scroll = rail.scrollLeft;
  // The rail is rebuilt every second while a job runs. Whoever is on a card
  // (or its ✕) with the keyboard keeps their place: note it and put the focus
  // back on the same element of the rebuilt card (review F2).
  const focused = rail.contains(document.activeElement) ? document.activeElement : null;
  const focusCard = focused && focused.closest('.card') ? focused.closest('.card').dataset.jobId : null;
  const focusX = !!(focused && focused.classList.contains('card-x'));
  rail.innerHTML = '';
  // A card is a summary: which PR, what runs over it, how far it got. The
  // actions (cancel, publish, comment, keep it here) live in the job's detail,
  // next to what they act on.
  for (const job of state.jobs) {
    const card = el('div', 'card ' + job.state + (waiting(job) ? ' awaits' : '') + (state.activeJob === job.id ? ' on' : ''));
    tid(card, 'job-card', { jobId: job.id, state: job.state, prKey: job.pr.key });
    // A named group that takes the keyboard too: Enter or Space opens it, like a click.
    card.setAttribute('role', 'group');
    card.setAttribute('aria-label', `${job.pr.key} ${job.pr.title} — ${label(job)}`);
    card.tabIndex = 0;
    card.addEventListener('click', () => openJob(job.id));
    card.addEventListener('keydown', (e) => {
      if (e.target === card && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); openJob(job.id); }
    });

    const top = el('div', 'card-top');
    top.append(el('span', 'dot'), el('span', 'card-ref', `${job.pr.slug} #${job.pr.number}`));
    const sp = el('div', 'spacer');
    const x = tid(el('button', 'card-x', '✕'), 'job-remove');
    x.title = 'drop from the queue';
    x.setAttribute('aria-label', 'drop from the queue');
    x.addEventListener('click', (e) => { e.stopPropagation(); removeJob(job.id, x); });
    top.append(sp, x);
    card.append(top, el('div', 'title', job.pr.title));

    // The design's card is the dot, repo #n, ✕, the title and the bar. State,
    // agent and spend go to the tooltip and to the detail; the state stays in
    // the card as screen-reader text, so the card still says it out loud.
    const meta = el('div', 'sr-only');
    meta.append(tid(fillState(el('span', 'state ' + job.state), job), 'job-state'));
    if (job.agent) meta.append(el('span', null, ' · ' + job.agent));
    if (job.posted) meta.append(tid(el('span', null, ' ✓ published'), 'job-published'));
    const custo = gasto(job);
    if (custo) meta.append(tid(el('span', 'tokens', ' · ' + custo), 'job-tokens', { partial: !!job.tokens_partial }));
    card.title = [
      `${job.pr.key} — ${job.pr.title}`,
      label(job) + (job.agent ? ` · ${job.agent}${job.pipeline ? ' ⛓' : ''}` : ''),
      job.publishing ? '⇧ publishing' : job.posts ? '⇧ publishes to the PR on its own' : job.posted ? '✓ published' : '',
      custo,
    ].filter(Boolean).join('\n');
    card.append(meta, progressBar(job));
    rail.append(card);
  }
  rail.scrollLeft = scroll;
  if (focusCard) {
    const card = [...rail.querySelectorAll('.card')].find((c) => c.dataset.jobId === focusCard);
    const target = card && (focusX ? card.querySelector('.card-x') : card);
    if (target) target.focus({ preventScroll: true });
  }
}

// progressBar is the card's bar: the share of steps already finished, a moving
// stripe while a lone step runs, and full once the job is over.
function progressBar(job) {
  const bar = el('div', 'progress');
  const fill = el('span');
  const steps = job.steps || [];
  const finished = steps.filter((s) => s.state === 'done' || s.state === 'failed').length;
  let pct = 0;
  if (job.state === 'done' || job.state === 'failed' || job.state === 'canceled') pct = 100;
  else if (job.state === 'running' && steps.length > 1) pct = Math.max(6, Math.round((100 * finished) / steps.length));
  else if (job.state === 'running') bar.classList.add('indeterminate');
  fill.style.width = pct + '%';
  bar.append(fill);
  return bar;
}

// jobSeconds counts from started_at while the job runs, so the clock doesn't
// freeze between one server event and the next.
function jobSeconds(job) {
  if (job.state === 'running' && job.started_at) {
    return Math.max(0, Math.round((Date.now() - new Date(job.started_at)) / 1000));
  }
  return job.seconds || 0;
}

// labelParts splits the state from the time that comes with it. The state is
// uppercased by CSS; the time isn't — "8S" isn't eight seconds, it's eight siemens.
function labelParts(job) {
  switch (job.state) {
    case 'queued': return ['queued', ''];
    case 'running': return ['running', jobSeconds(job) + 's'];
    case 'done': return waiting(job)
      ? ['waiting on you', ''] : ['done in', job.seconds + 's'];
    case 'failed': return ['failed', ''];
    case 'canceled': return ['canceled', ''];
    default: return [job.state, ''];
  }
}

function label(job) {
  return labelParts(job).filter(Boolean).join(' ');
}

// fillState writes the state inside a .state, with the time unit outside the
// uppercase. It appends to the element without clearing it.
function fillState(span, job) {
  const [texto, tempo] = labelParts(job);
  // Waiting for you to publish is only half an ending: the warning color is
  // what sets the card apart from reviews that ask for nothing more.
  if (waiting(job)) span.classList.add('awaits');
  span.append(document.createTextNode(texto));
  if (tempo) span.append(document.createTextNode(' '), el('span', 'dur', tempo));
  return span;
}

function renderSaved() {
  const rail = $('#rail');
  rail.innerHTML = '';
  if (!state.saved.length) {
    docPanel().append(el('p', 'empty', 'no saved reviews yet'));
    return;
  }
  if (!state.activeSaved) {
    const v = docPanel();
    const list = el('div', 'saved-list');
    for (const s of state.saved) {
      const row = tid(el('div', 'saved'), 'saved-row', { name: s.name });
      row.append(el('div', 'saved-title', s.title || s.name),
        el('div', 'name', [s.agent, new Date(s.mod_time).toLocaleString(), s.name].filter(Boolean).join(' · ')));
      row.addEventListener('click', () => openSaved(s.name));
      operable(row, `open saved review ${s.title || s.name}`, () => openSaved(s.name));
      list.append(row);
    }
    v.append(list);
  }
}

// viewerSignature is what, when it changes, requires redrawing the panel. The
// one-second tick goes through here: without it, rebuilding the whole DOM would
// kill the log's scroll and the text selection of whoever is reading.
function viewerSignature(job) {
  return [
    job.id, job.state, job.cloning ? 'c' : '',
    (job.steps || []).map((s) => s.state).join(''),
    job.posted ? 'p' : '', job.post_error ? 'e' : '',
    // Waiting on you or not changes the note and the buttons, so "keep it
    // here" has to redraw the viewer even though state is still done.
    waiting(job) ? 'w' : '', job.publishable === false ? 'n' : '',
    state.bodies[job.id] ? 'b' : '',
    jobView(job),
  ].join('|');
}

// tickViewer updates only what moves on its own: the step and job clocks.
function tickViewer(job) {
  const rows = document.querySelectorAll('#viewer .step');
  const steps = job.steps || [];
  const offset = job.cloning ? 1 : 0;
  rows.forEach((row, i) => {
    const st = steps[i - offset];
    if (!st) return;
    const time = row.querySelector('.step-time');
    const secs = stepSeconds(st);
    if (time) time.textContent = secs + 's';
    else if (secs) row.append(el('span', 'step-time', secs + 's'));
  });
  const pill = document.querySelector('#viewer .job-head .state-pill');
  if (pill) {
    pill.textContent = pillText(job);
    pill.title = label(job);
  }
}

// jobView is the tab open in a job's detail. Each job remembers the one you
// picked; until then it follows the job: the log while it runs or after it
// failed, the report once there is one, the overview while it waits in line.
function jobView(job) {
  const picked = state.views[job.id];
  if (picked && (picked !== 'report' || job.state === 'done')) return picked;
  if (job.state === 'done') return 'report';
  if (job.state === 'running' || job.state === 'failed') return 'log';
  return 'overview';
}

function renderViewer() {
  if (state.tab !== 'queue') return;
  const v = $('#viewer');
  const job = state.jobs.find((j) => j.id === state.activeJob);
  if (!job) return;

  const sig = viewerSignature(job);
  if (v.dataset.sig === sig) {
    tickViewer(job);
    return;
  }
  v.innerHTML = '';
  v.dataset.sig = sig;
  v.dataset.jobId = job.id;
  const view = jobView(job);
  const box = el('div', 'job');
  box.append(jobHead(job), jobTabs(job, view));
  const body = el('div', 'job-body ' + view);
  if (view === 'log') renderLogView(body, job);
  else if (view === 'report') renderReportView(body, job);
  else if (view === 'history') renderHistoryView(body, job);
  else renderOverview(body, job);
  box.append(body);
  v.append(box);
  if (view !== 'log') body.scrollTop = 0;
}

// pillText is the word in the detail's state pill, as the design writes it:
// Running, Ready… The clock goes to its tooltip.
function pillText(job) {
  if (job.state === 'done') return waiting(job) ? 'Waiting on you' : 'Ready';
  return { queued: 'Queued', running: 'Running', failed: 'Failed', canceled: 'Canceled' }[job.state] || job.state;
}

// jobHead names the PR (repo#n · author), its title, the state and the one
// action that makes sense while the job is alive: cancel.
function jobHead(job) {
  const head = el('div', 'job-head');
  const grow = el('div', 'grow');
  const kicker = el('div', 'kicker');
  const link = prLink(job.pr);
  link.textContent = `${job.pr.repo} #${job.pr.number}`;
  kicker.append(link, document.createTextNode(' · ' + job.pr.author));
  grow.append(kicker, el('h2', null, job.pr.title));
  const pill = el('span', 'state state-pill ' + job.state + (waiting(job) ? ' awaits' : ''), pillText(job));
  pill.title = label(job);
  head.append(grow, pill);
  if (job.state === 'queued' || job.state === 'running') {
    const b = tid(el('button', 'btn small', 'Cancel'), 'job-cancel');
    b.addEventListener('click', () => cancelJob(job.id));
    head.append(b);
  }
  return head;
}

function jobTabs(job, view) {
  const bar = el('div', 'vtabs');
  bar.setAttribute('role', 'tablist');
  bar.setAttribute('aria-label', 'job detail');
  const tabs = [['overview', 'Overview'], ['history', 'History'], ['log', 'Live log'], ['report', 'Report']];
  for (const [key, name] of tabs) {
    const b = el('button', 'vtab' + (key === view ? ' on' : ''), name);
    b.setAttribute('role', 'tab');
    b.setAttribute('aria-selected', String(key === view));
    b.dataset.view = key;
    if (key === 'report' && job.state !== 'done') {
      b.disabled = true;
      b.title = 'the report shows up when the review is done';
    }
    b.addEventListener('click', () => {
      state.views[job.id] = key;
      if (key === 'log') pollLog(job.id);
      renderViewer();
    });
    bar.append(b);
  }
  return bar;
}

// --- the PR page (Overview) ---
//
// The Overview is the pull request itself, as the design draws it: who wants
// to merge what, the description, a timeline of the commits (and of Bazel's
// review), the files it touches; reviewers, labels and checks on the side.
// The page comes from /api/prs/…/details, cached by the server per head sha
// and here per PR for the session.

// ageOf is how long ago a date was, the way the list writes ages.
function ageOf(date) {
  const t = new Date(date).getTime();
  if (!t) return '';
  const s = Math.max(0, (Date.now() - t) / 1000);
  if (s < 60) return 'now';
  if (s < 3600) return Math.floor(s / 60) + 'm';
  if (s < 86400) return Math.floor(s / 3600) + 'h';
  return Math.floor(s / 86400) + 'd';
}

// prPath is a PR's path under /api/prs/.
const prPath = (pr) => `/api/prs/${pr.repo.split('/').map(encodeURIComponent).join('/')}/${pr.number}`;

// lazy reads url once into cache[key] and returns the entry — {view} or
// {error} — or null while it loads; onReady runs when it settles. A failure
// stays in the cache until someone asks again (the Retry button): a redraw
// never refetches by itself, so a PR GitHub can't serve can't loop gh calls
// (review F1).
function lazy(cache, key, url, onReady) {
  const have = cache[key];
  if (have && (have.view || have.error)) return have;
  if (have && have.loading) return null;
  cache[key] = { loading: true };
  api(url)
    .then((view) => { cache[key] = { view }; onReady(); })
    .catch((err) => { cache[key] = { error: err.message }; onReady(); });
  return null;
}

// retryButton forgets a failed read and draws again, which reads it again.
function retryButton(cache, key, redraw) {
  const b = tid(el('button', 'btn small', 'Retry'), 'retry');
  b.addEventListener('click', () => { delete cache[key]; redraw(); });
  return b;
}

function sectionLabel(text, extra) {
  const h = el('h4', 'plabel', text);
  if (extra) h.append(el('span', 'plabel-extra', extra));
  return h;
}

// renderPRPage draws the page of pr into body. job, when given, is the review
// open on it: it adds Bazel's line to the timeline and a small block with the
// review's own facts at the end.
function renderPRPage(body, pr, job, redraw) {
  // A PR outside the list is only read from GitHub because you opened it
  // (offlist=1); the server rate-limits those reads.
  const inList = state.prs.some((p) => p.key === pr.key);
  const got = lazy(state.details, pr.key, prPath(pr) + '/details' + (inList ? '' : '?offlist=1'), redraw);
  const d = got && got.view;
  const err = got && got.error;
  const page = tid(el('div', 'prpage'), 'pr-page', { prKey: pr.key, loaded: !!d });
  const main = el('div', 'prmain');
  const side = el('aside', 'prside');
  page.append(main, side);
  body.append(page);

  const listed = state.prs.find((p) => p.key === pr.key) || {};
  const commits = d ? d.commits : [];
  const merge = el('p', 'pmerge');
  merge.append(el('b', null, pr.author), document.createTextNode(` wants to merge ${d ? commits.length + ' commit' + (commits.length === 1 ? '' : 's') : 'its commits'} into `),
    el('code', null, pr.base || listed.base || ''), document.createTextNode(' from '), el('code', null, pr.branch || listed.branch || ''));
  main.append(merge);
  if (listed.changed_since_review) {
    main.append(el('div', 'stale-box', `This PR changed after the review from ${ago(listed.reviewed_at)}`
      + (listed.review_agent ? ` (${listed.review_agent})` : '') + ' — what is here is no longer what the agent read.'));
  }
  const html = (d && d.body_html) || listed.body_html;
  const desc = el('div', 'md pdesc');
  if (html) desc.innerHTML = html; else desc.append(el('p', 'dim', d || err ? '(no description)' : 'loading the PR…'));
  main.append(desc);
  if (err) {
    const p = tid(el('p', 'dim perr'), 'pr-error');
    p.append(document.createTextNode('Could not read the rest of the PR from GitHub: ' + err + ' '), retryButton(state.details, pr.key, redraw));
    main.append(p);
  }

  // ACTIVITY: opened, each commit, and Bazel's reviews of it.
  if (d) {
    main.append(sectionLabel('Activity'));
    const tl = tid(el('ol', 'timeline'), 'pr-activity');
    const item = (who, what, extra, when, cls) => {
      const li = el('li', cls || '');
      li.append(el('span', 'tdot'), el('b', null, who), el('span', null, ' ' + what));
      if (extra) li.append(el('code', 'tsha', extra));
      li.append(el('span', 'twhen', when));
      tl.append(li);
    };
    if (d.created_at) item(pr.author, 'opened this pull request', '', ageOf(d.created_at));
    for (const c of commits) item(c.author || pr.author, c.headline, c.short, ageOf(c.date));
    const reviews = state.jobs.filter((j) => j.pr.key === pr.key);
    for (const j of reviews) {
      const live = j.state === 'running' || j.state === 'queued';
      const verb = j.history
        ? (live ? 'is writing the history with' : j.state === 'done' ? 'wrote the history with' : j.state === 'failed' ? 'failed the history with' : 'canceled the history with')
        : (live ? 'is reviewing with' : j.state === 'done' ? 'reviewed with' : j.state === 'failed' ? 'failed a review with' : 'canceled a review with');
      const li = el('li', 'tbazel' + (j.id === (job && job.id) ? ' on' : ''));
      li.append(el('span', 'tdot'), el('b', null, 'bazel'), el('span', null, ` ${verb} ${j.agent || 'an agent'}`),
        el('span', 'twhen', j.state === 'running' ? 'now' : ''));
      if (!job || j.id !== job.id) {
        operable(li, `open the review ${j.id}`, () => openJob(j.id));
        li.addEventListener('click', () => openJob(j.id));
      }
      tl.append(li);
    }
    main.append(tl);

    // FILES CHANGED with +/− and a five-square bar.
    const adds = d.files.reduce((n, f) => n + f.additions, 0);
    const dels = d.files.reduce((n, f) => n + f.deletions, 0);
    const fl = sectionLabel('Files changed', ` ${d.files.length} file${d.files.length === 1 ? '' : 's'} · `);
    fl.lastChild.append(el('span', 'add', '+' + adds), document.createTextNode(' '), el('span', 'del', '−' + dels));
    main.append(fl);
    const files = tid(el('div', 'pfiles'), 'pr-files');
    for (const f of d.files) {
      const row = el('div', 'pfile');
      row.append(el('code', 'pfile-path', f.path), el('span', 'add', '+' + f.additions), el('span', 'del', '−' + f.deletions));
      const bar = el('span', 'pbar');
      const total = f.additions + f.deletions;
      const green = total ? Math.round((5 * f.additions) / total) : 0;
      for (let i = 0; i < 5; i++) bar.append(el('i', i < green ? 'a' : total ? 'd' : ''));
      bar.setAttribute('aria-hidden', 'true');
      row.append(bar);
      files.append(row);
    }
    main.append(files);

    // The side: reviewers, labels, checks.
    side.append(sectionLabel('Reviewers'));
    if (!d.reviewers.length) side.append(el('p', 'dim', 'nobody yet'));
    for (const r of d.reviewers) {
      const row = tid(el('div', 'prev'), 'pr-reviewer', { login: r.login, state: r.state });
      row.append(el('span', 'avatar', (r.login[0] || '?').toUpperCase()), el('span', 'prev-who', r.login), el('span', 'prev-state ' + r.state.replace(/\s/g, '-'), r.state));
      side.append(row);
    }
    side.append(sectionLabel('Labels'));
    const labels = el('div', 'plabels');
    if (!d.labels.length) labels.append(el('span', 'dim', 'none'));
    for (const l of d.labels) labels.append(tid(el('span', 'plabel-chip', l), 'pr-label'));
    side.append(labels);
    side.append(sectionLabel('Checks'));
    if (!d.checks.length) side.append(el('p', 'dim', 'none reported'));
    for (const c of d.checks) {
      const row = tid(el('div', 'pcheck ' + c.state), 'pr-check', { name: c.name, state: c.state });
      row.append(el('span', 'tdot'), el('code', null, c.name), el('span', 'pcheck-state', c.state));
      side.append(row);
    }
  }
  return main;
}

// renderOverview is everything about the job that is not the report or the
// log: where it runs, with what, its steps, what it cost and where it landed.
function renderOverview(body, job) {
  // The page of the PR first, like the design; the review's own facts go to
  // a small block at the end of it.
  const main = renderPRPage(body, job.pr, job, () => { if (state.activeJob === job.id) { delete $('#viewer').dataset.sig; renderViewer(); } });
  const block = el('section', 'jobfacts');
  main.append(sectionLabel('This review'), block);
  body = block;
  const msg = {
    queued: 'Queued — starts as soon as a worker frees up.',
    running: job.publishing
      ? 'Publishing the review you read — cloning the PR and commenting line by line. The review comes back to this card when it is done.'
      : 'Running inside the PR clone. You can close the tab, the review keeps going.',
    canceled: 'Canceled.',
  }[job.state];
  if (msg) body.append(el('p', 'dim', msg));
  if (job.state === 'failed') body.append(el('div', 'error-box', job.error || 'failed'));
  if (job.post_error) body.append(el('div', 'error-box', 'failed to publish: ' + job.post_error));

  const facts = el('dl', 'facts');
  const fact = (k, ...vals) => {
    const dd = el('dd');
    dd.append(...vals);
    facts.append(el('dt', null, k), dd);
  };
  // PR, author and branch are on the page above; the block keeps the review's own.
  if (job.agent) {
    const a = el('span', null, job.agent + (job.pipeline ? ' ⛓' : ''));
    a.title = agentTitle(job.agent);
    fact('Agent', a);
  }
  if (job.posts) fact('Publishing', el('span', 'tag posts', '⇧ publishes to the PR on its own'));
  else if (job.posted) fact('Publishing', el('span', 'tag reviewed', 'published on the PR'));
  const custo = gasto(job);
  if (custo) {
    const g = el('span', 'tokens', custo + (job.state === 'done' ? ' · ' + jobSeconds(job) + 's' : ''));
    g.title = gastoTitle(job);
    fact('Spend', g);
  }
  if (job.saved_to) fact('Saved to', el('code', null, job.saved_to));
  if (job.workdir) fact('Clone kept at', el('code', null, job.workdir));
  if (job.truncated) fact('Diff', 'truncated');
  body.append(facts);

  if (job.cloning || (job.steps && job.steps.length)) {
    body.append(el('p', 'section-label', 'steps'));
    body.append(stepsBox(job));
  }
}

function renderLogView(body, job) {
  if (job.state === 'failed') body.append(el('div', 'error-box log-error', job.error || 'failed'));
  body.append(logPanel(job));
  pollLog(job.id);
}

// renderHistoryView is the PR's story, on demand: the history agent writes
// what the PR does, the decisions taken (with the commit and the source of
// each, and what was rejected) and what is still open. Nothing runs until you
// ask: "Generate" queues it as a job, and the result is kept per PR and head.
function renderHistoryView(body, job) {
  const pr = job.pr;
  const redraw = () => { if (state.activeJob === job.id && jobView(job) === 'history') { delete $('#viewer').dataset.sig; renderViewer(); } };
  const got = lazy(state.histories, pr.key, prPath(pr) + '/history', redraw);
  const box = tid(el('div', 'history'), 'history', { prKey: pr.key });
  body.append(box);
  if (!got) { box.append(el('p', 'dim', 'loading the history…')); return; }
  if (got.error) {
    const e = el('div', 'error-box', 'Could not read the history: ' + got.error + ' ');
    e.append(retryButton(state.histories, pr.key, redraw));
    box.append(e);
    return;
  }
  const h = got.view;

  const generate = (label) => {
    const b = tid(el('button', label === 'Generate' ? 'btn primary' : 'btn small ghost link', label),
      label === 'Generate' ? 'history-generate' : 'history-regenerate');
    b.title = `runs ${h.agent} over this PR — a job in the queue; nothing goes to the PR`;
    b.addEventListener('click', async () => {
      b.disabled = true;
      clearAlert();
      try {
        const view = await api(prPath(pr) + '/history', { method: 'POST' });
        upsertJob(view);
        delete state.histories[pr.key];
        redraw();
      } catch (err) {
        alertError('Could not start the history: ' + err.message);
        b.disabled = false;
      }
    });
    return b;
  };

  if (h.job) {
    const p = el('p', 'dim');
    p.append(document.createTextNode(`Writing the history with ${h.agent}… `));
    const a = el('button', 'btn small ghost link', 'watch its live log');
    a.addEventListener('click', () => { state.views[h.job.id] = 'log'; openJob(h.job.id); });
    p.append(a);
    box.append(p);
    if (!h.found) return;
  }
  if (!h.found) {
    const empty = el('div', 'history-empty');
    empty.append(el('p', null, 'No history for this PR yet.'),
      el('p', 'dim', `${h.agent} reads the commits, the description and the review threads, and writes what the PR does, the decisions taken along the way and what is still open.`),
      generate('Generate'));
    box.append(empty);
    return;
  }

  const head = el('div', 'history-head');
  const lab = sectionLabel('What it does', ` ${h.agent}` + (h.commits_read ? ` · ${h.commits_read} commits read` : ''));
  head.append(lab, el('div', 'spacer'));
  if (!h.job) head.append(generate('Regenerate'));
  box.append(head);
  if (h.stale) box.append(el('p', 'stale-box', 'Written at an older head of this PR — the commits since are not in it. Regenerate to bring it up to date.'));
  if (h.what_html) {
    const what = el('div', 'md history-what');
    what.innerHTML = h.what_html;
    box.append(what);
  }
  if (h.decisions.length) {
    box.append(sectionLabel('Decisions'));
    const list = el('ol', 'decisions');
    for (const d of h.decisions) {
      const li = tid(el('li', 'decision'), 'history-decision', { n: d.n });
      li.append(el('span', 'dnum', String(d.n)));
      const main = el('div', 'dmain');
      const t = el('div', 'dtitle');
      t.append(el('b', null, d.title));
      // One commit, or the first of several with "+N" (all of them in its tooltip).
      const sha = d.sha && d.shas && d.shas.length > 1 ? `${d.sha} +${d.shas.length - 1}` : d.sha;
      if (sha || d.source) {
        const meta = el('span', 'dmeta', [sha, d.source].filter(Boolean).join(' · '));
        if (d.shas && d.shas.length > 1) meta.title = d.shas.join(', ');
        t.append(meta);
      }
      main.append(t);
      if (d.body_html) {
        const b = el('div', 'md dbody');
        b.innerHTML = d.body_html;
        main.append(b);
      }
      for (const r of d.rejected) {
        const p = el('p', 'drej');
        p.append(el('span', null, 'Rejected: '), document.createTextNode(r));
        main.append(p);
      }
      li.append(main);
      list.append(li);
    }
    box.append(list);
  }
  if (h.still_open_html.length || h.still_open_note_html) {
    box.append(sectionLabel('Still open'));
    if (h.still_open_note_html) {
      const note = el('div', 'md dbody');
      note.innerHTML = h.still_open_note_html;
      box.append(note);
    }
    for (const q of h.still_open_html) {
      const row = el('div', 'open-q');
      const txt = el('div', 'md');
      txt.innerHTML = q;
      row.append(el('span', 'qmark', '?'), txt);
      box.append(row);
    }
  }
  if (h.rest_html) {
    const more = el('details', 'rmore');
    const rest = el('div', 'md');
    rest.innerHTML = h.rest_html;
    more.append(el('summary', null, 'More from the history'), rest);
    box.append(more);
  }
}



function renderReportView(v, job) {
  const html = state.bodies[job.id];
  if (!html) {
    v.append(el('p', 'dim', 'loading review…'));
    return;
  }
  // It ended on a `publish`: the pipeline sent nothing, and this is where you
  // choose what goes. Saying so before the review is what makes you read it
  // as the one who decides, not as someone who only checks.
  if (waiting(job)) {
    v.append(el('p', 'awaits-note', 'Nothing has gone to the PR yet. Untick the findings that should not go,'
      + ' then publish at the end, or keep it here.'));
  }
  // With some of the steps kept out of the PR, the screen separates the two: the
  // ticks go only on what goes up, with the same numbering the server uses to drop
  // findings — on the whole report, the number would point at another finding.
  const split = job.publishable !== false ? state.splits[job.id] : null;
  const key = pickKey('job', job.id);
  let total;
  if (split) {
    if (split.ctx) {
      v.append(el('p', 'section-label', 'stays in Bazel — not published'));
      const ctx = tid(el('div', 'md context'), 'review-context');
      ctx.innerHTML = split.ctx;
      v.append(ctx);
    }
    v.append(el('p', 'section-label', 'goes to the PR'));
    const pub = tid(el('div', 'md'), 'review-body');
    pub.innerHTML = split.pub;
    v.append(pub);
    total = decorateFindings(pub, key);
  } else {
    const md = tid(el('div', 'md'), 'review-body');
    md.innerHTML = html;
    v.append(md);
    total = decorateFindings(md, key, job.publishable !== false);
  }

  // The actions come after the review, in a bar pinned to the panel's bottom:
  // what goes to the PR is decided by reading, and each action still asks a
  // confirm() that lists what stays out.
  let actionBar = null;
  if (!job.publishing && job.publishable === false) {
    // An agent that produces no review has nowhere to publish. Saying so here
    // beats showing nothing: you have just read the report and the next
    // question is always "so, does this go to the PR?".
    v.append(el('p', 'dim end-note', `This is not a review — what "${job.agent}" produces stays in Bazel.`
      + ' Run a reviewing agent over this PR when you want something to publish.'));
  } else if (!job.publishing) {
    // The design's footer: "N of M findings" on the left, the two ways to the
    // PR on the right — a plain comment, and Publish with inline comments.
    const bar = el('div', 'viewer-actions');
    bar.append(total ? pickSummary(key, total) : el('span', 'dim', 'no findings to pick'), el('div', 'spacer'));
    if (waiting(job)) {
      const keep = tid(el('button', 'btn ghost', 'Keep it here'), 'keep-here');
      keep.title = 'nothing goes to the PR; the review stays in Bazel, and in the Saved tab';
      keep.addEventListener('click', () => dismissJob(job.id, keep));
      bar.append(keep);
    }
    if (job.posted) {
      bar.append(tid(el('span', 'state done', '✓ already on the PR'), 'already-published'));
    } else {
      const cm = tid(el('button', 'btn ghost', 'As comment'), 'paste-comment');
      cm.title = 'pastes the review as a single comment on the PR, no agent';
      cm.addEventListener('click', () => postJob(job.id, cm));
      bar.append(cm);
    }
    const pub = tid(el('button', 'btn primary', '↗ Publish'), 'publish-inline');
    pub.title = 'runs the post skill and publishes to the PR with inline comments';
    pub.addEventListener('click', () => publishJob(job.id, pub));
    bar.append(pub);
    actionBar = bar;
  }
  if (job.post_error) v.append(tid(el('div', 'error-box', 'failed to publish: ' + job.post_error), 'post-error'));

  // What the review cost, at its end — which is where you arrive after reading.
  const custo = gasto(job);
  const foot = tid(el('p', 'dim tokens-foot'), 'review-footer');
  foot.textContent = [custo && custo + ' · ' + jobSeconds(job) + 's', job.saved_to && 'saved to ' + job.saved_to,
    job.workdir && 'clone kept at ' + job.workdir, job.truncated && 'diff truncated']
    .filter(Boolean).join(' · ');
  if (custo) foot.title = gastoTitle(job);
  v.append(foot);
  // The footer bar goes last: pinned to the panel's bottom edge.
  if (actionBar) v.append(actionBar);
}

// --- bindings ---

function wire() {
  $('#refresh').addEventListener('click', () => loadPRs(true));
  $('#run').addEventListener('click', startReview);
  $('#agent').addEventListener('change', (e) => {
    state.agent = e.target.value;
    $('#agent').title = agentTitle(state.agent);
  });
  // Server events only arrive at step boundaries; this tick is what makes the
  // time of the current step move on screen.
  setInterval(() => {
    if (!state.jobs.some((j) => j.state === 'running')) return;
    renderJobs();
    const active = state.jobs.find((j) => j.id === state.activeJob);
    if (!active) return;
    renderViewer();
    // The log is only fetched for the review that is open and still alive.
    if (active.state === 'running' && logState(active.id).live && $('#log-terms')) pollLog(active.id);
  }, 1000);
  $('#filter').addEventListener('input', (e) => { state.filter = e.target.value; renderPRs(); });
  $('#owner-filter').addEventListener('change', (e) => {
    state.ownerFilter = e.target.value;
    // Switching owner rebuilds the repository selector: the chosen repository
    // either belongs to the new owner or is dropped with the old one.
    renderRepoFilters();
    renderPRs();
  });
  $('#repo-filter').addEventListener('change', (e) => {
    state.repoFilter = e.target.value;
    e.target.classList.toggle('on', !!state.repoFilter);
    renderPRs();
  });
  // The review state is a row of chips instead of a selector: it is the filter
  // used most, and one click beats opening a menu.
  document.querySelectorAll('#review-filter button').forEach((b) => {
    b.addEventListener('click', () => {
      state.reviewFilter = b.dataset.review;
      document.querySelectorAll('#review-filter button').forEach((x) => {
        x.classList.toggle('on', x === b);
        x.setAttribute('aria-pressed', String(x === b));
      });
      renderPRs();
    });
  });
  $('#toggle-prs').addEventListener('click', () => togglePRs(!state.prsHidden));
  // The filter menu next to the search: whose PRs, drafts, owner and repo.
  // It opens and closes on its button, and closes on Escape or a click
  // anywhere outside it.
  const fbtn = $('#filter-btn');
  const fpop = $('#filter-pop');
  const setPop = (open) => {
    fpop.hidden = !open;
    fbtn.setAttribute('aria-expanded', String(open));
  };
  fbtn.addEventListener('click', (e) => { e.stopPropagation(); setPop(fpop.hidden); });
  document.addEventListener('click', (e) => {
    if (!fpop.hidden && !fpop.contains(e.target) && e.target !== fbtn) setPop(false);
  });
  fpop.addEventListener('keydown', (e) => { if (e.key === 'Escape') { setPop(false); fbtn.focus(); } });
  $('#hide-drafts').addEventListener('change', (e) => { state.hideDrafts = e.target.checked; renderPRs(); });
  $('#select-all').addEventListener('change', (e) => { selectVisible(e.target.checked); renderPRs(); });
  document.querySelectorAll('#scope button').forEach((b) => {
    b.addEventListener('click', () => {
      state.scope = b.dataset.scope;
      document.querySelectorAll('#scope button').forEach((x) => {
        x.classList.toggle('on', x === b);
        x.setAttribute('aria-pressed', String(x === b));
      });
      renderPRs();
    });
  });
  document.querySelectorAll('.tab').forEach((t) => {
    t.addEventListener('click', () => { state.tab = t.dataset.tab; state.activeSaved = null; renderTabs(); if (state.tab === 'queue') renderViewer(); });
  });
  window.addEventListener('hashchange', route);
  $('#repo-form').addEventListener('submit', addRepo);
  // The whole file, to take to another machine. The server sends it with
  // Content-Disposition, so a link is enough — no Blob, no copy in memory.
  $('#config-download').addEventListener('click', () => { window.location.href = '/api/config/file'; });

  document.addEventListener('keydown', (e) => {
    // Escape on the config goes back to the dashboard: it is what the modal did,
    // and the fingers of those who already used Bazel still know it.
    if (e.key === 'Escape' && pageFromHash() === 'config') location.hash = '#/';
    if (e.key === '/' && document.activeElement !== $('#filter')) { e.preventDefault(); $('#filter').focus(); }
    if (e.key === 'r' && (e.metaKey || e.ctrlKey)) return;
  });
}

// renderAgentList is the config's agent list: what each one calls, whether the
// skill is installed, who is the default and the button to take it off the list.
//
// The list starts empty on purpose — you're the one who builds it, in the
// section below, from the skills installed on this machine.
function renderAgentList() {
  const box = $('#agent-list');
  box.innerHTML = '';
  const meus = state.agents.filter((a) => !a.publisher);
  if (!meus.length) {
    box.append(el('p', 'none', 'no agents yet — pick below, among the installed skills, which ones become agents.'));
  }
  for (const a of state.agents) {
    const row = el('div', 'agent-row');
    if (a.pipeline) tid(row, 'pipeline-row', { pipeline: a.name });
    else tid(row, 'agent-row', { agent: a.name, default: defaultAgent() === a.name, missingSkill: (a.skills || []).some((k) => !k.installed) });

    const top = el('div', 'agent-top');
    top.append(el('span', 'agent-name', a.name));
    // Badges, as the design draws them: default, publishes, pipeline, used
    // when publishing — and "not a review" for output that stays here.
    if (defaultAgent() === a.name && !a.publisher) top.append(el('span', 'badge accent', 'default'));
    if (a.posts && !a.publisher) top.append(el('span', 'badge', 'publishes'));
    if (a.pipeline) top.append(el('span', 'badge', 'pipeline'));
    if (a.publishable === false) top.append(el('span', 'badge', 'not a review'));
    if (a.publisher) top.append(el('span', 'badge', 'used when publishing'));
    // What the dashboard picker has on now, when it is not the default: it says
    // what runs next from this browser, while the file still says the default.
    if (!a.publisher && state.agent === a.name && defaultAgent() !== a.name) {
      const t = el('span', 'badge', 'picked on the dashboard');
      t.title = 'selected in the dashboard right now; config.yaml still has the default above';
      top.append(t);
    }

    // The publishing agent isn't a choice in the list: it is what runs when you
    // send a review you've already read to be published, and it can't be taken off.
    if (!a.publisher) {
      top.append(el('div', 'spacer'));
      if (defaultAgent() !== a.name) {
        const d = tid(el('button', 'btn small ghost link', 'Make default'), 'agent-default');
        d.title = 'becomes the agent the selector starts on';
        d.addEventListener('click', () => agentAction('/api/agents/' + encodeURIComponent(a.name) + '/default', 'POST', d));
        top.append(d);
      }
      // The publishing tick. An agent that publishes on its own doesn't show here:
      // it writes on the PR by itself, and turning off a button wouldn't
      // stop it from anything — the way is to take it off the list.
      if (!a.posts && !a.pipeline) {
        const pb = tid(el('button', 'btn small ghost',
          a.publishable === false ? 'output stays here' : '⇧ output can go to the PR'), 'agent-publishable');
        pb.title = a.publishable === false
          ? 'this agent produces no review — Bazel offers no way to publish it. Click to allow it again.'
          : 'the result of this agent can be published to the PR. Click if it is not a review.';
        pb.addEventListener('click', () => agentAction(
          '/api/agents/' + encodeURIComponent(a.name) + '/publishable', 'POST', pb,
          { publishable: a.publishable === false }));
        top.append(pb);
      }
      // Pipeline and agent leave through different doors: one lives under
      // `pipelines:`, the other under `agents:`.
      const base = a.pipeline ? '/api/pipelines/' : '/api/agents/';
      const rm = tid(el('button', 'btn small ghost', 'Remove'), a.pipeline ? 'pipeline-remove' : 'agent-remove');
      rm.addEventListener('click', () => agentAction(base + encodeURIComponent(a.name), 'DELETE', rm));
      top.append(rm);
    }
    row.append(top);

    if (a.description) row.append(el('div', 'agent-desc', a.description));
    if (a.pipeline) row.append(el('div', 'agent-desc', (a.steps || []).join(' → ')));

    const usa = el('div', 'agent-skills');
    for (const [i, sk] of (a.skills || []).entries()) {
      if (i && a.pipeline) usa.append(el('span', 'skill-arrow', '→'));
      const t = el('span', 'skill-ref ' + (sk.installed ? 'ok' : 'missing'), '/' + sk.name);
      t.title = sk.installed
        ? 'installed in ' + state.skillsDir
        : `this skill is not in ${state.skillsDir} — the agent will fail at run time`;
      usa.append(t);
    }
    if (!(a.skills || []).length) usa.append(el('span', 'dim', 'calls no skill'));
    row.append(usa);
    if ((a.skills || []).some((k) => !k.installed)) {
      row.append(el('p', 'agent-warn', 'Calls a skill that isn\'t installed here — it would fail at run time.'));
    }

    box.append(row);
  }
}

// agentAction sends the change and redraws everything that depends on the list:
// the picker, the config and the yaml on disk, which changed too.
async function agentAction(path, method, btn, body) {
  btn.disabled = true;
  try {
    const res = await api(path, body ? { method, body: JSON.stringify(body) } : { method });
    applyAgents(res.agents || [], path.endsWith('/default'));
    // Emptying the list again is going back to the initial state: with no agent
    // there is no review, and whoever has the config open needs to know.
    banner(selectableAgents().length ? '' : 'No agents configured — pick below which skills become agents.');
    await refreshConfig();
    return true;
  } catch (err) {
    banner(err.message);
    btn.disabled = false;
    return false;
  }
}

// --- pipeline builder ---
//
// The sequence is a chain of cards linked by arrows: you drag an agent from the
// tray into it, and drag a card to change the order. The steps are agents from
// the list above, not loose skills — that is what guarantees each step already
// has its prompt, command and publishing tick resolved, and it is what the
// server requires.
//
// Clicking still works for everything dragging does: a precise mouse can't be a
// requirement to build a pipeline.

// arrasto holds what is being dragged. `from` is the index in the chain when it
// is a card already there; `add` is the name when it comes from the tray.
let arrasto = null;
// alvo is the position where the step would land, and it is what draws the open gap.
let alvo = null;

const RESERVADOS = ['publish'];
const ehReservado = (n) => RESERVADOS.includes(n);

function renderPipelineBuilder() {
  const box = $('#pipeline-builder');
  box.innerHTML = '';

  const disponiveis = state.agents.filter((a) => !a.publisher && !a.pipeline);
  // One agent is enough: `review-fleet → publish` is a one-agent pipeline, and
  // it is precisely the one that exists so you choose what goes before sending.
  if (!disponiveis.length) {
    box.append(el('p', 'none', 'add an agent above, and you can chain it into a pipeline here.'));
    state.pipeSteps = [];
    return;
  }
  // An agent removed up there can't stay in the draft here. The reserved steps
  // stay: they don't depend on the list.
  state.pipeSteps = state.pipeSteps.filter((n) =>
    ehReservado(n) || disponiveis.some((a) => a.name === n));

  box.append(cadeia(), bandeja(disponiveis), linhaDeCriar());
}

// cadeia is the assembled sequence: cards and arrows, and the gaps between them
// where a dragged step lands.
function cadeia() {
  const fluxo = tid(el('div', 'pipe-flow'), 'pipeline-chain');
  if (!state.pipeSteps.length) fluxo.classList.add('empty');

  const solta = (i) => {
    const z = el('div', 'pipe-gap' + (alvo === i ? ' over' : ''));
    z.addEventListener('dragover', (e) => {
      if (!arrasto) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = arrasto.add ? 'copy' : 'move';
      if (alvo !== i) { alvo = i; renderPipelineBuilder(); }
    });
    z.addEventListener('drop', (e) => { e.preventDefault(); solte(i); });
    return z;
  };

  fluxo.append(solta(0));
  state.pipeSteps.forEach((nome, i) => {
    if (i > 0) fluxo.append(el('span', 'pipe-arrow', '→'));
    fluxo.append(no(nome, i));
    fluxo.append(solta(i + 1));
  });
  if (!state.pipeSteps.length) {
    fluxo.append(el('span', 'dim', 'drag an agent in here, or click one below'));
  }
  return fluxo;
}

// no is a step in the chain: the draggable card, with the order number and what
// it is. The × removes; dragging moves.
function no(nome, i) {
  const card = tid(el('div', 'pipe-node' + (ehReservado(nome) ? ' reserved' : '')
    + (arrasto && arrasto.from === i ? ' dragging' : '')), 'pipeline-step', { step: nome });
  card.draggable = true;
  card.title = 'drag to move this step';

  const topo = el('div', 'pipe-node-top');
  topo.append(el('span', 'n', String(i + 1)));
  topo.append(el('span', 'pipe-node-name', nome));
  const tira = el('button', 'x', '×');
  tira.title = 'take this step out';
  tira.draggable = false;
  tira.addEventListener('click', () => {
    state.pipeSteps.splice(i, 1);
    renderPipelineBuilder();
  });
  topo.append(tira);
  card.append(topo);

  const a = state.agents.find((x) => x.name === nome);
  const nota = nome === 'publish' ? 'you choose what goes'
    : a && a.publishable === false ? 'stays here' : '';
  if (nota) card.append(el('div', 'pipe-node-note', nota));

  card.addEventListener('dragstart', (e) => {
    arrasto = { from: i };
    e.dataTransfer.effectAllowed = 'move';
    // Firefox only starts the drag if there is data in the dataTransfer.
    e.dataTransfer.setData('text/plain', nome);
    setTimeout(renderPipelineBuilder, 0);
  });
  card.addEventListener('dragend', () => { arrasto = null; alvo = null; renderPipelineBuilder(); });
  return card;
}

// bandeja is the steps that can be added: the agents in the list and the
// publish, which Bazel runs on its own.
function bandeja(disponiveis) {
  const box = tid(el('div', 'pipe-tray'), 'pipeline-tray');
  const fechada = state.pipeSteps.includes('publish');

  const item = (nome, rotulo, dica, porque) => {
    const b = el('button', 'btn small ghost' + (ehReservado(nome) ? ' reserved' : ''), rotulo);
    if (porque) {
      b.disabled = true;
      b.title = porque;
      box.append(b);
      return;
    }
    b.title = dica;
    b.draggable = true;
    b.addEventListener('dragstart', (e) => {
      arrasto = { add: nome };
      e.dataTransfer.effectAllowed = 'copy';
      e.dataTransfer.setData('text/plain', nome);
    });
    b.addEventListener('dragend', () => { arrasto = null; alvo = null; renderPipelineBuilder(); });
    // Clicking appends at the end: it is the path for those who don't want to drag.
    b.addEventListener('click', () => {
      state.pipeSteps.push(nome);
      renderPipelineBuilder();
    });
    box.append(b);
  };

  for (const a of disponiveis) {
    item(a.name, '+ ' + a.name, a.description || `adds ${a.name} as a step`,
      fechada ? 'publish is the last step — nothing runs after it'
        : state.pipeSteps.includes(a.name) ? 'already a step in this pipeline'
        : '');
  }

  box.append(el('span', 'pipe-sep', '·'));
  // The same rules the server applies — disabling here is saying why
  // beforehand, instead of refusing afterwards.
  const semAgente = !state.pipeSteps.some((n) => !ehReservado(n));
  const nadaPublicavel = !state.pipeSteps.some((n) => {
    const a = state.agents.find((x) => x.name === n);
    return a && a.publishable !== false;
  });
  item('publish', '+ publish', 'stops with what would go to the PR — you untick what should not, then send it with the publishing agent',
    semAgente ? 'needs an agent before it — there would be nothing to publish yet'
      : fechada ? 'a pipeline publishes at most once'
      : nadaPublicavel ? 'nothing before it produces a review that can go to the PR'
      : '');
  return box;
}

// solte applies the drag: moving a card, or bringing a new step from the
// tray to the position where it was dropped.
function solte(pos) {
  if (!arrasto) return;
  if (arrasto.add) {
    state.pipeSteps.splice(pos, 0, arrasto.add);
  } else {
    const de = arrasto.from;
    const [x] = state.pipeSteps.splice(de, 1);
    // Removing the card before inserting shortens the chain: a position after it
    // is no longer the same.
    state.pipeSteps.splice(de < pos ? pos - 1 : pos, 0, x);
  }
  arrasto = null;
  alvo = null;
  renderPipelineBuilder();
}

// linhaDeCriar is the name and the button. The button says what is blocking it,
// instead of just being greyed out.
function linhaDeCriar() {
  const linha = el('div', 'pipe-row');
  const nome = tid(el('input'), 'pipeline-name');
  nome.type = 'text';
  nome.placeholder = 'pipeline name — how it shows up in the selector';
  nome.autocomplete = 'off';
  nome.value = state.pipeName || '';
  nome.addEventListener('input', () => { state.pipeName = nome.value; });

  const criar = tid(el('button', 'btn primary', 'Create'), 'pipeline-create');
  const agentes = state.pipeSteps.filter((n) => !ehReservado(n)).length;
  let impede = '';
  if (!agentes) impede = 'a pipeline needs at least one agent';
  else if (agentes < 2 && state.pipeSteps.length < 2) impede = 'a pipeline chains two agents or more, or one agent and a step of its own';
  criar.disabled = !!impede;
  criar.title = impede || 'creates the sequence and puts it in the selector';

  const enviar = async () => {
    const n = (state.pipeName || '').trim();
    if (!n) { banner('give the pipeline a name.'); nome.focus(); return; }
    const ok = await agentAction('/api/pipelines', 'POST', criar, { name: n, steps: state.pipeSteps });
    if (!ok) return; // the error is already in the banner and the draft is still there
    // Once created, the draft resets: the next build starts from empty, and the
    // pipeline that was just born is already in the list above.
    state.pipeSteps = [];
    state.pipeName = '';
    renderPipelineBuilder();
  };
  criar.addEventListener('click', enviar);
  nome.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); enviar(); } });
  linha.append(nome, criar);
  return linha;
}

// renderSkillList shows what is actually installed — the list that rules, and
// where the agents come from. Each skill becomes an agent in one click: "use" is
// the lens you read before publishing, "⇧ publishes" goes straight to the PR.
function renderSkillList() {
  const box = $('#skill-list');
  box.innerHTML = '';
  $('#skills-dir').textContent = state.skillsDir || '';
  if (!state.skills.length) {
    box.append(el('p', 'none', `nothing in ${state.skillsDir || '~/.claude/skills'} — install a skill there to build an agent out of it.`));
    return;
  }
  // The ones that already became agents come first: they are what matters here.
  const usadas = new Set();
  for (const a of state.agents) for (const sk of a.skills || []) usadas.add(sk.name);
  // Only the agents in the list count for "already there": the publishing one
  // isn't a picker choice, and it is what the server also ignores when adding.
  const nomes = new Set(state.agents.filter((a) => !a.publisher).map((a) => a.name));
  const ordenadas = [...state.skills].sort((x, y) => (usadas.has(y.name) ? 1 : 0) - (usadas.has(x.name) ? 1 : 0));

  for (const sk of ordenadas) {
    const row = tid(el('div', 'skill-row' + (usadas.has(sk.name) ? ' on' : '')), 'skill-row', { skill: sk.name });
    row.append(el('span', 'skill-name', '/' + sk.name));
    if (sk.builtin) {
      // This one isn't on disk: it came in the binary and is written into the
      // clone before running. Saying so here is what explains why it shows up on
      // a machine where nobody installed any skill.
      const t = el('span', 'badge', 'built in');
      t.title = 'ships inside the Bazel binary — nothing to install, and it works on any machine';
      row.append(t);
    }
    const desc = el('span', 'skill-desc', sk.description || '');
    desc.title = sk.description || '';
    row.append(desc);

    const acoes = el('span', 'skill-actions');
    if (sk.name === state.postSkill) {
      // The publishing one doesn't become an agent: alone it runs without the
      // review and posts nothing. It comes in via the publish button and the publish step.
      const t = el('span', 'badge', 'runs as publish');
      t.title = 'publishes a review you have read — from the publish button or the publish step of a pipeline';
      acoes.append(t);
      row.append(acoes);
      box.append(row);
      continue;
    }
    const add = (rotulo, posts, dica) => {
      const nome = posts ? sk.name + '-post' : sk.name;
      const b = tid(el('button', 'btn small', rotulo), posts ? 'skill-publishes' : 'skill-use');
      if (nomes.has(nome)) {
        // Already an agent: the design says "In use" in the Use slot.
        if (!posts) b.textContent = 'In use';
        b.disabled = true;
        b.title = `${nome} is already in the agent list`;
      } else {
        b.title = dica;
        b.addEventListener('click', () => agentAction('/api/agents', 'POST', b, { skill: sk.name, posts }));
      }
      acoes.append(b);
    };
    add('Use', false, `becomes the agent ${sk.name}: runs /${sk.name} on the PR and hands the review back for you to read`);
    add('+ publishes', true, `becomes the agent ${sk.name}-post: runs /${sk.name} --post and publishes the review straight to the PR`);
    row.append(acoes);

    box.append(row);
  }
}

async function loadSkills() {
  try {
    const data = await api('/api/skills');
    state.skills = data.skills || [];
    state.skillsDir = data.dir || '';
    state.postSkill = data.post_skill || '';
  } catch (err) {
    state.skills = [];
  }
}

// --- the two pages ---
//
// Navigation is by hash and reloads nothing: the SSE stays open, the jobs stay
// in memory and the review you were reading is there when you come back.
// Reloading the tab at #/config opens straight into the config, which is what
// you expect from a real page.
function pageFromHash() {
  return location.hash.replace(/^#\/?/, '') === 'config' ? 'config' : 'dashboard';
}

async function showPage(nome) {
  $('#page-dashboard').hidden = nome !== 'dashboard';
  $('#page-config').hidden = nome !== 'config';
  for (const a of document.querySelectorAll('#nav a')) {
    a.classList.toggle('on', a.dataset.page === nome);
  }
  document.title = nome === 'config' ? 'Bazel · config' : 'Bazel';
  // The config is read from disk on every visit: it changes from outside —
  // another tab, an editor — and showing what was in memory would be lying.
  if (nome === 'config') await refreshConfig();
}

function route() {
  return showPage(pageFromHash());
}

// refreshConfig redraws the whole config from disk. It is called on opening
// and after every change to the agent list — the yaml on screen is the file,
// and it has just changed.
async function refreshConfig() {
  try {
    const cfg = await api('/api/config');
    $('#config-path').textContent = cfg.path;
    $('#config-yaml').textContent = cfg.yaml;
    renderStray(cfg.stray_command);
  } catch (err) {
    $('#config-yaml').textContent = err.message;
    $('#config-raw').open = true;
  }
  renderRepos();
  await loadSkills();
  renderAgentList();
  renderPipelineBuilder();
  renderSkillList();
}

// renderStray warns, on the page where it gets fixed, that the `agent` template
// fires a command on its own. It is the most misleading error: the agent runs,
// the report comes out, and only the token count reveals that two commands ran.
function renderStray(cmd) {
  const box = $('#stray');
  box.hidden = !cmd;
  if (!cmd) return;
  box.innerHTML = '';
  box.append(el('b', null, `agent.prompt runs ${cmd} on its own.`));
  box.append(document.createTextNode(' It has no '));
  box.append(el('code', null, '{{task}}'), document.createTextNode(', so Bazel prepends the agent'
    + ` you picked and the template still fires ${cmd} below it — every run does both. Put `));
  box.append(el('code', null, '{{task}}'));
  box.append(document.createTextNode(` where ${cmd} is, in the file below.`));
}

function renderRepos() {
  const box = $('#repo-list');
  box.innerHTML = '';
  if (!state.repos.length) {
    box.append(el('p', 'none', 'none yet — Bazel has nowhere to look for PRs.'));
    return;
  }
  for (const repo of state.repos) {
    const row = tid(el('div', 'repo-row'), 'repo-row', { repo });
    // "N open": the PRs of this repository in the list right now.
    const open = state.prs.filter((pr) => repoKey(pr.repo) === repoKey(repo)).length;
    row.append(el('span', 'repo-name', repo), el('span', 'repo-open dim', `${open} open`));
    const rm = tid(el('button', 'btn small ghost', 'Remove'), 'repo-remove');
    rm.addEventListener('click', () => removeRepo(repo, rm));
    row.append(rm);
    box.append(row);
  }
}

async function addRepo(ev) {
  ev.preventDefault();
  const input = $('#repo-input');
  const repo = input.value.trim();
  if (!repo) return;
  try {
    const res = await api('/api/repos', { method: 'POST', body: JSON.stringify({ repo }) });
    state.repos = res.repos;
    input.value = '';
    banner('');
    renderRepos();
    loadPRs(true);
  } catch (err) {
    banner(err.message);
  }
}

async function removeRepo(repo, btn) {
  btn.disabled = true;
  try {
    const res = await api('/api/repos/' + repo.split('/').map(encodeURIComponent).join('/'), { method: 'DELETE' });
    state.repos = res.repos;
    renderRepos();
    loadPRs(true);
  } catch (err) {
    banner(err.message);
    btn.disabled = false;
  }
}

boot();
