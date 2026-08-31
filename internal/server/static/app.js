// Interface do Bazel. Sem framework e sem CDN: o servidor é local e a página
// precisa abrir offline, então tudo que ela usa está embutido no binário.
'use strict';

const state = {
  me: '',
  prs: [],
  repoErrors: [],
  jobs: [],          // mais novo primeiro
  saved: [],
  repos: [],
  agents: [],         // escolhas do seletor: agents e pipelines
  limits: null,       // cota do Claude: sessão (5h) e semana (7d)
  skills: [],         // skills instaladas na máquina
  skillsDir: '',
  agent: '',          // nome do que roda no próximo review
  selected: new Set(),
  scope: 'all',
  filter: '',
  repoFilter: '',     // '' = todos os repositórios
  reviewFilter: 'all', // all | none | reviewed | changed
  hideDrafts: false,
  prsHidden: false,   // lista de PRs recolhida, para ler o review em tela cheia
  tab: 'queue',
  activeJob: null,
  activeSaved: null,
  activePR: null,      // chave do PR aberto no painel da direita
  bodies: {},        // id do job -> html do review
  logs: {},          // id do job -> {next, lines, dropped, live, busy}
};

const $ = (sel) => document.querySelector(sel);

// clearViewer esvazia o painel da direita e esquece a assinatura do que estava
// desenhado nele. Sem esse esquecimento, voltar para a aba da fila caía no
// atalho do renderViewer — "já está desenhado" — e o painel ficava com o que
// a outra aba tinha escrito.
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
  if (!res.ok) throw new Error(body.error || `${res.status} ${res.statusText}`);
  return body;
}

// fmtTokens abrevia a contagem do jeito que o servidor abrevia: um review da
// frota queima milhões de tokens, e "1,8M" se lê melhor que sete dígitos.
function fmtTokens(n) {
  if (!n || n < 0) return '0';
  if (n < 1000) return String(n);
  const [v, suf] = n < 1e6 ? [n / 1000, 'k'] : [n / 1e6, 'M'];
  return v.toFixed(1).replace('.0', '').replace('.', ',') + suf;
}

// gasto é a linha do que o review custou. O ~ marca a conta parcial de um
// review em curso: ela só enxerga a conversa principal do agente, e as lentes
// que ele rodou como sub-agente entram no fechamento. Agente que não fala
// stream-json não reporta nada, e aí não há linha nenhuma.
function gasto(job) {
  const partes = [];
  if (job.tokens) partes.push((job.tokens_partial ? '~' : '') + fmtTokens(job.tokens) + ' tokens');
  if (job.cost_usd) partes.push('$' + job.cost_usd.toFixed(2));
  return partes.join(' · ');
}

// gastoTitle explica o número onde ele aparece.
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

// --- carga ---

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
    state.me = st.me;
    state.jobs = st.jobs || [];
    $('#who').textContent = '@' + st.me;
    $('#who').title = `config: ${st.config_path}\nreviews: ${st.reviews_dir}\nagent: ${st.agent.command} ${(st.agent.args || []).join(' ')}\nconcurrent reviews: ${st.concurrency}`;
    state.limits = st.limits && st.limits.at && !st.limits.at.startsWith('0001') ? st.limits : null;
    renderQuota();
    applyAgents(st.agents || []);
    state.repos = st.repos || [];
    renderRepoFilter();
    if (!state.repos.length) {
      banner('No repositories watched — open "config" at the top and add one (owner/repo).');
    } else if (!selectableAgents().length) {
      banner('No agents configured — open "config" at the top and build your list out of the installed skills.');
    }
    renderJobs();
  } catch (err) {
    banner('Could not reach the server: ' + err.message);
    return;
  }
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
    banner(state.repoErrors.length
      ? state.repoErrors.map((e) => `⚠ ${e.repo}: ${e.error}`).join('\n')
      : '');
    // As contagens do filtro por repositório saem daqui, então ele é montado
    // antes da lista.
    renderRepoFilter();
    renderPRs();
  } catch (err) {
    list.innerHTML = '';
    const p = el('p', 'empty', 'failed: ' + err.message);
    list.append(p);
  } finally {
    $('#refresh').disabled = false;
  }
}

