package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/beroni/bazel/internal/agent"
	"github.com/beroni/bazel/internal/config"
	"github.com/beroni/bazel/internal/gh"
	"github.com/beroni/bazel/internal/store"
)

// State é o ciclo de vida de um review na fila.
type State string

const (
	StateQueued   State = "queued"
	StateRunning  State = "running"
	StateDone     State = "done"
	StateFailed   State = "failed"
	StateCanceled State = "canceled"
)

// maxJobs limita o histórico em memória. Os reviews que interessam já estão
// salvos em disco; isto aqui é só a fila da sessão.
const maxJobs = 200

// O log de um review é uma janela, não um arquivo: um agente falante escreve
// milhares de linhas e o que interessa na tela é o fim. Estes dois limites são
// o teto de memória por job.
const (
	maxLogLines     = 500
	maxLogLineRunes = 2000
)

// logLine é uma linha que um agente escreveu. Seq é monotônico por job: é como
// o navegador pede só o que ainda não viu.
type logLine struct {
	Seq  int `json:"seq"`
	Step int `json:"step"`
	// Agent é quem escreveu: o agente do passo ou um sub-agente dele. É por
	// ele que a página separa as lentes que rodam em paralelo.
	Agent  string `json:"agent"`
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

// jobStep é um passo do review — um agente da escolha — como a página o vê.
type jobStep struct {
	Name      string
	State     State
	StartedAt time.Time
	Duration  time.Duration
	Err       string
}

// Job é um review pedido pela interface: enfileirado, rodando ou terminado.
//
// Um review leva minutos — o `timeout_seconds` padrão é 1800 — e nenhuma
// requisição HTTP segura isso. Por isso o handler só enfileira e devolve o id;
// o resultado chega ao navegador pelo SSE de /api/events.
type Job struct {
	ID   string
	PR   gh.PR
	Mine bool

	// Choice é o agente ou pipeline escolhido para este PR; Steps é o
	// andamento de cada passo dele, que é o que a página anima.
	Choice  config.Choice
	Steps   []jobStep
	Cloning bool

	// publish, quando preenchido, faz deste job uma publicação: em vez de
	// revisar, o agente pega o review já lido e o põe no PR.
	publish *publishInput

	// logs é a janela do que os agentes escreveram; logSeq é o próximo
	// número de linha e dropped conta o que já saiu pela frente da janela.
	logs    []logLine
	logSeq  int
	dropped int

	State      State
	QueuedAt   time.Time
	StartedAt  time.Time
	FinishedAt time.Time

	Result agent.Result
	// Live é o gasto parcial de um review em curso: o fechado dos passos que
	// já terminaram mais o que o passo atual já consumiu.
	Live    agent.Usage
	Err     string
	SavedTo string
	Posted  bool
	PostErr string

	cancel context.CancelFunc
}

func (j *Job) duration() time.Duration {
	switch {
	case j.StartedAt.IsZero():
		return 0
	case j.FinishedAt.IsZero():
		return time.Since(j.StartedAt)
	default:
		return j.FinishedAt.Sub(j.StartedAt)
	}
}

// publishInput é o review já pronto que um job de publicação leva ao PR.
type publishInput struct {
	Path string
	Body string
	// From é o id do job que produziu o review, para a página ligar os dois.
	From string
}

// Manager é a fila de reviews e o pool que a consome.
type Manager struct {
	cfg        *config.Config
	runner     *agent.Runner
	reviewsDir string
	hub        *Hub
	ctx        context.Context

	queue chan *Job

	mu    sync.Mutex
	jobs  map[string]*Job
	order []string
	seq   int
}

// NewManager sobe o pool de workers. concurrency limita quantos agentes rodam
// ao mesmo tempo: cada review clona um repositório e sobe um processo, então
// isso é o que separa "revisando dois PRs" de "derrubando o notebook".
func NewManager(ctx context.Context, cfg *config.Config, reviewsDir string, concurrency int, keep bool, hub *Hub) *Manager {
	if concurrency < 1 {
		concurrency = 1
	}
	runner := agent.New(cfg)
	runner.KeepWorkspace = keep

	m := &Manager{
		cfg:        cfg,
		runner:     runner,
		reviewsDir: reviewsDir,
		hub:        hub,
		ctx:        ctx,
		queue:      make(chan *Job, 256),
		jobs:       map[string]*Job{},
	}
	for range concurrency {
		go m.worker()
	}
	return m
}

// Enqueue põe um PR na fila com o agente escolhido. O mesmo PR com o mesmo
// agente já esperando ou rodando devolve o job existente em vez de clonar o
// repositório duas vezes — com outro agente é outro review, e entra na fila.
func (m *Manager) Enqueue(pr gh.PR, mine bool, choice config.Choice) (jobView, error) {
	if len(choice.Steps) == 0 {
		choice = m.cfg.DefaultChoice()
	}
	// Sem agente nenhum não há review para enfileirar: a lista começa vazia e
	// é a página que a preenche com as skills instaladas.
	if len(choice.Steps) == 0 {
		return jobView{}, config.ErrNoAgents
	}
	m.mu.Lock()
	for _, id := range m.order {
		j := m.jobs[id]
		if j.PR.Key() == pr.Key() && j.Choice.Name == choice.Name &&
			(j.State == StateQueued || j.State == StateRunning) {
			v := j.view(false)
			m.mu.Unlock()
			return v, nil
		}
	}
	m.seq++
	steps := make([]jobStep, 0, len(choice.Steps))
	for _, name := range choice.StepNames() {
		steps = append(steps, jobStep{Name: name, State: StateQueued})
	}
	job := &Job{
		ID:       fmt.Sprintf("j%d", m.seq),
		PR:       pr,
		Mine:     mine,
		Choice:   choice,
		Steps:    steps,
		State:    StateQueued,
		QueuedAt: time.Now(),
	}
	m.jobs[job.ID] = job
	m.order = append(m.order, job.ID)
	m.trimLocked()
	m.mu.Unlock()

	select {
	case m.queue <- job:
	default:
		m.finish(job, StateFailed, "fila cheia — espere os reviews em andamento")
		return m.mustView(job.ID), fmt.Errorf("fila cheia")
	}
	m.publish(job)
	return m.mustView(job.ID), nil
}

// trimLocked descarta os jobs terminados mais antigos. Chamar com o lock.
func (m *Manager) trimLocked() {
	for len(m.order) > maxJobs {
		for i, id := range m.order {
			j := m.jobs[id]
			if j.State == StateQueued || j.State == StateRunning {
				continue
			}
			delete(m.jobs, id)
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
		// Nada descartável: tudo na fila está ativo.
		if len(m.order) > maxJobs {
			return
		}
	}
}

func (m *Manager) worker() {
	for job := range m.queue {
		m.run(job)
	}
}

func (m *Manager) run(job *Job) {
	ctx, cancel := context.WithCancel(m.ctx)
	defer cancel()

	m.mu.Lock()
	// Cancelado enquanto esperava na fila.
	if job.State != StateQueued {
		m.mu.Unlock()
		return
	}
	job.State = StateRunning
	job.StartedAt = time.Now()
	job.cancel = cancel
	m.mu.Unlock()
	m.publish(job)

	onEvent := func(e agent.Event) { m.applyEvent(job, e) }
	var (
		res agent.Result
		err error
	)
	if job.publish != nil {
		res, err = m.runner.Publish(ctx, job.PR, job.Choice, job.publish.Path, job.publish.Body, onEvent)
	} else {
		res, err = m.runner.Review(ctx, job.PR, job.Choice, onEvent)
	}
	if err != nil {
		state := StateFailed
		if ctx.Err() != nil {
			state = StateCanceled
		}
		m.finish(job, state, err.Error())
		return
	}

	// O relatório de uma publicação não vira arquivo: o review já está salvo,
	// e este job só conta o que foi para o PR.
	var (
		path    string
		saveErr error
	)
	if job.publish == nil {
		path, saveErr = store.Save(m.reviewsDir, res)
	}

	// O PR entra no índice: é o ✓ da lista, e é o commit gravado aqui que
	// depois denuncia mudança no PR depois do review.
	if job.publish == nil {
		_ = store.MarkReviewed(m.reviewsDir, res, path)
	} else {
		_ = store.MarkPosted(m.reviewsDir, res.PR.Key())
	}

	m.mu.Lock()
	job.Result = res
	job.SavedTo = path
	if saveErr != nil {
		job.Err = "review pronto, mas não consegui salvar em disco: " + saveErr.Error()
	}
	job.State = StateDone
	job.FinishedAt = time.Now()
	job.Cloning = false
	job.cancel = nil
	m.mu.Unlock()
	m.publish(job)
}

// applyEvent registra o andamento de um passo e avisa os navegadores. É
// chamada da goroutine do runner, então mexe no job sob o lock e só publica
// depois de soltá-lo.
func (m *Manager) applyEvent(job *Job, e agent.Event) {
	// Linha de log não mexe no estado do job e não vai para o SSE: um agente
	// falante geraria milhares de eventos e cada um acordaria todo navegador
	// aberto. O navegador busca o log incrementalmente em /api/jobs/{id}/log.
	if e.Kind == agent.EventLog {
		m.mu.Lock()
		job.appendLog(e.Index, e.Agent, e.Stream, e.Text)
		m.mu.Unlock()
		return
	}

	// O gasto parcial anda sozinho: não mexe em passo nenhum, só no número
	// que a página mostra enquanto o agente trabalha.
	if e.Kind == agent.EventUsage {
		m.mu.Lock()
		job.Live = e.Usage
		m.mu.Unlock()
		m.publish(job)
		return
	}

	m.mu.Lock()
	switch {
	case e.Kind == agent.EventClone:
		job.Cloning = true
	case e.Index < 0 || e.Index >= len(job.Steps):
		// Evento de um passo que não existe: nada a mostrar.
	case e.Kind == agent.EventStep:
		job.Cloning = false
		job.Steps[e.Index].State = StateRunning
		job.Steps[e.Index].StartedAt = time.Now()
	case e.Kind == agent.EventStepDone:
		st := &job.Steps[e.Index]
		st.Duration = e.Duration
		st.State = StateDone
		if e.Err != nil {
			st.State = StateFailed
			st.Err = e.Err.Error()
		}
	}
	m.mu.Unlock()
	m.publish(job)
}

func (m *Manager) finish(job *Job, state State, errMsg string) {
	m.mu.Lock()
	job.State = state
	job.Err = errMsg
	job.FinishedAt = time.Now()
	job.Cloning = false
	job.cancel = nil
	m.mu.Unlock()
	m.publish(job)
}

// appendLog guarda uma linha, jogando fora a mais velha quando a janela enche.
// Chamar com o lock do Manager seguro.
func (j *Job) appendLog(step int, who, stream, text string) {
	if r := []rune(text); len(r) > maxLogLineRunes {
		text = string(r[:maxLogLineRunes]) + "…"
	}
	j.logs = append(j.logs, logLine{Seq: j.logSeq, Step: step, Agent: who, Stream: stream, Text: text})
	j.logSeq++
	if len(j.logs) > maxLogLines {
		j.dropped += len(j.logs) - maxLogLines
		j.logs = j.logs[len(j.logs)-maxLogLines:]
	}
}

// logView é o pedaço do log que o navegador ainda não tem.
type logView struct {
	Lines []logLine `json:"lines"`
	// Next é o seq a pedir na próxima vez.
	Next int `json:"next"`
	// Dropped conta o que a janela descartou — o navegador avisa do buraco.
	Dropped int `json:"dropped"`
	// Live diz se ainda vem mais coisa; falso, o navegador para de pedir.
	Live bool `json:"live"`
}

// Log devolve as linhas com seq >= from.
func (m *Manager) Log(id string, from int) (logView, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return logView{}, false
	}
	out := logView{
		Lines:   []logLine{},
		Next:    job.logSeq,
		Dropped: job.dropped,
		Live:    job.State == StateQueued || job.State == StateRunning,
	}
	for _, l := range job.logs {
		if l.Seq >= from {
			out.Lines = append(out.Lines, l)
		}
	}
	return out, true
}

// Remove tira um job da fila. Um review ainda vivo é cancelado antes: o card
// some da tela, e deixar o processo rodando sem nada que o mostre seria um
// agente fantasma queimando token.
//
// Só sai da memória desta sessão — o markdown salvo e o índice de revisados
// continuam onde estão.
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("job %q não existe", id)
	}
	cancel := job.cancel
	vivo := job.State == StateQueued || job.State == StateRunning
	if vivo {
		// O worker só descobre o cancelamento depois; marcar aqui impede que
		// ele republique um job que a página já esqueceu.
		job.State = StateCanceled
		job.FinishedAt = time.Now()
	}
	delete(m.jobs, id)
	for i, other := range m.order {
		if other == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	m.mu.Unlock()

	if vivo && cancel != nil {
		cancel()
	}
	m.hub.Broadcast("job_gone", map[string]string{"id": id})
	return nil
}

