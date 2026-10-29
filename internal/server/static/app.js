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
  return v;
};
const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
};

async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...opts,
  });
  const body = await res.json().catch(() => ({}));
  if (!res.ok) {
    const err = new Error(body.error || `${res.status} ${res.statusText}`);
    err.status = res.status;
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

function banner(msg) {
  const b = $('#banner');
  if (!msg) { b.hidden = true; return; }
  b.textContent = msg;
  b.hidden = false;
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
  } finally {
    $('#refresh').disabled = false;
  }
}

// --- server events ---

function connect() {
  const src = new EventSource('/api/events');
  src.onopen = () => $('#conn').classList.add('on');
  src.onerror = () => $('#conn').classList.remove('on');
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

// adoptInstance keeps the id of the server process. Job ids restart at j1 on
// every boot, so with a different id everything the page cached by job id
// (review body, split, ticks, log) belongs to another job, or to none. It
// forgets all that and redoes what depended on it.
function adoptInstance(id) {
  if (!id || id === state.instance) return;
  const anterior = state.instance;
  state.instance = id;
  if (anterior === null) return; // first time we hear from the server
  state.bodies = {};
  state.splits = {};
  state.logs = {};
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

  const v = clearViewer();

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
  catch (err) { banner(err.message); }
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
  if (btn) { btn.disabled = true; btn.textContent = 'publishing…'; }
  try {
    const view = await api(`/api/jobs/${encodeURIComponent(id)}/publish`, { method: 'POST', body: JSON.stringify({ skip }) });
    upsertJob(view);
    state.activeJob = view.id;
    state.tab = 'queue';
    renderJobs();
    renderViewer();
  } catch (err) {
    banner(err.status === 409 ? conflictNote(err) : err.message);
    if (err.status === 409) refreshJob(id);
    if (btn) { btn.disabled = false; btn.textContent = 'publish inline review'; }
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
    // The job may have gone with the server; the banner already said what matters.
  }
}

// dismissJob is "keep it here": the review stays in Bazel, the card stops
// waiting, and nothing goes to the PR. The saved review stays in the Saved tab.
async function dismissJob(id, btn) {
  if (btn) btn.disabled = true;
  try {
    upsertJob(await api(`/api/jobs/${encodeURIComponent(id)}/dismiss`, { method: 'POST' }));
  } catch (err) {
    banner(err.status === 409 ? `Could not keep it here: ${err.message}.` : err.message);
    if (err.status === 409) refreshJob(id);
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
  if (btn) { btn.disabled = true; btn.textContent = 'publishing…'; }
  try {
    upsertJob(await api(`/api/jobs/${encodeURIComponent(id)}/post`, { method: 'POST', body: JSON.stringify({ skip }) }));
  } catch (err) {
    banner(err.status === 409 ? conflictNote(err) : 'failed to publish: ' + err.message);
    if (err.status === 409) refreshJob(id);
    if (btn) { btn.disabled = false; btn.textContent = 'publish to the PR'; }
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
    const v = clearViewer();
    const head = el('div', 'viewer-head');
    const grow = el('div', 'grow');
    grow.append(el('h2', null, data.name));
    head.append(grow);
    v.append(head);
    const md = el('div', 'md');
    md.innerHTML = data.html;
    v.append(md);
    const key = pickKey('saved', name);
    const total = decorateFindings(md, key);

    // A saved review stays publishable: closing Bazel before sending it to the
    // PR can't cost the agent's work.
    const entry = state.saved.find((x) => x.name === name);
    if (entry && entry.repo && entry.publishable === false) {
      const nota = el('p', 'dim');
      nota.style.marginTop = '28px';
      nota.textContent = `This is not a review — what "${entry.agent}" produces stays in Bazel.`;
      v.append(nota);
    } else if (entry && entry.repo) {
      const bar = el('div', 'viewer-actions');
      const pub = el('button', 'btn primary', 'publish inline review');
      pub.title = `runs the post skill and publishes to ${entry.repo}#${entry.number} with inline comments`;
      pub.addEventListener('click', () => publishSaved(name, pub));
      const cm = el('button', 'btn ghost', 'or paste as a comment');
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
    banner(err.message);
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
  try {
    await api('/api/reviews/' + encodeURIComponent(name) + '/comment', { method: 'POST', body: JSON.stringify({ skip }) });
    btn.textContent = '✓ commented on ' + alvo;
    loadPRs(false);
  } catch (err) {
    banner('failed to publish: ' + err.message);
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
function decorateFindings(md, key) {
  const secs = md.querySelectorAll('section.finding');
  const out = skipped(key);
  secs.forEach((sec) => {
    const idx = Number(sec.dataset.finding);
    const label = el('label', 'finding-pick');
    const box = document.createElement('input');
    box.type = 'checkbox';
    box.checked = !out.has(idx);
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
  const s = el('span', 'pick-summary');
  s.dataset.key = key;
  s.dataset.total = total;
  const txt = el('span', 'dim');
  const all = el('button', 'btn small ghost', 'all');
  all.title = 'tick every finding';
  const none = el('button', 'btn small ghost', 'none');
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
  const row = el('div', 'step ' + st.state);
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

// logLineEl builds a line with the signature of whoever wrote it. The label's
// color comes from the order of appearance, so the fleet's lenses can be told
// apart at a glance even when interleaved.
// --- one terminal per agent ---
//
// The fleet starts the three lenses inside the same process, in parallel: in a
// single stream the lines arrive interleaved and you can't follow any line of
// reasoning. Each agent writes to its own terminal, with its own scroll.

const maxTermLines = 300;

// agentName is the name that goes in the terminal header. An unsigned line
// belongs to the step's agent — the server already sends that name filled in.
function agentName(l) {
  return l.agent || 'agent';
}

function logLineEl(l) {
  return el('div', 'log-line ' + (l.stream === 'stderr' ? 'stderr' : 'stdout'), l.text);
}

// termFor returns the body of an agent's terminal, creating the terminal on the
// first line it writes. The order on screen is the order in which they spoke.
function termFor(wrap, id, who) {
  if (!wrap._terms) wrap._terms = new Map();
  let body = wrap._terms.get(who);
  if (body) return body;

  const st = logState(id);
  const cor = 'c' + (Math.max(0, st.agents.indexOf(who)) % 6);
  const term = el('div', 'term');
  const head = el('div', 'term-head ' + cor);
  head.append(el('span', 'dot'), el('span', 'term-name', who));
  const count = el('span', 'term-count', '0');
  head.append(count);
  body = el('div', 'term-body');
  term.append(head, body);
  wrap.append(term);

  body._count = count;
  wrap._terms.set(who, body);
  return body;
}

// pushLine writes to the agent's terminal and scrolls if the reader was already at the end.
function pushLine(wrap, id, l) {
  const body = termFor(wrap, id, agentName(l));
  const noFim = body.scrollHeight - body.scrollTop - body.clientHeight < 30;
  body.append(logLineEl(l));
  while (body.childElementCount > maxTermLines) body.firstElementChild.remove();
  if (body._count) body._count.textContent = String(body.childElementCount);
  if (noFim) body.scrollTop = body.scrollHeight;
}

// logTerminals builds the grid with what the page already has on hand.
function logTerminals(id) {
  const wrap = el('div', 'terms');
  wrap.id = 'log-terms';
  wrap.dataset.job = id;
  const st = logState(id);
  for (const l of st.lines) pushLine(wrap, id, l);
  if (!st.lines.length) wrap.append(el('p', 'log-empty', 'waiting for the agent to speak…'));
  return wrap;
}

// appendLog sends each new line to the terminal of whoever wrote it.
function appendLog(id, lines) {
  const wrap = $('#log-terms');
  if (!wrap || wrap.dataset.job !== id) return;
  const vazio = wrap.querySelector('.log-empty');
  if (vazio) vazio.remove();
  for (const l of lines) pushLine(wrap, id, l);
  const count = $('#log-count');
  if (count) count.textContent = logCountLabel(id);
}

// total is what the server says it has — the panel of a finished review only
// fetches the lines when someone opens it, and until then "0 lines" would be a lie.
function logCountLabel(id, total) {
  const st = logState(id);
  const n = Math.max(st.next, total || 0);
  const agentes = st.agents.length;
  return `${n} line${n === 1 ? '' : 's'}`
    + (agentes > 1 ? ` · ${agentes} agents` : '')
    + (st.dropped ? ` · ${st.dropped} older dropped` : '');
}

function logPanel(job, open) {
  const wrap = el('details', 'log-panel');
  wrap.open = open;
  const sum = el('summary');
  const count = el('span', 'dim', logCountLabel(job.id, job.log_lines));
  count.id = 'log-count';
  sum.append(el('span', null, 'agent log'), document.createTextNode(' · '), count);

  wrap.append(sum, logTerminals(job.id));
  // The log of an already finished review is only fetched when someone opens it.
  wrap.addEventListener('toggle', () => { if (wrap.open) pollLog(job.id); });
  return wrap;
}

function renderStats() {
  const total = state.prs.length;
  const shown = visiblePRs().length;
  const marked = markedPRs().length;
  // With a filter on, the number that matters is the slice's — but the total
  // stays in view, otherwise the hint that a filter is hiding PRs disappears.
  const conta = shown === total
    ? `${total} PR${total === 1 ? '' : 's'}`
    : `${shown} of ${total} PRs`;
  $('#stats').textContent = conta + (marked ? ` · ${marked} ticked` : '');
}

function renderPRs() {
  const list = $('#pr-list');
  const prs = visiblePRs();
  list.innerHTML = '';
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
    box.addEventListener('click', (e) => e.stopPropagation());
    box.addEventListener('change', () => toggle(pr.key, box.checked));
    // The row opens, the checkbox ticks: whoever clicks the PR wants to read it,
    // and ticking for review has a target of its own.
    row.addEventListener('click', () => openPR(pr.key));

    const main = el('div', 'pr-main');
    const top = el('div', 'pr-top');
    top.append(el('span', 'num', '#' + pr.number), el('span', 'repo', pr.slug));
    if (pr.draft) top.append(el('span', 'tag draft', 'draft'));
    if (pr.mine) top.append(el('span', 'tag mine', 'you'));
    if (pr.review_decision === 'APPROVED') top.append(el('span', 'tag approved', 'approved'));
    if (pr.review_decision === 'CHANGES_REQUESTED') top.append(el('span', 'tag changes', 'changes'));
    for (const t of reviewTags(pr)) top.append(t);

    const meta = el('div', 'meta');
    meta.append(
      el('span', null, '@' + pr.author),
      el('span', 'add', '+' + pr.additions),
      el('span', 'del', '−' + pr.deletions),
      el('span', null, `${pr.changed_files} files`),
      el('span', null, pr.age),
    );

    main.append(top, el('span', 'title', pr.title), meta);
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
    const t = el('span', 'tag stale', '⟳ changed');
    // A stale review that already went to the PR is the case worth flagging.
    t.title = `reviewed ${ago(pr.reviewed_at)} by ${pr.review_agent || 'an agent'}`
      + (pr.review_posted ? ', published on the PR' : '')
      + ', and the PR got new commits afterwards';
    return [t];
  }
  const t = el('span', 'tag reviewed', '✓ ' + pr.reviewed_at + (pr.review_posted ? ' ↑' : ''));
  t.title = `reviewed ${ago(pr.reviewed_at)} by ${pr.review_agent || 'an agent'}`
    + (pr.review_posted ? ', and published on the PR' : '');
  return [t];
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
  btn.textContent = n > 1 ? `run ${n}` : 'run';
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
  document.querySelectorAll('.tab').forEach((t) => t.classList.toggle('on', t.dataset.tab === state.tab));
  if (state.tab === 'saved') loadSaved(); else renderJobs();
}

// renderQuota shows the Claude quota — the same windows as `/usage`: five
// hours and seven days. It isn't queried: it rides along on the stream of
// whoever is running, so what is on screen is the last reading, and the title
// says when it is from.
function renderQuota() {
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
  rail.innerHTML = '';
  for (const job of state.jobs) {
    const card = el('div', 'card' + (state.activeJob === job.id ? ' on' : ''));
    card.addEventListener('click', () => openJob(job.id));

    const top = el('div', 'card-top');
    const st = el('span', 'state ' + job.state);
    st.append(el('span', 'dot'), document.createTextNode(' '));
    fillState(st, job);
    top.append(el('span', 'num', '#' + job.pr.number), el('span', 'repo', job.pr.slug));
    const sp = el('div', 'spacer'); sp.style.flex = '1';
    const x = el('button', 'card-x', '✕');
    x.title = 'drop from the queue';
    x.addEventListener('click', (e) => { e.stopPropagation(); removeJob(job.id, x); });
    top.append(sp, st, x);
    card.append(top, el('div', 'title', job.pr.title));
    if (job.agent) {
      const who = el('div', 'meta');
      who.append(el('span', null, job.agent + (job.pipeline ? ' ⛓' : '')));
      if (job.publishing) who.append(el('span', 'tag posts', '⇧ publishing'));
      else if (job.posts) who.append(el('span', 'tag posts', '⇧ publishes'));
      const custo = gasto(job);
      if (custo) {
        const g = el('span', 'tokens', custo);
        g.title = gastoTitle(job);
        who.append(g);
      }
      card.append(who);
    }
    if (job.cloning || (job.steps && job.steps.length > 1)) card.append(stepsBox(job));

    const actions = el('div', 'card-actions');
    if (job.state === 'queued' || job.state === 'running') {
      const b = el('button', 'btn small ghost', 'cancel');
      b.addEventListener('click', (e) => { e.stopPropagation(); cancelJob(job.id); });
      actions.append(b);
    }
    if (job.state === 'done' && !job.publishing && job.publishable === false) {
      const t = el('span', 'dim', 'stays here');
      t.title = `what "${job.agent}" produces does not go to the PR`;
      actions.append(t);
    } else if (waiting(job)) {
      // It ended on a `publish`: publishing from here would send everything
      // without you seeing what goes. The card only points to the review,
      // where the ticks are.
      const r = el('button', 'btn small', 'choose what goes');
      r.title = 'opens the review with what goes to the PR, finding by finding';
      r.addEventListener('click', (e) => { e.stopPropagation(); openJob(job.id); });
      actions.append(r);
      const k = el('button', 'btn small ghost', 'keep it here');
      k.title = 'nothing goes to the PR; the review stays in Bazel, and in the Saved tab';
      k.addEventListener('click', (e) => { e.stopPropagation(); dismissJob(job.id, k); });
      actions.append(k);
    } else if (job.state === 'done' && !job.publishing) {
      if (!job.posted) {
        const p = el('button', 'btn small', 'publish inline review');
        p.title = 'runs the post skill and publishes with inline comments';
        p.addEventListener('click', (e) => { e.stopPropagation(); publishJob(job.id, p); });
        actions.append(p);
      }
      if (job.posted) {
        actions.append(el('span', 'state done', '✓ published'));
      } else {
        const b = el('button', 'btn small ghost', 'comment');
        b.title = 'pastes the review as a single comment, no agent';
        b.addEventListener('click', (e) => { e.stopPropagation(); postJob(job.id, b); });
        actions.append(b);
      }
    }
    if (actions.childNodes.length) card.append(actions);
    rail.append(card);
  }
  rail.scrollLeft = scroll;
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
    clearViewer().append(el('p', 'empty', 'no saved reviews yet'));
    return;
  }
  if (!state.activeSaved) {
    const v = clearViewer();
    const list = el('div');
    for (const s of state.saved) {
      const row = el('div', 'saved');
      row.append(el('div', null, s.title || s.name), el('div', 'name', `${s.name} · ${new Date(s.mod_time).toLocaleString()}`));
      row.addEventListener('click', () => openSaved(s.name));
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
  const label_ = document.querySelector('#viewer .viewer-head .state');
  if (label_) {
    label_.textContent = '';
    fillState(label_, job);
  }
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
  const head = el('div', 'viewer-head');
  const grow = el('div', 'grow');
  const h = el('h2');
  h.append(el('span', 'num', '#' + job.pr.number), document.createTextNode(' ' + job.pr.title));
  const sub = el('div', 'meta');
  const link = el('a', null, job.pr.key);
  link.href = job.pr.url;
  link.target = '_blank';
  link.rel = 'noreferrer';
  sub.append(link, el('span', null, '@' + job.pr.author), el('span', null, `${job.pr.branch} → ${job.pr.base}`), fillState(el('span', 'state ' + job.state), job));
  if (job.agent) sub.append(el('span', null, job.agent + (job.pipeline ? ' ⛓' : '')));
  if (job.posts) sub.append(el('span', 'tag posts', '⇧ publishes to the PR'));
  grow.append(h, sub);
  head.append(grow);
  v.append(head);

  if (job.state === 'queued') {
    v.append(el('p', 'dim', 'queued — starts as soon as a worker frees up.'));
    if (job.steps && job.steps.length) v.append(stepsBox(job));
    return;
  }
  if (job.state === 'running') {
    v.append(el('p', 'dim', job.publishing
      ? 'publishing the review you read — cloning the PR and commenting line by line. The review comes back to this card when it is done.'
      : 'running inside the PR clone. You can close the tab, the review keeps going.'));
    v.append(stepsBox(job));
    const custo = gasto(job);
    if (custo) {
      const g = el('p', 'dim tokens-foot', custo);
      g.title = gastoTitle(job);
      v.append(g);
    }
    v.append(logPanel(job, true));
    pollLog(job.id);
    return;
  }
  if (job.state === 'canceled') {
    v.append(el('p', 'dim', 'canceled.'));
    return;
  }
  if (job.state === 'failed') {
    v.append(el('div', 'error-box', job.error || 'failed'));
    if (job.log_lines) v.append(logPanel(job, true));
    pollLog(job.id);
    return;
  }

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
      const ctx = el('div', 'md context');
      ctx.innerHTML = split.ctx;
      v.append(ctx);
    }
    v.append(el('p', 'section-label', 'goes to the PR'));
    const pub = el('div', 'md');
    pub.innerHTML = split.pub;
    v.append(pub);
    total = decorateFindings(pub, key);
  } else {
    const md = el('div', 'md');
    md.innerHTML = html;
    v.append(md);
    total = decorateFindings(md, key);
  }

  if (job.steps && job.steps.length > 1) v.append(stepsBox(job));
  if (job.log_lines) v.append(logPanel(job, false));

  // The actions come after the review, not before: this is where you arrive
  // when you're done reading — the moment to decide whether this goes to the PR.
  if (!job.publishing && job.publishable === false) {
    // An agent that produces no review has nowhere to publish. Saying so here
    // beats showing nothing: you have just read the report and the next
    // question is always "so, does this go to the PR?".
    const nota = el('p', 'dim');
    nota.style.marginTop = '28px';
    nota.textContent = `This is not a review — what "${job.agent}" produces stays in Bazel.`
      + ' Run a reviewing agent over this PR when you want something to publish.';
    v.append(nota);
  } else if (!job.publishing) {
    const bar = el('div', 'viewer-actions');
    const pub = el('button', 'btn primary', 'publish inline review');
    pub.title = 'runs the post skill and publishes to the PR with inline comments';
    pub.addEventListener('click', () => publishJob(job.id, pub));
    bar.append(pub);
    if (waiting(job)) {
      const keep = el('button', 'btn ghost', 'keep it here');
      keep.title = 'nothing goes to the PR; the review stays in Bazel, and in the Saved tab';
      keep.addEventListener('click', () => dismissJob(job.id, keep));
      bar.append(keep);
    }
    if (job.posted) {
      bar.append(el('span', 'state done', '✓ already on the PR'));
    } else {
      const cm = el('button', 'btn ghost', 'or paste as a comment');
      cm.addEventListener('click', () => postJob(job.id, cm));
      bar.append(cm);
    }
    if (total) bar.append(pickSummary(key, total));
    v.append(bar);
  }

  const foot = el('p', 'dim');
  foot.style.marginTop = '32px';
  foot.textContent = job.saved_to ? 'saved to ' + job.saved_to : '';
  if (job.workdir) foot.textContent += (foot.textContent ? ' · ' : '') + 'clone kept at ' + job.workdir;
  if (job.truncated) foot.textContent += ' · diff truncated';
  if (job.post_error) v.append(el('div', 'error-box', 'failed to publish: ' + job.post_error));
  v.append(foot);

  // What the review cost, at its end — which is where you arrive after reading.
  const custo = gasto(job);
  if (custo) {
    const linha = el('p', 'dim tokens-foot', custo + ' · ' + jobSeconds(job) + 's');
    linha.title = gastoTitle(job);
    v.append(linha);
  }
  v.scrollTop = 0;
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
  $('#review-filter').addEventListener('change', (e) => {
    state.reviewFilter = e.target.value;
    e.target.classList.toggle('on', state.reviewFilter !== 'all');
    renderPRs();
  });
  $('#toggle-prs').addEventListener('click', () => togglePRs(!state.prsHidden));
  $('#hide-drafts').addEventListener('change', (e) => { state.hideDrafts = e.target.checked; renderPRs(); });
  $('#select-all').addEventListener('change', (e) => {
    state.selected.clear();
    if (e.target.checked) visiblePRs().forEach((pr) => state.selected.add(pr.key));
    renderPRs();
  });
  document.querySelectorAll('#scope button').forEach((b) => {
    b.addEventListener('click', () => {
      state.scope = b.dataset.scope;
      document.querySelectorAll('#scope button').forEach((x) => x.classList.toggle('on', x === b));
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

    const top = el('div', 'agent-top');
    top.append(el('span', 'agent-name', a.name));
    if (a.pipeline) top.append(el('span', 'tag', 'pipeline'));
    if (a.posts) top.append(el('span', 'tag posts', '⇧ publishes'));
    if (a.publishable === false) top.append(el('span', 'tag', 'not a review'));
    if (a.publisher) top.append(el('span', 'tag', 'used when publishing'));
    else if (state.agent === a.name) top.append(el('span', 'tag mine', 'default'));

    // The publishing agent isn't a choice in the list: it is what runs when you
    // send a review you've already read to be published, and it can't be taken off.
    if (!a.publisher) {
      top.append(el('div', 'spacer'));
      if (state.agent !== a.name) {
        const d = el('button', 'btn small ghost', 'make default');
        d.title = 'becomes the agent the selector starts on';
        d.addEventListener('click', () => agentAction('/api/agents/' + encodeURIComponent(a.name) + '/default', 'POST', d));
        top.append(d);
      }
      // The publishing tick. An agent that publishes on its own doesn't show here:
      // it writes on the PR by itself, and turning off a button wouldn't
      // stop it from anything — the way is to take it off the list.
      if (!a.posts && !a.pipeline) {
        const pb = el('button', 'btn small ghost',
          a.publishable === false ? 'output stays here' : '⇧ output can go to the PR');
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
      const rm = el('button', 'btn small ghost', 'remove');
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
  const fluxo = el('div', 'pipe-flow');
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
  const card = el('div', 'pipe-node' + (ehReservado(nome) ? ' reserved' : '')
    + (arrasto && arrasto.from === i ? ' dragging' : ''));
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
  const box = el('div', 'pipe-tray');
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
  const nome = el('input');
  nome.type = 'text';
  nome.placeholder = 'pipeline name — how it shows up in the selector';
  nome.autocomplete = 'off';
  nome.value = state.pipeName || '';
  nome.addEventListener('input', () => { state.pipeName = nome.value; });

  const criar = el('button', 'btn primary', 'create pipeline');
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
    const row = el('div', 'skill-row' + (usadas.has(sk.name) ? ' on' : ''));
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
      const b = el('button', 'btn small' + (posts ? ' ghost' : ''), rotulo);
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
    const row = el('div', 'repo-row');
    row.append(el('span', null, repo));
    const rm = el('button', 'btn small ghost', 'remove');
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