// --- eventos do servidor ---

function connect() {
  const src = new EventSource('/api/events');
  src.onopen = () => $('#conn').classList.add('on');
  src.onerror = () => $('#conn').classList.remove('on');
  src.onmessage = (msg) => {
    let ev;
    try { ev = JSON.parse(msg.data); } catch { return; }
    if (ev.type === 'job') upsertJob(ev.data);
    if (ev.type === 'limits') { state.limits = ev.data; renderQuota(); }
    // Outra aba tirou este job da fila: some daqui também.
    if (ev.type === 'job_gone') dropJob(ev.data.id);
  };
}

function upsertJob(job) {
  const i = state.jobs.findIndex((j) => j.id === job.id);
  if (i >= 0) state.jobs[i] = job;
  else state.jobs.unshift(job);

  // O corpo do review não vem no evento; busca quando ficar pronto.
  if (job.state === 'done' && !state.bodies[job.id]) {
    if (state.activeJob === job.id || (!state.activeJob && !state.activePR)) openJob(job.id);
  }
  renderJobs();
  if (state.activeJob === job.id) renderViewer();

  // Review terminado muda o selo do PR na lista: recarrega do cache do
  // servidor (que não custa chamada ao GitHub) para o ✓ aparecer na hora.
  if ((job.state === 'done' || job.posted) && !state.reloadingPRs) {
    state.reloadingPRs = true;
    loadPRs(false).finally(() => { state.reloadingPRs = false; });
  }
}

// dropJob esquece um job: some da fila e, se era ele que estava aberto, o
// painel da direita volta ao começo.
function dropJob(id) {
  const antes = state.jobs.length;
  state.jobs = state.jobs.filter((j) => j.id !== id);
  if (state.jobs.length === antes) return;
  delete state.bodies[id];
  delete state.logs[id];
  if (state.activeJob === id) {
    state.activeJob = null;
    clearViewer();
  }
  renderJobs();
  if (state.tab === 'queue') renderViewer();
}

// removeJob tira o job da fila no servidor. Um review ainda vivo morre junto,
// e isso vale uma pergunta: é trabalho em andamento indo embora.
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

// --- ações ---

function startReview() {
  return reviewRefs(markedPRs());
}

async function reviewRefs(refs) {
  if (!refs.length) return;
  // Escolher um agente que publica é escrever no PR de outra pessoa: vale uma
  // pergunta antes, que é a mesma cerimônia do botão "publicar no PR".
  const chosen = state.agents.find((a) => a.name === state.agent);
  if (chosen && chosen.posts) {
    const alvo = refs.length > 1 ? `${refs.length} PRs` : refs[0];
    if (!confirm(`The agent "${chosen.name}" publishes the review straight to ${alvo}. Run it anyway?`)) return;
  }
  $('#review').disabled = true;
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

  const act = el('button', 'btn primary', 'review this PR');
  act.addEventListener('click', () => reviewRefs([pr.key]));
  head.append(grow, act);
  v.append(head);

  if (pr.changed_since_review) {
    const aviso = el('div', 'stale-box');
    aviso.textContent = `This PR changed after the review from ${pr.reviewed_at} ago`
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
    if (state.activeJob === id) renderViewer();
  } catch (err) {
    banner(err.message);
  }
}

async function cancelJob(id) {
  try { await api(`/api/jobs/${encodeURIComponent(id)}/cancel`, { method: 'POST' }); }
  catch (err) { banner(err.message); }
}

// publishJob manda o agente publicar o review que está na tela. É o caminho
// "li e aprovei": diferente do postJob, que é o Bazel colando o markdown como
// comentário, aqui roda a skill de post e o review sai com inline.
async function publishJob(id, btn) {
  const job = state.jobs.find((j) => j.id === id);
  if (!job) return;
  if (!confirm(`Publish this review on ${job.pr.key} with inline comments?\n\nThe agent runs again, only to publish what you have just read.`)) return;
  if (btn) { btn.disabled = true; btn.textContent = 'publishing…'; }
  try {
    const view = await api(`/api/jobs/${encodeURIComponent(id)}/publish`, { method: 'POST' });
    upsertJob(view);
    state.activeJob = view.id;
    state.tab = 'queue';
    renderJobs();
    renderViewer();
  } catch (err) {
    banner(err.message);
    if (btn) { btn.disabled = false; btn.textContent = 'publish inline review'; }
  }
}