// Cancel interrompe um review — na fila ou já rodando.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("job %q não existe", id)
	}
	switch job.State {
	case StateQueued:
		job.State = StateCanceled
		job.FinishedAt = time.Now()
		m.mu.Unlock()
		m.publish(job)
		return nil
	case StateRunning:
		cancel := job.cancel
		m.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil
	default:
		m.mu.Unlock()
		return fmt.Errorf("esse review já terminou")
	}
}

// PublishWithAgent enfileira um job que leva ao PR um review já pronto — o que
// o usuário acabou de ler. É o outro caminho para o GitHub: o comentário
// simples do Post é o Bazel escrevendo; este é o agente publicando um review
// com comentários inline.
func (m *Manager) PublishWithAgent(id string) (jobView, error) {
	m.mu.Lock()
	src, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return jobView{}, fmt.Errorf("job %q não existe", id)
	}
	if src.State != StateDone {
		m.mu.Unlock()
		return jobView{}, errors.New("esse review não terminou")
	}
	if src.publish != nil {
		m.mu.Unlock()
		return jobView{}, errors.New("isso já é uma publicação")
	}
	if src.SavedTo == "" {
		m.mu.Unlock()
		return jobView{}, errors.New("esse review não foi salvo em disco — não há o que publicar")
	}
	// Já publicando este mesmo review: devolve o job que está em curso.
	for _, other := range m.order {
		j := m.jobs[other]
		if j.publish != nil && j.publish.From == id && (j.State == StateQueued || j.State == StateRunning) {
			v := j.view(false)
			m.mu.Unlock()
			return v, nil
		}
	}

	choice := m.cfg.PostChoice()
	m.seq++
	job := &Job{
		ID:     fmt.Sprintf("j%d", m.seq),
		PR:     src.PR,
		Mine:   src.Mine,
		Choice: choice,
		Steps:  []jobStep{{Name: choice.Steps[0].Name, State: StateQueued}},
		publish: &publishInput{
			Path: src.SavedTo,
			Body: src.Result.Body,
			From: id,
		},
		State:    StateQueued,
		QueuedAt: time.Now(),
	}
	m.jobs[job.ID] = job
	m.order = append(m.order, job.ID)
	m.trimLocked()
	m.mu.Unlock()

	select {
	case m.queue <- job:
	default:
		m.finish(job, StateFailed, "fila cheia — espere os reviews em andamento")
		return m.mustView(job.ID), errors.New("fila cheia")
	}
	m.publish(job)
	return m.mustView(job.ID), nil
}

