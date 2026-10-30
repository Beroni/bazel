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

  const head = el('div', 'viewer-head');
  const grow = el('div', 'grow');
  const h = el('h2');
  h.append(el('span', 'num', '#' + pr.number), document.createTextNode(' ' + pr.title));

  const sub = el('div', 'meta');
  const link = el('a', null, pr.key);
  link.href = pr.url;
  link.target = '_blank';
  link.rel = 'noreferrer';
  sub.append(
    link,
    el('span', null, '@' + pr.author),
    el('span', null, `${pr.branch} → ${pr.base}`),
    el('span', 'add', '+' + pr.additions),
    el('span', 'del', '−' + pr.deletions),
    el('span', null, `${pr.changed_files} files`),
    el('span', null, pr.age),
  );
  if (pr.draft) sub.append(el('span', 'tag draft', 'draft'));
  if (pr.review_decision === 'APPROVED') sub.append(el('span', 'tag approved', 'approved'));
  if (pr.review_decision === 'CHANGES_REQUESTED') sub.append(el('span', 'tag changes', 'changes'));
  for (const t of reviewTags(pr)) sub.append(t);
  grow.append(h, sub);

  head.append(grow);
  v.append(head);

  if (pr.changed_since_review) {
    const aviso = el('div', 'stale-box');
    aviso.textContent = `This PR changed after the review from ${ago(pr.reviewed_at)}`
      + (pr.review_agent ? ` (${pr.review_agent})` : '')
      + ' — what is here is no longer what the agent read.';
    v.append(aviso);
  }

  if (pr.body_html) {
    const md = el('div', 'md');
    md.innerHTML = pr.body_html;
    v.append(md);
  } else {
    v.append(el('p', 'dim', '(no description)'));
  }

  const done = state.jobs.filter((j) => j.pr.key === pr.key);
  if (done.length) {
    const back = el('p', 'dim');
    back.style.marginTop = '28px';
    back.append(document.createTextNode('reviews in this session: '));
    for (const j of done) {
      const a = el('a', null, `${j.id} (${label(j)})`);
      a.href = '#';
      a.style.marginRight = '10px';
      a.addEventListener('click', (e) => { e.preventDefault(); openJob(j.id); });
      back.append(a);
    }
    v.append(back);
  }
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
    const total = decorateFindings(md, key);

    // A saved review stays publishable: closing Bazel before sending it to the
    // PR can't cost the agent's work.
    if (entry && entry.repo && entry.publishable === false) {
      const nota = el('p', 'dim');
      nota.style.marginTop = '28px';
      nota.textContent = `This is not a review — what "${entry.agent}" produces stays in Bazel.`;
      v.append(nota);
    } else if (entry && entry.repo) {
      const bar = el('div', 'viewer-actions');
      const pub = tid(el('button', 'btn solid', 'Publish inline review'), 'saved-publish');
      pub.title = `runs the post skill and publishes to ${entry.repo}#${entry.number} with inline comments`;
      pub.addEventListener('click', () => publishSaved(name, pub));
      const cm = tid(el('button', 'btn ghost', 'or paste as a comment'), 'saved-comment');
      cm.title = `pastes this review as a single comment on ${entry.repo}#${entry.number}`;
      cm.addEventListener('click', () => commentSaved(name, cm));
      bar.append(pub, cm);
      if (total) bar.append(pickSummary(key, total));
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
  if (!skip.length) return '';
  return `\n\n${skip.length} unticked finding${skip.length === 1 ? '' : 's'} will be left out.`;
}

// decorateFindings puts the checkbox on each finding of the review drawn in md
// and returns how many findings there are.
// findingTitle is a short " (title)" for a finding's accessible name: its ###
// heading, or the bold line that opens it, trimmed to about 60 characters.
function findingTitle(sec) {
  const h = sec.querySelector('h1, h2, h3, h4, strong');
  let t = h ? h.textContent.replace(/\s+/g, ' ').trim().replace(/^\d+[.)]\s*/, '') : '';
  if (t.length > 60) t = t.slice(0, 59).trimEnd() + '…';
  return t ? ` (${t})` : '';
}