async function postJob(id, btn) {
  const job = state.jobs.find((j) => j.id === id);
  if (!job) return;
  const aviso = job.posts
    ? `The agent "${job.agent}" already published this review on ${job.pr.key}. Publish it again, as a comment?`
    : `Publish this review as a comment on ${job.pr.key}?`;
  if (!confirm(aviso)) return;
  if (btn) { btn.disabled = true; btn.textContent = 'publishing…'; }
  try {
    upsertJob(await api(`/api/jobs/${encodeURIComponent(id)}/post`, { method: 'POST' }));
  } catch (err) {
    banner('failed to publish: ' + err.message);
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

    // Um review salvo continua publicável: fechar o Bazel antes de mandá-lo
    // ao PR não pode custar o trabalho do agente.
    const entry = state.saved.find((x) => x.name === name);
    if (entry && entry.repo) {
      const bar = el('div', 'viewer-actions');
      const pub = el('button', 'btn primary', 'publish inline review');
      pub.title = `runs the post skill and publishes to ${entry.repo}#${entry.number} with inline comments`;
      pub.addEventListener('click', () => publishSaved(name, pub));
      const cm = el('button', 'btn ghost', 'or paste as a comment');
      cm.title = `pastes this review as a single comment on ${entry.repo}#${entry.number}`;
      cm.addEventListener('click', () => commentSaved(name, cm));
      bar.append(pub, cm);
      v.append(bar);
    }
    v.scrollTop = 0;
  } catch (err) {
    banner(err.message);
  }
}