// Post publica o review como comentário no PR.
func (m *Manager) Post(ctx context.Context, id string) (jobView, error) {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return jobView{}, fmt.Errorf("job %q não existe", id)
	}
	if job.State != StateDone {
		m.mu.Unlock()
		return jobView{}, fmt.Errorf("esse review não terminou")
	}
	if job.Posted {
		m.mu.Unlock()
		return jobView{}, fmt.Errorf("esse review já foi publicado")
	}
	res := job.Result
	m.mu.Unlock()

	err := gh.Comment(ctx, res.PR.Repo, res.PR.Number, store.CommentBody(res))

	if err == nil {
		_ = store.MarkPosted(m.reviewsDir, res.PR.Key())
	}

	m.mu.Lock()
	if err != nil {
		job.PostErr = err.Error()
	} else {
		job.Posted = true
		job.PostErr = ""
	}
	m.mu.Unlock()
	m.publish(job)
	return m.mustView(id), err
}

// View serializa um job. Os workers mexem nos jobs o tempo todo, então nada
// de *Job sai daqui — quem lê, lê uma cópia feita sob o lock.
func (m *Manager) View(id string, withBody bool) (jobView, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return jobView{}, false
	}
	return job.view(withBody), true
}

func (m *Manager) mustView(id string) jobView {
	v, _ := m.View(id, false)
	return v
}

// Snapshot devolve os jobs do mais novo para o mais velho.
func (m *Manager) Snapshot() []jobView {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]jobView, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- {
		out = append(out, m.jobs[m.order[i]].view(false))
	}
	return out
}

// Active conta os reviews esperando ou rodando.
func (m *Manager) Active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, id := range m.order {
		if s := m.jobs[id].State; s == StateQueued || s == StateRunning {
			n++
		}
	}
	return n
}

func (m *Manager) publish(job *Job) {
	m.mu.Lock()
	view := job.view(false)
	m.mu.Unlock()
	m.hub.Broadcast("job", view)
}
