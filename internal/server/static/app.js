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
  skills: [],         // skills instaladas na máquina
  skillsDir: '',
  agent: '',          // nome do que roda no próximo review
  selected: new Set(),
  scope: 'all',
  filter: '',
  hideDrafts: false,
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

function banner(msg) {
  const b = $('#banner');
  if (!msg) { b.hidden = true; return; }
  b.textContent = msg;
  b.hidden = false;
}

// --- carga ---

async function boot() {
  wire();
  connect();
  try {
    const st = await api('/api/state');
    state.me = st.me;
    state.jobs = st.jobs || [];
    $('#who').textContent = '@' + st.me;
    $('#who').title = `config: ${st.config_path}\nreviews: ${st.reviews_dir}\nagente: ${st.agent.command} ${(st.agent.args || []).join(' ')}\nreviews simultâneos: ${st.concurrency}`;
    state.agents = st.agents || [];
    const primeiro = selectableAgents()[0];
    state.agent = primeiro ? primeiro.name : '';
    renderAgents();
    state.repos = st.repos || [];
    if (!state.repos.length) {
      banner('Nenhum repositório monitorado — abra "config" no topo e adicione um (owner/repo).');
    }
    renderJobs();
  } catch (err) {
    banner('Não consegui falar com o servidor: ' + err.message);
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
    renderPRs();
    renderStats();
  } catch (err) {
    list.innerHTML = '';
    const p = el('p', 'empty', 'falhou: ' + err.message);
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

// --- ações ---

function startReview() {
  return reviewRefs([...state.selected]);
}

async function reviewRefs(refs) {
  if (!refs.length) return;
  // Escolher um agente que publica é escrever no PR de outra pessoa: vale uma
  // pergunta antes, que é a mesma cerimônia do botão "publicar no PR".
  const chosen = state.agents.find((a) => a.name === state.agent);
  if (chosen && chosen.posts) {
    const alvo = refs.length > 1 ? `${refs.length} PRs` : refs[0];
    if (!confirm(`O agente "${chosen.name}" publica o review direto em ${alvo}. Rodar assim mesmo?`)) return;
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
    el('span', null, `${pr.changed_files} arq`),
    el('span', null, pr.age),
  );
  if (pr.draft) sub.append(el('span', 'tag draft', 'draft'));
  if (pr.review_decision === 'APPROVED') sub.append(el('span', 'tag approved', 'aprovado'));
  if (pr.review_decision === 'CHANGES_REQUESTED') sub.append(el('span', 'tag changes', 'mudanças'));
  for (const t of reviewTags(pr)) sub.append(t);
  grow.append(h, sub);

  const act = el('button', 'btn primary', 'revisar este PR');
  act.addEventListener('click', () => reviewRefs([pr.key]));
  head.append(grow, act);
  v.append(head);

  if (pr.changed_since_review) {
    const aviso = el('div', 'stale-box');
    aviso.textContent = `Este PR mudou depois do review de ${pr.reviewed_at} atrás`
      + (pr.review_agent ? ` (${pr.review_agent})` : '')
      + ' — o que está aqui não é mais o que o agente leu.';
    v.append(aviso);
  }

  if (pr.body_html) {
    const md = el('div', 'md');
    md.innerHTML = pr.body_html;
    v.append(md);
  } else {
    v.append(el('p', 'dim', '(sem descrição)'));
  }

  const done = state.jobs.filter((j) => j.pr.key === pr.key);
  if (done.length) {
    const back = el('p', 'dim');
    back.style.marginTop = '28px';
    back.append(document.createTextNode('reviews nesta sessão: '));
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
  if (!confirm(`Publicar este review em ${job.pr.key} com comentários inline?\n\nO agente vai rodar de novo, só para publicar o que você acabou de ler.`)) return;
  if (btn) { btn.disabled = true; btn.textContent = 'publicando…'; }
  try {
    const view = await api(`/api/jobs/${encodeURIComponent(id)}/publish`, { method: 'POST' });
    upsertJob(view);
    state.activeJob = view.id;
    state.tab = 'queue';
    renderJobs();
    renderViewer();
  } catch (err) {
    banner(err.message);
    if (btn) { btn.disabled = false; btn.textContent = 'publicar review inline'; }
  }
}

async function postJob(id, btn) {
  const job = state.jobs.find((j) => j.id === id);
  if (!job) return;
  const aviso = job.posts
    ? `O agente "${job.agent}" já publicou este review em ${job.pr.key}. Publicar de novo, como comentário?`
    : `Publicar este review como comentário em ${job.pr.key}?`;
  if (!confirm(aviso)) return;
  if (btn) { btn.disabled = true; btn.textContent = 'publicando…'; }
  try {
    upsertJob(await api(`/api/jobs/${encodeURIComponent(id)}/post`, { method: 'POST' }));
  } catch (err) {
    banner('falha ao publicar: ' + err.message);
    if (btn) { btn.disabled = false; btn.textContent = 'publicar no PR'; }
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
    v.scrollTop = 0;
  } catch (err) {
    banner(err.message);
  }
}

// --- render ---

function visiblePRs() {
  const q = state.filter.toLowerCase();
  return state.prs.filter((pr) => {
    if (state.scope === 'mine' && !pr.mine) return false;
    if (state.hideDrafts && pr.draft) return false;
    if (!q) return true;
    return `${pr.repo} #${pr.number} ${pr.title} ${pr.author}`.toLowerCase().includes(q);
  });
}

// renderAgents monta o seletor com os agents e pipelines do config.yaml — a
// mesma lista que a TUI mostra no enter.
// selectableAgents são os que podem revisar. O agente de publicação também
// vem do servidor — ele aparece na configuração — mas rodar ele sobre um PR
// sem review pronto não faria nada.
function selectableAgents() {
  return state.agents.filter((a) => !a.publisher);
}

function renderAgents() {
  const sel = $('#agent');
  sel.innerHTML = '';
  const opcoes = selectableAgents();
  if (!opcoes.length) { sel.hidden = true; return; }
  sel.hidden = false;
  for (const a of opcoes) {
    const opt = el('option', null, a.name + (a.pipeline ? ' ⛓' : '') + (a.posts ? ' ⇧ publica' : ''));
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
  if (!a) return 'qual agente roda sobre os PRs marcados';
  return [
    a.description,
    a.pipeline ? 'pipeline: ' + (a.steps || []).join(' → ') : '',
    a.posts ? '⇧ este agente publica o review no PR sozinho' : '',
  ].filter(Boolean).join('\n') || a.name;
}

// stepsBox é a lista de passos de um job: quem terminou, quem está na vez e
// quem ainda nem começou.
function stepsBox(job) {
  const box = el('div', 'steps');
  if (job.cloning) box.append(stepRow({ name: 'clonando o repositório…', state: 'running' }));
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
    const who = l.agent || 'agente';
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
  return l.agent || 'agente';
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
  if (!st.lines.length) wrap.append(el('p', 'log-empty', 'esperando o agente falar…'));
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

function logCountLabel(id) {
  const st = logState(id);
  const n = st.next;
  const agentes = st.agents.length;
  return `${n} linha${n === 1 ? '' : 's'}`
    + (agentes > 1 ? ` · ${agentes} agentes` : '')
    + (st.dropped ? ` · ${st.dropped} mais antigas descartadas` : '');
}

function logPanel(job, open) {
  const wrap = el('details', 'log-panel');
  wrap.open = open;
  const sum = el('summary');
  const count = el('span', 'dim', logCountLabel(job.id));
  count.id = 'log-count';
  sum.append(el('span', null, 'log dos agentes'), document.createTextNode(' · '), count);

  wrap.append(sum, logTerminals(job.id));
  // Log de review já terminado só é buscado quando alguém abre.
  wrap.addEventListener('toggle', () => { if (wrap.open) pollLog(job.id); });
  return wrap;
}

function renderStats() {
  const total = state.prs.length;
  const marked = state.selected.size;
  $('#stats').textContent = `${total} PR${total === 1 ? '' : 's'}` + (marked ? ` · ${marked} marcado${marked === 1 ? '' : 's'}` : '');
}

function renderPRs() {
  const list = $('#pr-list');
  const prs = visiblePRs();
  list.innerHTML = '';
  if (!prs.length) {
    list.append(el('p', 'empty', state.prs.length ? 'nada bate com o filtro' : 'nenhum PR aberto'));
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
    if (pr.mine) top.append(el('span', 'tag mine', 'você'));
    if (pr.review_decision === 'APPROVED') top.append(el('span', 'tag approved', 'aprovado'));
    if (pr.review_decision === 'CHANGES_REQUESTED') top.append(el('span', 'tag changes', 'mudanças'));
    for (const t of reviewTags(pr)) top.append(t);

    const meta = el('div', 'meta');
    meta.append(
      el('span', null, '@' + pr.author),
      el('span', 'add', '+' + pr.additions),
      el('span', 'del', '−' + pr.deletions),
      el('span', null, `${pr.changed_files} arq`),
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
    const t = el('span', 'tag stale', '⟳ mudou desde o review');
    t.title = `revisado há ${pr.reviewed_at} por ${pr.review_agent || 'um agente'}, e o PR recebeu commit novo depois`;
    return [t];
  }
  const t = el('span', 'tag reviewed', '✓ revisado ' + pr.reviewed_at + (pr.review_posted ? ' · publicado' : ''));
  t.title = `revisado por ${pr.review_agent || 'um agente'}`;
  return [t];
}

function toggle(key, on) {
  if (on) state.selected.add(key); else state.selected.delete(key);
  renderPRs();
}

function updateReviewButton() {
  const n = state.selected.size;
  const btn = $('#review');
  btn.disabled = n === 0;
  btn.textContent = n > 1 ? `revisar ${n}` : 'revisar';
}

function renderTabs() {
  document.querySelectorAll('.tab').forEach((t) => t.classList.toggle('on', t.dataset.tab === state.tab));
  if (state.tab === 'saved') loadSaved(); else renderJobs();
}

function renderJobs() {
  const active = state.jobs.filter((j) => j.state === 'queued' || j.state === 'running').length;
  $('#queue-count').textContent = String(state.jobs.length);
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
    st.append(el('span', 'dot'), document.createTextNode(' ' + label(job)));
    top.append(el('span', 'num', '#' + job.pr.number), el('span', 'repo', job.pr.slug));
    const sp = el('div', 'spacer'); sp.style.flex = '1';
    top.append(sp, st);
    card.append(top, el('div', 'title', job.pr.title));
    if (job.agent) {
      const who = el('div', 'meta');
      who.append(el('span', null, job.agent + (job.pipeline ? ' ⛓' : '')));
      if (job.publishing) who.append(el('span', 'tag posts', '⇧ publicando'));
      else if (job.posts) who.append(el('span', 'tag posts', '⇧ publica'));
      card.append(who);
    }
    if (job.cloning || (job.steps && job.steps.length > 1)) card.append(stepsBox(job));

    const actions = el('div', 'card-actions');
    if (job.state === 'queued' || job.state === 'running') {
      const b = el('button', 'btn small ghost', 'cancelar');
      b.addEventListener('click', (e) => { e.stopPropagation(); cancelJob(job.id); });
      actions.append(b);
    }
    if (job.state === 'done' && !job.publishing) {
      if (!job.posted) {
        const p = el('button', 'btn small', 'publicar review inline');
        p.title = 'roda a skill de post e publica com comentários inline';
        p.addEventListener('click', (e) => { e.stopPropagation(); publishJob(job.id, p); });
        actions.append(p);
      }
      if (job.posted) {
        actions.append(el('span', 'state done', '✓ comentado'));
      } else {
        const b = el('button', 'btn small ghost', 'comentar');
        b.title = 'cola o review como um comentário só, sem agente';
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

function label(job) {
  switch (job.state) {
    case 'queued': return 'na fila';
    case 'running': return `rodando ${jobSeconds(job)}s`;
    case 'done': return `pronto em ${job.seconds}s`;
    case 'failed': return 'falhou';
    case 'canceled': return 'cancelado';
    default: return job.state;
  }
}

function renderSaved() {
  const rail = $('#rail');
  rail.innerHTML = '';
  if (!state.saved.length) {
    clearViewer().append(el('p', 'empty', 'nenhum review salvo ainda'));
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
  if (label_) label_.textContent = label(job);
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
  sub.append(link, el('span', null, '@' + job.pr.author), el('span', null, `${job.pr.branch} → ${job.pr.base}`), el('span', 'state ' + job.state, label(job)));
  if (job.agent) sub.append(el('span', null, job.agent + (job.pipeline ? ' ⛓' : '')));
  if (job.posts) sub.append(el('span', 'tag posts', '⇧ publica no PR'));
  grow.append(h, sub);
  head.append(grow);
  v.append(head);

  if (job.state === 'queued') {
    v.append(el('p', 'dim', 'na fila — começa assim que um worker liberar.'));
    if (job.steps && job.steps.length) v.append(stepsBox(job));
    return;
  }
  if (job.state === 'running') {
    v.append(el('p', 'dim', job.publishing
      ? 'publicando no PR o review que você leu — clonando e comentando linha a linha.'
      : 'rodando no clone do PR. Pode fechar a aba, o review continua.'));
    v.append(stepsBox(job));
    v.append(logPanel(job, true));
    pollLog(job.id);
    return;
  }
  if (job.state === 'canceled') {
    v.append(el('p', 'dim', 'cancelado.'));
    return;
  }
  if (job.state === 'failed') {
    v.append(el('div', 'error-box', job.error || 'falhou'));
    if (job.log_lines) v.append(logPanel(job, true));
    pollLog(job.id);
    return;
  }

  const html = state.bodies[job.id];
  if (!html) {
    v.append(el('p', 'dim', 'carregando review…'));
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
    const pub = el('button', 'btn primary', 'publicar review inline');
    pub.title = 'roda a skill de post e publica no PR com comentários inline';
    pub.addEventListener('click', () => publishJob(job.id, pub));
    bar.append(pub);
    if (job.posted) {
      bar.append(el('span', 'state done', '✓ já comentado no PR'));
    } else {
      const cm = el('button', 'btn ghost', 'ou colar como comentário');
      cm.addEventListener('click', () => postJob(job.id, cm));
      bar.append(cm);
    }
    v.append(bar);
  }

  const foot = el('p', 'dim');
  foot.style.marginTop = '32px';
  foot.textContent = job.saved_to ? 'salvo em ' + job.saved_to : '';
  if (job.workdir) foot.textContent += (foot.textContent ? ' · ' : '') + 'clone preservado em ' + job.workdir;
  if (job.truncated) foot.textContent += ' · diff truncado';
  if (job.post_error) v.append(el('div', 'error-box', 'falha ao publicar: ' + job.post_error));
  v.append(foot);
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

// renderAgentList mostra, na configuração, cada agente e a skill que ele
// invoca — com o aviso quando essa skill não está instalada. Um agente que
// chama uma skill que você não tem só falha na hora de rodar.
function renderAgentList() {
  const box = $('#agent-list');
  box.innerHTML = '';
  for (const a of state.agents) {
    const row = el('div', 'agent-row');

    const top = el('div', 'agent-top');
    top.append(el('span', 'agent-name', a.name));
    if (a.pipeline) top.append(el('span', 'tag', 'pipeline'));
    if (a.posts) top.append(el('span', 'tag posts', '⇧ publica'));
    if (a.publisher) top.append(el('span', 'tag', 'usado ao publicar'));
    else if (state.agent === a.name) top.append(el('span', 'tag mine', 'padrão'));
    row.append(top);

    if (a.description) row.append(el('div', 'agent-desc', a.description));
    if (a.pipeline) row.append(el('div', 'agent-desc', (a.steps || []).join(' → ')));

    const usa = el('div', 'agent-skills');
    for (const sk of a.skills || []) {
      const t = el('span', 'skill-ref ' + (sk.installed ? 'ok' : 'missing'),
        (sk.installed ? '✓ /' : '✗ /') + sk.name);
      t.title = sk.installed
        ? 'instalada em ' + state.skillsDir
        : `esta skill não está em ${state.skillsDir} — o agente vai falhar ao rodar`;
      usa.append(t);
    }
    if (!(a.skills || []).length) usa.append(el('span', 'dim', 'não chama skill nenhuma'));
    row.append(usa);

    box.append(row);
  }
}

// renderSkillList mostra o que está instalado de fato — a lista que manda.
function renderSkillList() {
  const box = $('#skill-list');
  box.innerHTML = '';
  $('#skills-dir').textContent = state.skillsDir ? '· ' + state.skillsDir : '';
  if (!state.skills.length) {
    box.append(el('p', 'none', `nada em ${state.skillsDir || '~/.claude/skills'} — os agentes que chamam skill não vão rodar.`));
    return;
  }
  // As que algum agente usa vêm primeiro: são as que importam aqui.
  const usadas = new Set();
  for (const a of state.agents) for (const sk of a.skills || []) usadas.add(sk.name);
  const ordenadas = [...state.skills].sort((x, y) => (usadas.has(y.name) ? 1 : 0) - (usadas.has(x.name) ? 1 : 0));

  for (const sk of ordenadas) {
    const row = el('div', 'skill-row' + (usadas.has(sk.name) ? ' on' : ''));
    row.append(el('span', 'skill-name', '/' + sk.name));
    row.append(el('span', 'skill-desc', sk.description || ''));
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
  $('#modal').hidden = false;
}

function renderRepos() {
  const box = $('#repo-list');
  box.innerHTML = '';
  if (!state.repos.length) {
    box.append(el('p', 'none', 'nenhum ainda — o Bazel não tem onde procurar PR.'));
    return;
  }
  for (const repo of state.repos) {
    const row = el('div', 'repo-row');
    row.append(el('span', null, repo));
    const rm = el('button', 'btn small ghost', 'remover');
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