// publishSaved manda o agente de publicação levar ao PR um review que está em
// disco — o de uma sessão anterior, aberto aqui nos salvos.
async function publishSaved(name, btn) {
  const entry = state.saved.find((x) => x.name === name);
  const alvo = entry ? `${entry.repo}#${entry.number}` : 'the PR';
  if (!confirm(`Publish this saved review on ${alvo} with inline comments?\n\nThe agent runs to publish what is in the file — it does not review again.`)) return;
  btn.disabled = true;
  try {
    const job = await api('/api/reviews/' + encodeURIComponent(name) + '/publish', { method: 'POST' });
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

// commentSaved cola o review salvo no PR, sem agente.
async function commentSaved(name, btn) {
  const entry = state.saved.find((x) => x.name === name);
  const alvo = entry ? `${entry.repo}#${entry.number}` : 'the PR';
  if (!confirm(`Paste this saved review as a comment on ${alvo}?`)) return;
  btn.disabled = true;
  const antes = btn.textContent;
  btn.textContent = 'publishing…';
  try {
    await api('/api/reviews/' + encodeURIComponent(name) + '/comment', { method: 'POST' });
    btn.textContent = '✓ commented on ' + alvo;
    loadPRs(false);
  } catch (err) {
    banner('failed to publish: ' + err.message);
    btn.disabled = false;
    btn.textContent = antes;
  }
}

// --- render ---

function visiblePRs() {
  const q = state.filter.toLowerCase();
  return state.prs.filter((pr) => {
    if (state.scope === 'mine' && !pr.mine) return false;
    if (state.repoFilter && pr.repo !== state.repoFilter) return false;
    if (!matchesReview(pr)) return false;
    if (state.hideDrafts && pr.draft) return false;
    if (!q) return true;
    return `${pr.repo} #${pr.number} ${pr.title} ${pr.author}`.toLowerCase().includes(q);
  });
}

// markedPRs é o que o botão "revisar" vai rodar: o que está marcado **e**
// visível. Filtrar não desmarca nada — a marcação fica esperando o filtro
// sair — mas revisar um PR que saiu da tela seria surpresa.
function markedPRs() {
  return visiblePRs().filter((pr) => state.selected.has(pr.key)).map((pr) => pr.key);
}

// matchesReview separa o que o Bazel já revisou do que ainda não viu. "mudou"
// é o caso que importa: revisado, e com commit novo depois — o review que
// está salvo já não é sobre este código.
function matchesReview(pr) {
  switch (state.reviewFilter) {
    case 'none': return !pr.reviewed;
    case 'reviewed': return !!pr.reviewed && !pr.changed_since_review;
    case 'changed': return !!pr.changed_since_review;
    default: return true;
  }
}

// renderRepoFilter monta as opções com os repositórios monitorados e quantos
// PRs cada um tem agora. Com um repositório só o seletor não aparece: não há
// o que escolher.
function renderRepoFilter() {
  const sel = $('#repo-filter');
  const contagem = new Map();
  for (const pr of state.prs) contagem.set(pr.repo, (contagem.get(pr.repo) || 0) + 1);
  // Os monitorados mandam na ordem; um PR de repositório que saiu da lista
  // ainda aparece, senão ele viraria um filtro impossível de desfazer.
  const repos = [...new Set([...state.repos, ...contagem.keys()])];

  sel.hidden = repos.length < 2;
  if (sel.hidden) {
    if (state.repoFilter) { state.repoFilter = ''; }
    return;
  }
  // O repositório escolhido continua escolhido mesmo que fique sem PR aberto.
  if (state.repoFilter && !repos.includes(state.repoFilter)) state.repoFilter = '';

  sel.innerHTML = '';
  const todos = el('option', null, `any repo (${state.prs.length})`);
  todos.value = '';
  sel.append(todos);
  for (const repo of repos) {
    const o = el('option', null, `${repo} (${contagem.get(repo) || 0})`);
    o.value = repo;
    sel.append(o);
  }
  sel.value = state.repoFilter;
  sel.classList.toggle('on', !!state.repoFilter);
}

// renderAgents monta o seletor com os agents e pipelines do config.yaml.
// selectableAgents são os que podem revisar. O agente de publicação também
// vem do servidor — ele aparece na configuração — mas rodar ele sobre um PR
// sem review pronto não faria nada.
function selectableAgents() {
  return state.agents.filter((a) => !a.publisher);
}

// applyAgents guarda a lista que veio do servidor e mantém a escolha do
// seletor válida: quem foi removido não pode continuar selecionado, e o
// primeiro da lista é o padrão.
//
// adotaPadrao troca a escolha atual pelo novo primeiro. É o que faz o botão
// "tornar padrão" valer já na próxima revisão: sem isso ele mudaria o arquivo
// e o seletor continuaria no agente de antes.
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
    const opt = el('option', null, a.name + (a.pipeline ? ' ⛓' : '') + (a.posts ? ' ⇧ publishes' : ''));
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
  ].filter(Boolean).join('\n') || a.name;
}

// stepsBox é a lista de passos de um job: quem terminou, quem está na vez e
// quem ainda nem começou.
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

// stepSeconds conta o tempo do passo que está rodando a partir do started_at,
// para o relógio andar entre um evento e outro do servidor.
function stepSeconds(st) {
  if (st.state === 'running' && st.started_at) {
    return Math.max(0, Math.round((Date.now() - new Date(st.started_at)) / 1000));
  }
  return st.seconds || 0;
}

// --- log dos agentes ---
//
// O log não vem pelo SSE: um agente falante escreve milhares de linhas e cada
// uma acordaria todo navegador aberto. A página guarda o `next` e busca só o
// que falta, uma vez por segundo enquanto o review roda.

const maxLogLines = 500;

function logState(id) {
  if (!state.logs[id]) {
    state.logs[id] = { next: 0, lines: [], dropped: 0, live: true, busy: false, agents: [] };
  }
  return state.logs[id];
}

// noteAgents mantém a lista de quem já falou, na ordem em que apareceram — é
// ela que dá a cor de cada terminal.
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
    // O log é acessório: falhar em buscá-lo não pode estragar a tela.
  } finally {
    st.busy = false;
  }
}