function decorateFindings(md, key) {
  const secs = md.querySelectorAll('section.finding');
  const out = skipped(key);
  secs.forEach((sec) => {
    const idx = Number(sec.dataset.finding);
    const label = el('label', 'finding-pick');
    const box = document.createElement('input');
    box.type = 'checkbox';
    box.checked = !out.has(idx);
    tid(box, 'finding-pick', { findingIndex: idx });
    // Every box shows the same word, "report"; the accessible name says which
    // finding it is, so a screen reader can tell them apart.
    box.setAttribute('aria-label', `include finding ${idx + 1}${findingTitle(sec)} in what goes to the PR`);
    box.title = 'untick to leave this finding out of what goes to the PR';
    label.append(box, el('span', null, 'report'));
    sec.classList.toggle('skipped', out.has(idx));
    box.addEventListener('change', () => {
      if (box.checked) out.delete(idx); else out.add(idx);
      sec.classList.toggle('skipped', !box.checked);
      refreshPickSummary();
    });
    sec.prepend(label);
  });
  return secs.length;
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
  if (!out) txt.textContent = `${total} finding${total === 1 ? '' : 's'} · all ticked`;
  else if (!kept) txt.textContent = `all ${total} findings unticked — only the rest of the review goes out`;
  else txt.textContent = `${kept} of ${total} findings ticked · ${out} left out`;
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
  return (state.logFilter || {})[id] || '';
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
  col.innerHTML = '';
  const pick = (who) => {
    state.logFilter = state.logFilter || {};
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

// total is what the server says it has — the log of a finished review is only
// fetched when someone opens it, and until then "0 lines" would be a lie.
function logCountLabel(id, total) {
  const st = logState(id);
  const n = Math.max(st.next, total || 0);
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

function renderStats() {
  syncSelectAll();
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

    // Title first, the way the eye scans a list; under it the repository and
    // number with the state tags on the right, and the size of the change.
    const main = el('div', 'pr-main');
    const sub = el('div', 'pr-sub');
    sub.append(el('span', 'repo', `${pr.slug} #${pr.number}`));
    const tags = el('span', 'pr-tags');
    if (pr.draft) tags.append(el('span', 'tag draft', 'draft'));
    if (pr.mine) tags.append(el('span', 'tag mine', 'you'));
    if (pr.review_decision === 'APPROVED') tags.append(el('span', 'tag approved', 'approved'));
    if (pr.review_decision === 'CHANGES_REQUESTED') tags.append(el('span', 'tag changes', 'changes'));
    for (const t of reviewTags(pr)) tags.append(t);
    sub.append(tags);

    const meta = el('div', 'meta');
    meta.append(
      el('span', null, '@' + pr.author),
      el('span', 'add', '+' + pr.additions),
      el('span', 'del', '−' + pr.deletions),
      el('span', null, `${pr.changed_files} files`),
      el('span', null, pr.age),
    );

    // The title is the keyboard way in: the row's click opens the PR, and the
    // checkbox next to it stays its own control.
    const title = operable(el('span', 'title', pr.title), `open ${pr.key}: ${pr.title}`, () => openPR(pr.key));
    title.title = pr.title;
    main.append(title, sub, meta);
    row.append(box, main);
    list.append(row);
  }
  renderStats();
  updateReviewButton();
}

// ago turns "2h" into "2h ago"; "just now" is already a phrase and would get
// a crooked "ago".
const ago = (age) => (age === 'just now' ? age : age + ' ago');

// reviewTags is the PR's history in the list: whether it was reviewed, when, and
// — what matters most — whether it got a new commit after that. A reviewed PR
// that changed is worth a warning, not a ✓: what is on GitHub is no longer what
// the agent read.
function reviewTags(pr) {
  if (!pr.reviewed) return [];
  if (pr.changed_since_review) {
    const t = tid(el('span', 'tag stale', '⟳ changed'), 'pr-review-tag', { state: 'changed' });
    // A stale review that already went to the PR is the case worth flagging.
    t.title = `reviewed ${ago(pr.reviewed_at)} by ${pr.review_agent || 'an agent'}`
      + (pr.review_posted ? ', published on the PR' : '')
      + ', and the PR got new commits afterwards';
    return [t];
  }
  const t = tid(el('span', 'tag reviewed', '✓ ' + pr.reviewed_at + (pr.review_posted ? ' ↑' : '')),
    'pr-review-tag', { state: pr.review_posted ? 'published' : 'reviewed' });
  t.title = `reviewed ${ago(pr.reviewed_at)} by ${pr.review_agent || 'an agent'}`
    + (pr.review_posted ? ', and published on the PR' : '');
  return [t];
}

// selectVisible is "select all": it ticks, or unticks, only the PRs the filter
// shows. Ticks on PRs a filter is hiding stay as they are — filtering never
// unticks anything, and neither does a box that only sees the visible list.
function selectVisible(on) {
  for (const pr of visiblePRs()) {
    if (on) state.selected.add(pr.key); else state.selected.delete(pr.key);
  }
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
    const title = el('div', 'title', job.pr.title);
    title.title = job.pr.title;
    card.append(top, title);

    const meta = el('div', 'card-meta');
    meta.append(tid(fillState(el('span', 'state ' + job.state), job), 'job-state'));
    if (job.agent) meta.append(el('span', 'card-agent', job.agent + (job.pipeline ? ' ⛓' : '')));
    if (job.publishing) meta.append(el('span', 'tag posts', '⇧ publishing'));
    else if (job.posts) meta.append(el('span', 'tag posts', '⇧ publishes'));
    else if (job.posted) meta.append(tid(el('span', 'tag reviewed', '✓ published'), 'job-published'));
    const custo = gasto(job);
    if (custo) {
      const g = tid(el('span', 'tokens', custo), 'job-tokens', { partial: !!job.tokens_partial });
      g.title = gastoTitle(job);
      meta.append(g);
    }
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
// uppercase. It doesn't clear the element: the card's colored dot is already there.
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
    jobView(job), jobView(job) === 'history' ? state.saved.length : '',
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
  const label_ = document.querySelector('#viewer .job-head .state');
  if (label_) {
    label_.textContent = '';
    fillState(label_, job);
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

// jobHead names the PR (repo#n · author), its title, the state and the one
// action that makes sense while the job is alive: cancel.
function jobHead(job) {
  const head = el('div', 'job-head');
  const grow = el('div', 'grow');
  const kicker = el('div', 'kicker');
  const link = el('a', null, job.pr.key);
  link.href = job.pr.url;
  link.target = '_blank';
  link.rel = 'noreferrer';
  kicker.append(link, document.createTextNode(' · ' + job.pr.author));
  grow.append(kicker, el('h2', null, job.pr.title));
  head.append(grow, fillState(el('span', 'state state-pill ' + job.state), job));
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
      if (key === 'history') refreshSavedList();
      renderViewer();
    });
    bar.append(b);
  }
  return bar;
}

// renderOverview is everything about the job that is not the report or the
// log: where it runs, with what, its steps, what it cost and where it landed.
function renderOverview(body, job) {
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
  const link = el('a', null, job.pr.key);
  link.href = job.pr.url;
  link.target = '_blank';
  link.rel = 'noreferrer';
  fact('PR', link);
  fact('Author', '@' + job.pr.author);
  fact('Branch', `${job.pr.branch} → ${job.pr.base}`);
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

// renderHistoryView lists the other reviews of the same PR: the ones in this
// session's queue and the ones saved on disk. Nothing new on the server — it is
// the queue and the Saved list, narrowed to this PR.
function renderHistoryView(body, job) {
  const mesmo = (repo, n) => repo && repo.toLowerCase() === job.pr.repo.toLowerCase() && n === job.pr.number;
  const sessao = state.jobs.filter((j) => j.id !== job.id && mesmo(j.pr.repo, j.pr.number));
  const salvos = state.saved.filter((s) => mesmo(s.repo, s.number));

  body.append(el('p', 'section-label', 'this session'));
  if (!sessao.length) body.append(el('p', 'dim', 'No other review of this PR in the queue.'));
  for (const j of sessao) {
    const row = el('button', 'history-row');
    row.append(fillState(el('span', 'state ' + j.state), j), el('span', null, j.agent || 'agent'), el('span', 'dim', j.id));
    row.addEventListener('click', () => openJob(j.id));
    body.append(row);
  }
  body.append(el('p', 'section-label', 'saved on disk'));
  if (!salvos.length) body.append(el('p', 'dim', 'No saved review of this PR yet.'));
  for (const s of salvos) {
    const row = el('button', 'history-row');
    row.append(el('span', null, s.agent || 'review'), el('span', 'dim', new Date(s.mod_time).toLocaleString()), el('span', 'dim', s.name));
    row.addEventListener('click', () => {
      state.tab = 'saved';
      state.activeSaved = s.name;
      renderTabs();
      openSaved(s.name);
    });
    body.append(row);
  }
}

// refreshSavedList fetches the saved reviews without touching the Saved tab's
// screen — the History tab only needs the list.
async function refreshSavedList() {
  try {
    const data = await api('/api/reviews');
    state.saved = data.reviews || [];
    if (state.tab === 'queue') renderViewer();
  } catch {
    // The history is a convenience: without the list it shows the session only.
  }
}

function renderReportView(body, job) {
  const v = body;
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
    total = decorateFindings(md, key);
  }

  // The actions come after the review, not before: this is where you arrive
  // when you're done reading — the moment to decide whether this goes to the PR.
  if (!job.publishing && job.publishable === false) {
    // An agent that produces no review has nowhere to publish. Saying so here
    // beats showing nothing: you have just read the report and the next
    // question is always "so, does this go to the PR?".
    v.append(el('p', 'dim end-note', `This is not a review — what "${job.agent}" produces stays in Bazel.`
      + ' Run a reviewing agent over this PR when you want something to publish.'));
  } else if (!job.publishing) {
    const bar = el('div', 'viewer-actions');
    const pub = tid(el('button', 'btn solid', 'Publish inline review'), 'publish-inline');
    pub.title = 'runs the post skill and publishes to the PR with inline comments';
    pub.addEventListener('click', () => publishJob(job.id, pub));
    bar.append(pub);
    if (waiting(job)) {
      const keep = tid(el('button', 'btn', 'Keep it here'), 'keep-here');
      keep.title = 'nothing goes to the PR; the review stays in Bazel, and in the Saved tab';
      keep.addEventListener('click', () => dismissJob(job.id, keep));
      bar.append(keep);
    }
    if (job.posted) {
      bar.append(tid(el('span', 'state done', '✓ already on the PR'), 'already-published'));
    } else {
      const cm = tid(el('button', 'btn ghost', 'or paste as a comment'), 'paste-comment');
      cm.addEventListener('click', () => postJob(job.id, cm));
      bar.append(cm);
    }
    if (total) bar.append(pickSummary(key, total));
    v.append(bar);
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
    if (a.pipeline) top.append(el('span', 'tag', 'pipeline'));
    if (a.posts) top.append(el('span', 'tag posts', '⇧ publishes'));
    if (a.publishable === false) top.append(el('span', 'tag', 'not a review'));
    if (a.publisher) top.append(el('span', 'tag', 'used when publishing'));
    else if (defaultAgent() === a.name) top.append(el('span', 'tag mine', 'default'));
    // What the dashboard picker has on now, when it is not the default: it says
    // what runs next from this browser, while the file still says the default.
    if (!a.publisher && state.agent === a.name && defaultAgent() !== a.name) {
      const t = el('span', 'tag', 'picked on the dashboard');
      t.title = 'selected in the dashboard right now; config.yaml still has the default above';
      top.append(t);
    }

    // The publishing agent isn't a choice in the list: it is what runs when you
    // send a review you've already read to be published, and it can't be taken off.
    if (!a.publisher) {
      top.append(el('div', 'spacer'));
      if (defaultAgent() !== a.name) {
        const d = tid(el('button', 'btn small ghost', 'make default'), 'agent-default');
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
      const rm = tid(el('button', 'btn small ghost', 'remove'), a.pipeline ? 'pipeline-remove' : 'agent-remove');
      rm.addEventListener('click', () => agentAction(base + encodeURIComponent(a.name), 'DELETE', rm));
      top.append(rm);
    }
    row.append(top);

    if (a.description) row.append(el('div', 'agent-desc', a.description));
    if (a.pipeline) row.append(el('div', 'agent-desc', (a.steps || []).join(' → ')));

    const usa = el('div', 'agent-skills');
    for (const sk of a.skills || []) {
      const t = el('span', 'skill-ref ' + (sk.installed ? 'ok' : 'missing'),
        (sk.installed ? '✓ /' : '✗ /') + sk.name);
      t.title = sk.installed
        ? 'installed in ' + state.skillsDir
        : `this skill is not in ${state.skillsDir} — the agent will fail at run time`;
      usa.append(t);
    }
    if (!(a.skills || []).length) usa.append(el('span', 'dim', 'calls no skill'));
    row.append(usa);

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

  const criar = tid(el('button', 'btn primary', 'create pipeline'), 'pipeline-create');
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
  $('#skills-dir').textContent = state.skillsDir ? '· ' + state.skillsDir : '';
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
      const t = el('span', 'tag', 'in Bazel');
      t.title = 'ships inside the Bazel binary — nothing to install, and it works on any machine';
      row.append(t);
    }
    row.append(el('span', 'skill-desc', sk.description || ''));

    const acoes = el('span', 'skill-actions');
    if (sk.name === state.postSkill) {
      // The publishing one doesn't become an agent: alone it runs without the
      // review and posts nothing. It comes in via the publish button and the publish step.
      const t = el('span', 'tag', 'runs as publish');
      t.title = 'publishes a review you have read — from the publish button or the publish step of a pipeline';
      acoes.append(t);
      row.append(acoes);
      box.append(row);
      continue;
    }
    const add = (rotulo, posts, dica) => {
      const nome = posts ? sk.name + '-post' : sk.name;
      const b = tid(el('button', 'btn small' + (posts ? ' ghost' : ''), rotulo), posts ? 'skill-publishes' : 'skill-use');
      if (nomes.has(nome)) {
        b.disabled = true;
        b.title = `${nome} is already in the agent list`;
      } else {
        b.title = dica;
        b.addEventListener('click', () => agentAction('/api/agents', 'POST', b, { skill: sk.name, posts }));
      }
      acoes.append(b);
    };
    add('use', false, `becomes the agent ${sk.name}: runs /${sk.name} on the PR and hands the review back for you to read`);
    add('⇧ publishes', true, `becomes the agent ${sk.name}-post: runs /${sk.name} --post and publishes the review straight to the PR`);
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
    row.append(el('span', null, repo));
    const rm = tid(el('button', 'btn small ghost', 'remove'), 'repo-remove');
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