// logLineEl monta uma linha com a assinatura de quem a escreveu. A cor da
// etiqueta vem da ordem de aparição, para as lentes da frota se distinguirem
// de relance mesmo intercaladas.
// --- um terminal por agente ---
//
// A frota sobe as três lentes dentro do mesmo processo, em paralelo: num
// fluxo só as linhas chegam intercaladas e não dá para seguir raciocínio
// nenhum. Cada agente escreve no próprio terminal, com o próprio scroll.

const maxTermLines = 300;

// agentName é o nome que vai no cabeçalho do terminal. Linha sem assinatura é
// do agente do passo — o servidor já manda esse nome preenchido.
function agentName(l) {
  return l.agent || 'agent';
}

function logLineEl(l) {
  return el('div', 'log-line ' + (l.stream === 'stderr' ? 'stderr' : 'stdout'), l.text);
}

// termFor devolve o corpo do terminal de um agente, criando o terminal na
// primeira linha que ele escreve. A ordem na tela é a ordem em que falaram.
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

// pushLine escreve no terminal do agente e rola se quem lê já estava no fim.
function pushLine(wrap, id, l) {
  const body = termFor(wrap, id, agentName(l));
  const noFim = body.scrollHeight - body.scrollTop - body.clientHeight < 30;
  body.append(logLineEl(l));
  while (body.childElementCount > maxTermLines) body.firstElementChild.remove();
  if (body._count) body._count.textContent = String(body.childElementCount);
  if (noFim) body.scrollTop = body.scrollHeight;
}

// logTerminals monta a grade com o que a página já tem em mãos.
function logTerminals(id) {
  const wrap = el('div', 'terms');
  wrap.id = 'log-terms';
  wrap.dataset.job = id;
  const st = logState(id);
  for (const l of st.lines) pushLine(wrap, id, l);
  if (!st.lines.length) wrap.append(el('p', 'log-empty', 'waiting for the agent to speak…'));
  return wrap;
}

// appendLog manda cada linha nova para o terminal de quem a escreveu.
function appendLog(id, lines) {
  const wrap = $('#log-terms');
  if (!wrap || wrap.dataset.job !== id) return;
  const vazio = wrap.querySelector('.log-empty');
  if (vazio) vazio.remove();
  for (const l of lines) pushLine(wrap, id, l);
  const count = $('#log-count');
  if (count) count.textContent = logCountLabel(id);
}

// total é o que o servidor diz ter — o painel de um review terminado só busca
// as linhas quando alguém o abre, e até lá "0 linhas" seria mentira.
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
  // Log de review já terminado só é buscado quando alguém abre.
  wrap.addEventListener('toggle', () => { if (wrap.open) pollLog(job.id); });
  return wrap;
}

function renderStats() {
  const total = state.prs.length;
  const shown = visiblePRs().length;
  const marked = markedPRs().length;
  // Com filtro ligado o número que interessa é o do recorte — mas o total
  // continua à vista, senão some a pista de que há um filtro escondendo PR.
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
    // Linha abre, checkbox marca: quem clica no PR quer lê-lo, e marcar para
    // revisar tem um alvo próprio.
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

// reviewTags é o histórico do PR na lista: se já foi revisado, quando, e —
// o que mais importa — se ele ganhou commit novo depois disso. Um PR revisado
// que mudou vale um aviso, não um ✓: o que está no GitHub não é mais o que
// o agente leu.
function reviewTags(pr) {
  if (!pr.reviewed) return [];
  if (pr.changed_since_review) {
    const t = el('span', 'tag stale', '⟳ changed since review');
    t.title = `reviewed ${pr.reviewed_at} ago by ${pr.review_agent || 'an agent'}, and the PR got new commits afterwards`;
    return [t];
  }
  const t = el('span', 'tag reviewed', '✓ reviewed ' + pr.reviewed_at + (pr.review_posted ? ' · published' : ''));
  t.title = `reviewed by ${pr.review_agent || 'an agent'}`;
  return [t];
}

function toggle(key, on) {
  if (on) state.selected.add(key); else state.selected.delete(key);
  renderPRs();
}

function updateReviewButton() {
  const n = markedPRs().length;
  const btn = $('#review');
  const semAgente = !selectableAgents().length;
  btn.disabled = n === 0 || semAgente;
  btn.title = semAgente
    ? 'no agents configured — open "config" and build the list out of your skills'
    : 'runs the chosen agent over the ticked PRs';
  btn.textContent = n > 1 ? `review ${n}` : 'review';
}

// togglePRs recolhe a lista da esquerda até a faixa do hambúrguer e a traz de
// volta. É o que dá a tela inteira ao review, e a escolha fica guardada no
// navegador — recolher de novo a cada aba aberta seria trabalho repetido.
function togglePRs(hide) {
  state.prsHidden = hide;
  document.querySelector('main').classList.toggle('no-prs', hide);
  const b = $('#toggle-prs');
  b.title = hide ? 'show the PR list' : 'hide the list (gives the review the full window)';
  b.setAttribute('aria-expanded', hide ? 'false' : 'true');
  try {
    localStorage.setItem('bazel.prsHidden', hide ? '1' : '');
  } catch (err) {
    // Navegador sem storage: a escolha vale só para esta sessão.
  }
}

function renderTabs() {
  document.querySelectorAll('.tab').forEach((t) => t.classList.toggle('on', t.dataset.tab === state.tab));
  if (state.tab === 'saved') loadSaved(); else renderJobs();
}

// renderQuota mostra a cota do Claude — as mesmas janelas do `/usage`: cinco
// horas e sete dias. Ela não se consulta: chega de carona no stream de quem
// está rodando, então o que está na tela é a última leitura, e o título diz de
// quando ela é.
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

// quotaPart pinta a janela conforme ela enche: amarelo aos 75%, vermelho aos
// 90% — o número sozinho não avisa que a parede está perto.
function quotaPart(nome, w) {
  const pct = Math.round((w && w.utilization ? w.utilization : 0) * 100);
  const cls = pct >= 90 ? 'hot' : pct >= 75 ? 'warn' : '';
  return el('span', cls, `${nome} ${pct}%`);
}

function resetLine(nome, w) {
  if (!w || !w.resets_at) return '';
  return `${nome} resets at ${new Date(w.resets_at).toLocaleString()}`;
}

// renderSpend soma o que os reviews desta sessão consumiram. Fica na barra de
// cima porque é a pergunta que se faz sem abrir nada: quanto já custou.
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
    if (job.state === 'done' && !job.publishing) {
      if (!job.posted) {
        const p = el('button', 'btn small', 'publish inline review');
        p.title = 'runs the post skill and publishes with inline comments';
        p.addEventListener('click', (e) => { e.stopPropagation(); publishJob(job.id, p); });
        actions.append(p);
      }
      if (job.posted) {
        actions.append(el('span', 'state done', '✓ commented'));
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

// jobSeconds conta do started_at enquanto o job roda, para o relógio não
// congelar entre um evento e outro do servidor.
function jobSeconds(job) {
  if (job.state === 'running' && job.started_at) {
    return Math.max(0, Math.round((Date.now() - new Date(job.started_at)) / 1000));
  }
  return job.seconds || 0;
}

// labelParts separa o estado do tempo que vem junto. O estado vai em caixa
// alta pelo CSS; o tempo, não — "8S" não é oito segundos, é oito siemens.
function labelParts(job) {
  switch (job.state) {
    case 'queued': return ['queued', ''];
    case 'running': return ['running', jobSeconds(job) + 's'];
    case 'done': return ['done in', job.seconds + 's'];
    case 'failed': return ['failed', ''];
    case 'canceled': return ['canceled', ''];
    default: return [job.state, ''];
  }
}

function label(job) {
  return labelParts(job).filter(Boolean).join(' ');
}

// fillState escreve o estado dentro de um .state, com a unidade de tempo fora
// da caixa alta. Não limpa o elemento: o ponto colorido do card já está lá.
function fillState(span, job) {
  const [texto, tempo] = labelParts(job);
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

// viewerSignature é o que, mudando, exige redesenhar o painel. O tique de um
// segundo passa por aqui: sem isso, refazer o DOM inteiro mataria a rolagem do
// log e a seleção de texto de quem está lendo.
function viewerSignature(job) {
  return [
    job.id, job.state, job.cloning ? 'c' : '',
    (job.steps || []).map((s) => s.state).join(''),
    job.posted ? 'p' : '', job.post_error ? 'e' : '',
    state.bodies[job.id] ? 'b' : '',
  ].join('|');
}

// tickViewer atualiza só o que anda sozinho: os relógios dos passos e do job.
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
      ? 'publishing the review you read — cloning the PR and commenting line by line.'
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
  const md = el('div', 'md');
  md.innerHTML = html;
  v.append(md);

  if (job.steps && job.steps.length > 1) v.append(stepsBox(job));
  if (job.log_lines) v.append(logPanel(job, false));

  // As ações ficam depois do review, e não antes: é aqui que você chega
  // quando terminou de ler — que é o momento de decidir se isso vai para o PR.
  if (!job.publishing) {
    const bar = el('div', 'viewer-actions');
    const pub = el('button', 'btn primary', 'publish inline review');
    pub.title = 'runs the post skill and publishes to the PR with inline comments';
    pub.addEventListener('click', () => publishJob(job.id, pub));
    bar.append(pub);
    if (job.posted) {
      bar.append(el('span', 'state done', '✓ already commented on the PR'));
    } else {
      const cm = el('button', 'btn ghost', 'or paste as a comment');
      cm.addEventListener('click', () => postJob(job.id, cm));
      bar.append(cm);
    }
    v.append(bar);
  }

  const foot = el('p', 'dim');
  foot.style.marginTop = '32px';
  foot.textContent = job.saved_to ? 'saved to ' + job.saved_to : '';
  if (job.workdir) foot.textContent += (foot.textContent ? ' · ' : '') + 'clone kept at ' + job.workdir;
  if (job.truncated) foot.textContent += ' · diff truncated';
  if (job.post_error) v.append(el('div', 'error-box', 'failed to publish: ' + job.post_error));
  v.append(foot);

  // O que o review custou, no fim dele — é onde você chega depois de ler.
  const custo = gasto(job);
  if (custo) {
    const linha = el('p', 'dim tokens-foot', custo + ' · ' + jobSeconds(job) + 's');
    linha.title = gastoTitle(job);
    v.append(linha);
  }
  v.scrollTop = 0;
}

// --- ligações ---

function wire() {
  $('#refresh').addEventListener('click', () => loadPRs(true));
  $('#review').addEventListener('click', startReview);
  $('#agent').addEventListener('change', (e) => {
    state.agent = e.target.value;
    $('#agent').title = agentTitle(state.agent);
  });
  // Os eventos do servidor chegam só nas bordas dos passos; este tique é o que
  // faz o tempo do passo em curso andar na tela.
  setInterval(() => {
    if (!state.jobs.some((j) => j.state === 'running')) return;
    renderJobs();
    const active = state.jobs.find((j) => j.id === state.activeJob);
    if (!active) return;
    renderViewer();
    // O log só é buscado do review que está aberto e ainda vivo.
    if (active.state === 'running' && logState(active.id).live && $('#log-terms')) pollLog(active.id);
  }, 1000);
  $('#filter').addEventListener('input', (e) => { state.filter = e.target.value; renderPRs(); });
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
  $('#show-config').addEventListener('click', showConfig);
  $('#repo-form').addEventListener('submit', addRepo);
  $('#modal-close').addEventListener('click', () => { $('#modal').hidden = true; });
  $('#modal').addEventListener('click', (e) => { if (e.target.id === 'modal') $('#modal').hidden = true; });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') $('#modal').hidden = true;
    if (e.key === '/' && document.activeElement !== $('#filter')) { e.preventDefault(); $('#filter').focus(); }
    if (e.key === 'r' && (e.metaKey || e.ctrlKey)) return;
  });
}

// renderAgentList é a lista de agentes da configuração: o que cada um chama,
// se a skill está instalada, quem é o padrão e o botão de tirar da lista.
//
// A lista começa vazia de propósito — é você que a monta, na seção de baixo, a
// partir das skills que estão instaladas nesta máquina.
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
    if (a.publisher) top.append(el('span', 'tag', 'used when publishing'));
    else if (state.agent === a.name) top.append(el('span', 'tag mine', 'default'));

    // O agente de publicação não é uma escolha da lista: ele é o que roda
    // quando você manda publicar um review já lido, e não sai daqui.
    if (!a.publisher) {
      top.append(el('div', 'spacer'));
      if (state.agent !== a.name) {
        const d = el('button', 'btn small ghost', 'make default');
        d.title = 'becomes the agent the selector starts on';
        d.addEventListener('click', () => agentAction('/api/agents/' + encodeURIComponent(a.name) + '/default', 'POST', d));
        top.append(d);
      }
      const rm = el('button', 'btn small ghost', 'remove');
      rm.addEventListener('click', () => agentAction('/api/agents/' + encodeURIComponent(a.name), 'DELETE', rm));
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

// agentAction manda a mudança e redesenha tudo que depende da lista: o
// seletor, a configuração e o yaml em disco, que também mudou.
async function agentAction(path, method, btn, body) {
  btn.disabled = true;
  try {
    const res = await api(path, body ? { method, body: JSON.stringify(body) } : { method });
    applyAgents(res.agents || [], path.endsWith('/default'));
    // Esvaziar a lista de novo é voltar ao estado inicial: sem agente não há
    // review, e quem está com a configuração aberta precisa saber.
    banner(selectableAgents().length ? '' : 'No agents configured — pick below which skills become agents.');
    await refreshConfig();
  } catch (err) {
    banner(err.message);
    btn.disabled = false;
  }
}

// renderSkillList mostra o que está instalado de fato — a lista que manda, e
// de onde saem os agentes. Cada skill vira um agente com um clique: "usar" é a
// lente que você lê antes de publicar, "⇧ publica" é a que vai direto ao PR.
function renderSkillList() {
  const box = $('#skill-list');
  box.innerHTML = '';
  $('#skills-dir').textContent = state.skillsDir ? '· ' + state.skillsDir : '';
  if (!state.skills.length) {
    box.append(el('p', 'none', `nothing in ${state.skillsDir || '~/.claude/skills'} — with no skill installed there is no agent to build.`));
    return;
  }
  // As que já viraram agente vêm primeiro: são as que importam aqui.
  const usadas = new Set();
  for (const a of state.agents) for (const sk of a.skills || []) usadas.add(sk.name);
  // Só os agentes da lista contam para "já está lá": o de publicação não é
  // uma escolha do seletor, e é o que o servidor também ignora ao adicionar.
  const nomes = new Set(state.agents.filter((a) => !a.publisher).map((a) => a.name));
  const ordenadas = [...state.skills].sort((x, y) => (usadas.has(y.name) ? 1 : 0) - (usadas.has(x.name) ? 1 : 0));

  for (const sk of ordenadas) {
    const row = el('div', 'skill-row' + (usadas.has(sk.name) ? ' on' : ''));
    row.append(el('span', 'skill-name', '/' + sk.name));
    row.append(el('span', 'skill-desc', sk.description || ''));

    const acoes = el('span', 'skill-actions');
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
  } catch (err) {
    state.skills = [];
  }
}

async function showConfig() {
  await refreshConfig();
  $('#modal').hidden = false;
}

// refreshConfig redesenha a configuração inteira a partir do disco. É chamada
// ao abrir e depois de cada mudança na lista de agentes — o yaml na tela é o
// arquivo, e ele acabou de mudar.
async function refreshConfig() {
  try {
    const cfg = await api('/api/config');
    $('#config-path').textContent = cfg.path;
    $('#config-yaml').textContent = cfg.yaml;
  } catch (err) {
    $('#config-yaml').textContent = err.message;
  }
  renderRepos();
  await loadSkills();
  renderAgentList();
  renderSkillList();
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
