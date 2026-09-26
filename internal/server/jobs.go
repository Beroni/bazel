package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/beroni/bazel/internal/agent"
	"github.com/beroni/bazel/internal/config"
	"github.com/beroni/bazel/internal/gh"
	"github.com/beroni/bazel/internal/store"
	"github.com/beroni/bazel/internal/workspace"
)

// State é o ciclo de vida de um review na fila.
type State string

const (
	StateQueued   State = "queued"
	StateRunning  State = "running"
	StateDone     State = "done"
	StateFailed   State = "failed"
	StateCanceled State = "canceled"
	// StatePaused é a pipeline que chegou num passo `pause`: rodou o que vinha
	// antes, o clone continua de pé e ela espera você ler e mandar seguir. Não
	// é um fim — o job volta para a fila quando você continua.
	StatePaused State = "paused"
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

	// publish, quando preenchido, diz que este job está levando um review ao
	// PR em vez de produzir um: o agente pega o review já lido e o põe lá.
	// Acontece de duas formas — no card do próprio review, depois de o
	// usuário lê-lo (pubChoice é o agente de post e stepBase diz onde os
	// passos dele entram em Steps), ou num card só dele, quando o review veio
	// do disco e não há job de origem nenhum.
	publish   *publishInput
	pubChoice config.Choice
	stepBase  int

	// keptDir é o clone que sobreviveu a uma parada, e cont é por onde a
	// pipeline recomeça. Enquanto o job está pausado o Manager é o dono dessa
	// pasta: quem abandona o job leva o clone junto.
	keptDir string
	cont    *agent.Cont

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
}

// publishInputFor monta o que o agente de post recebe. Sem achado desmarcado e
// com o relatório inteiro publicável é o review como está; se algum achado saiu
// ou se a pipeline tinha passo que não vai ao PR, o agente ganha uma cópia do
// arquivo já aparado em publish/, porque a skill de post lê o arquivo, não o
// prompt.
//
// trimmed diz que o corpo recebido já não é o que está em path — é o que
// obriga a cópia mesmo sem achado desmarcado nenhum.
func (m *Manager) publishInputFor(path, body string, skip []int, trimmed bool) (*publishInput, error) {
	if len(skip) == 0 && !trimmed {
		return &publishInput{Path: path, Body: body}, nil
	}
	if len(skip) > 0 {
		body = store.DropFindings(body, skip)
	}
	copyPath, err := store.SavePublishCopy(m.reviewsDir, path, body)
	if err != nil {
		return nil, fmt.Errorf("could not write the review without the unticked findings: %w", err)
	}
	return &publishInput{Path: copyPath, Body: body}, nil
}

// inPlacePublish diz que a publicação está rodando dentro do card do review
// que a originou: os passos do agente de post vêm depois dos do review, e o
// que ele escreve não substitui o review que está na tela.
func (j *Job) inPlacePublish() bool { return j.publish != nil && j.stepBase > 0 }

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

	// keep repete o --keep: com ele ligado o clone de um review sobrevive ao
	// fim dele, e o Manager não pode apagar o que a pessoa pediu para guardar.
	keep bool

	// limits é a última leitura da cota do Claude. Não pertence a job
	// nenhum: é da máquina, e vale para a página inteira.
	limitsMu sync.RWMutex
	limits   agent.Limits
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
	runner.PricingTable = &cfg.Agent.Pricing

	m := &Manager{
		cfg:        cfg,
		runner:     runner,
		reviewsDir: reviewsDir,
		hub:        hub,
		ctx:        ctx,
		queue:      make(chan *Job, 256),
		jobs:       map[string]*Job{},
		keep:       keep,
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
			(j.State == StateQueued || j.State == StateRunning || j.State == StatePaused) {
			v := j.view(false)
			m.mu.Unlock()
			return v, nil
		}
	}
	m.seq++
	steps := make([]jobStep, 0, len(choice.Steps))
	for _, st := range choice.Steps {
		// O passo `publish` não ganha linha própria: quem aparece na tela é o
		// agente de publicação, que entra no fim da lista quando chega a vez
		// dele — e é o mesmo caminho do botão de publicar.
		if st.Reserved == config.StepPublish {
			continue
		}
		steps = append(steps, jobStep{Name: st.Name, State: StateQueued})
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
		m.finish(job, StateFailed, "queue is full — wait for the reviews in flight")
		return m.mustView(job.ID), fmt.Errorf("queue is full")
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
	// Retomando, o relógio é o do review inteiro: a pausa é tempo de gente
	// lendo, e zerar aqui faria a pipeline parecer mais rápida do que foi.
	if job.StartedAt.IsZero() {
		job.StartedAt = time.Now()
	}
	job.cancel = cancel
	cont := job.cont
	m.mu.Unlock()
	m.publish(job)

	onEvent := func(e agent.Event) { m.applyEvent(job, e) }
	var (
		res agent.Result
		err error
	)
	switch {
	case job.publish != nil:
		res, err = m.runner.Publish(ctx, job.PR, job.pubChoice, job.publish.Path, job.publish.Body, onEvent)
	case cont != nil:
		res, err = m.runner.Continue(ctx, job.PR, job.Choice, *cont, onEvent)
	default:
		res, err = m.runner.Review(ctx, job.PR, job.Choice, onEvent)
	}
	if err != nil {
		// Publicando dentro do card do review, quem falhou foi a publicação:
		// o review continua lido, salvo e publicável de novo, e apagá-lo da
		// tela custaria justamente o que o usuário quer.
		if job.inPlacePublish() {
			msg := err.Error()
			if ctx.Err() != nil {
				msg = "publishing canceled"
			}
			m.failPublish(job, msg)
			return
		}
		state := StateFailed
		if ctx.Err() != nil {
			state = StateCanceled
		}
		m.finish(job, state, err.Error())
		return
	}

	// Relatório colado num bloco ```markdown sai do bloco antes de qualquer
	// outra coisa: dentro dele os headings são texto, e a tela, o disco e o PR
	// mostrariam o markdown cru em vez do review.
	//
	// Passo a passo, e não só no corpo final, porque o corpo de cada passo é
	// reaproveitado depois: ele volta no `joinSteps` quando uma pipeline parada
	// continua, e é dele que sai o que vai ao PR quando só parte dos passos é
	// publicável.
	desembrulha(&res)

	// Parou num `pause`: o clone fica, o worker sai, e o job espera você ler o
	// que saiu e mandar continuar.
	if res.StoppedAt >= 0 && job.Choice.Steps[res.StoppedAt].Reserved == config.StepPause {
		m.pauseJob(job, res)
		return
	}

	// O relatório de uma publicação não vira arquivo: o review já está salvo,
	// e este job só conta o que foi para o PR.
	var (
		path    string
		saveErr error
	)
	if job.publish == nil {
		path, saveErr = store.Save(m.reviewsDir, res, &m.cfg.Agent.Pricing)
	}

	// O PR entra no índice: é o ✓ da lista, e é o commit gravado aqui que
	// depois denuncia mudança no PR depois do review.
	if job.publish == nil {
		_ = store.MarkReviewed(m.reviewsDir, res, path)
	} else {
		_ = store.MarkPosted(m.reviewsDir, res.PR.Key())
	}

	m.mu.Lock()
	if job.inPlacePublish() {
		// O relatório do agente de post não toma o lugar do review: o card
		// continua mostrando o que o usuário leu, agora marcado como
		// publicado e com o gasto das duas rodadas somado.
		job.Posted = true
		job.PostErr = ""
		job.Result.Usage = job.Result.Usage.Plus(res.Usage)
		job.publish = nil
		job.Live = agent.Usage{}
	} else {
		job.Result = res
		job.SavedTo = path
	}
	if saveErr != nil {
		job.Err = "review done, but I could not save it to disk: " + saveErr.Error()
	}
	job.State = StateDone
	job.FinishedAt = time.Now()
	job.Cloning = false
	job.cancel = nil
	job.cont = nil
	// A pipeline acabou: o clone que ela vinha carregando entre as paradas já
	// não serve a ninguém.
	publicar := res.StoppedAt >= 0 && job.Choice.Steps[res.StoppedAt].Reserved == config.StepPublish
	m.discardCloneLocked(job)
	m.mu.Unlock()
	m.publish(job)

	// Passo `publish`: é o mesmo caminho do botão — o agente de publicação
	// entra no fim deste card com o relatório que acabou de ser salvo. A
	// diferença é só quem apertou, e por isso ele vem sempre depois de um
	// `pause`, com você tendo lido.
	if publicar && saveErr == nil {
		if _, err := m.PublishWithAgent(job.ID, nil); err != nil {
			m.mu.Lock()
			job.PostErr = err.Error()
			m.mu.Unlock()
			m.publish(job)
		}
	}
}

// desembrulha tira o relatório de dentro de um bloco ```markdown — o corpo e o
// de cada passo. O agente às vezes devolve o relatório inteiro colado num bloco
// de código, e aí nada dele renderiza: a página mostra o markdown cru.
func desembrulha(res *agent.Result) {
	res.Body = store.Unwrap(res.Body)
	for i := range res.Steps {
		res.Steps[i].Body = store.Unwrap(res.Steps[i].Body)
	}
}

// pauseJob guarda o que já rodou e para o job, liberando o worker. O clone não
// é apagado: é dentro dele que o resto da pipeline continua, e clonar de novo
// daria outro código — os passos já corridos falariam de outro repositório.
func (m *Manager) pauseJob(job *Job, res agent.Result) {
	m.mu.Lock()
	job.Result = res
	job.keptDir = res.Workdir
	job.cont = &agent.Cont{From: res.StoppedAt + 1, Workdir: res.Workdir, Steps: res.Steps}
	if res.StoppedAt < len(job.Steps) {
		job.Steps[res.StoppedAt].State = StatePaused
	}
	job.State = StatePaused
	job.Cloning = false
	job.cancel = nil
	job.Live = agent.Usage{}
	m.mu.Unlock()
	m.publish(job)
}

// Continue solta uma pipeline parada: ela volta para a fila e recomeça do passo
// seguinte ao `pause`, dentro do mesmo clone.
func (m *Manager) Continue(id string) (jobView, error) {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return jobView{}, fmt.Errorf("job %q does not exist", id)
	}
	if job.State != StatePaused {
		m.mu.Unlock()
		return jobView{}, errors.New("that review is not waiting on you")
	}
	if job.cont == nil {
		m.mu.Unlock()
		return jobView{}, errors.New("that review has nothing left to run")
	}
	// A linha da pausa deixa de estar esperando: quem espera agora é a fila.
	if i := job.cont.From - 1; i >= 0 && i < len(job.Steps) {
		job.Steps[i].State = StateDone
	}
	job.State = StateQueued
	job.QueuedAt = time.Now()
	job.Err = ""
	m.mu.Unlock()

	select {
	case m.queue <- job:
	default:
		m.mu.Lock()
		job.State = StatePaused
		m.mu.Unlock()
		return m.mustView(id), errors.New("queue is full — wait for the reviews in flight")
	}
	m.publish(job)
	return m.mustView(id), nil
}

// discardCloneLocked apaga o clone que uma parada deixou de pé. Com --keep
// ligado ele fica: guardar o clone foi o que a pessoa pediu na linha de
// comando. Chamar com o lock do Manager seguro.
func (m *Manager) discardCloneLocked(job *Job) {
	if job.keptDir == "" {
		return
	}
	dir := job.keptDir
	job.keptDir = ""
	if m.keep {
		return
	}
	(&workspace.Workspace{Dir: dir}).Remove()
}

// failPublish devolve ao card o review que ele já tinha: a publicação falhou
// ou foi cancelada, mas o review continua na tela, salvo em disco e publicável
// de novo. Os passos do agente de post ficam como estão — é neles que se vê
// onde a publicação quebrou.
func (m *Manager) failPublish(job *Job, msg string) {
	m.mu.Lock()
	m.failPublishLocked(job, msg)
	m.mu.Unlock()
	m.publish(job)
}

// failPublishLocked é o failPublish para quem já está com o lock na mão.
func (m *Manager) failPublishLocked(job *Job, msg string) {
	for i := job.stepBase; i < len(job.Steps); i++ {
		if st := &job.Steps[i]; st.State == StateQueued || st.State == StateRunning {
			st.State = StateFailed
		}
	}
	job.PostErr = msg
	job.publish = nil
	job.Live = agent.Usage{}
	job.State = StateDone
	job.FinishedAt = time.Now()
	job.Cloning = false
	job.cancel = nil
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
		job.appendLog(e.Index+job.stepBase, e.Agent, e.Stream, e.Text)
		m.mu.Unlock()
		return
	}

	// A cota do Claude não é do job: chega pelo stream dele, mas vale para a
	// máquina toda e vai para a página por um aviso próprio.
	if e.Kind == agent.EventLimits {
		m.limitsMu.Lock()
		m.limits = e.Limits
		m.limitsMu.Unlock()
		m.hub.Broadcast("limits", e.Limits)
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
	// O runner conta os passos da rodada dele a partir do zero; publicando
	// dentro do card do review, essa rodada começa depois dos passos que já
	// estão lá — stepBase é o que põe o evento na linha certa.
	i := e.Index + job.stepBase
	switch {
	case e.Kind == agent.EventClone:
		job.Cloning = true
	case e.Index < 0 || i >= len(job.Steps):
		// Evento de um passo que não existe: nada a mostrar.
	case e.Kind == agent.EventStep:
		job.Cloning = false
		job.Steps[i].State = StateRunning
		job.Steps[i].StartedAt = time.Now()
	case e.Kind == agent.EventStepDone:
		st := &job.Steps[i]
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
	job.cont = nil
	// Falhou ou foi cancelado: o clone que a parada segurava não espera mais
	// ninguém.
	m.discardCloneLocked(job)
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
		// Pausado nada escreve — a página para de pedir e volta a pedir
		// sozinha quando o job sai da pausa e republica.

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
		return fmt.Errorf("job %q does not exist", id)
	}
	cancel := job.cancel
	vivo := job.State == StateQueued || job.State == StateRunning
	if vivo {
		// O worker só descobre o cancelamento depois; marcar aqui impede que
		// ele republique um job que a página já esqueceu.
		job.State = StateCanceled
		job.FinishedAt = time.Now()
	}
	// Tirar da tela uma pipeline parada é abandoná-la: o clone que ela
	// segurava sai junto, senão fica uma pasta temporária órfã por review.
	m.discardCloneLocked(job)
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
		return fmt.Errorf("job %q does not exist", id)
	}
	switch job.State {
	case StateQueued:
		// Cancelar uma publicação que espera na fila não cancela o review que
		// já está no card: ele volta a ser um review terminado. O worker vê o
		// estado mudado e nem chega a rodar o agente de post.
		if job.inPlacePublish() {
			m.failPublishLocked(job, "publishing canceled")
			m.mu.Unlock()
			m.publish(job)
			return nil
		}
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
	case StatePaused:
		// Parada, não há processo para matar: cancelar é desistir do resto da
		// pipeline. O que já rodou continua na tela; o clone, não.
		job.State = StateCanceled
		job.FinishedAt = time.Now()
		job.cont = nil
		m.discardCloneLocked(job)
		m.mu.Unlock()
		m.publish(job)
		return nil
	default:
		m.mu.Unlock()
		return fmt.Errorf("that review has already finished")
	}
}

// PublishWithAgent manda o agente de post levar ao PR o review que o usuário
// acabou de ler. É o outro caminho para o GitHub: o comentário simples do Post
// é o Bazel escrevendo; este é o agente publicando um review com comentários
// inline.
//
// Roda no card do próprio review: os passos do agente de post entram depois
// dos do review e o card volta a rodar. Publicar não é um trabalho à parte —
// é o fim do review — e um segundo card só espalharia o mesmo PR por dois
// lugares da fila.
//
// skip são os achados que o usuário desmarcou na tela: eles saem do que vai
// ao PR, e o agente recebe uma cópia do review sem eles — o arquivo salvo
// continua inteiro.
func (m *Manager) PublishWithAgent(id string, skip []int) (jobView, error) {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return jobView{}, fmt.Errorf("job %q does not exist", id)
	}
	// Já publicando: devolve o card como está, em vez de mandar o agente de
	// post ao mesmo PR duas vezes.
	if job.publish != nil && (job.State == StateQueued || job.State == StateRunning) {
		v := job.view(false)
		m.mu.Unlock()
		return v, nil
	}
	if job.State != StateDone {
		m.mu.Unlock()
		return jobView{}, errors.New("that review has not finished")
	}
	if job.SavedTo == "" {
		m.mu.Unlock()
		return jobView{}, errors.New("that review was not saved to disk — there is nothing to publish")
	}
	// Agente cuja saída não é review não tem caminho para o PR. A interface
	// já esconde o botão; isto é o que segura um POST vindo de outro lugar.
	if !job.Choice.Publishable {
		m.mu.Unlock()
		return jobView{}, fmt.Errorf("what %q produces does not go to the PR", job.Choice.Name)
	}
	choice := m.cfg.PostChoice()
	if len(choice.Steps) == 0 {
		m.mu.Unlock()
		return jobView{}, config.ErrNoAgents
	}
	body, trimmed, err := agent.PublishableBody(job.Result, job.Choice)
	if err != nil {
		m.mu.Unlock()
		return jobView{}, err
	}
	in, err := m.publishInputFor(job.SavedTo, body, skip, trimmed)
	if err != nil {
		m.mu.Unlock()
		return jobView{}, err
	}

	// Uma publicação anterior que falhou deixou os passos dela no card; a
	// tentativa nova os refaz, não os empilha.
	if job.stepBase > 0 {
		job.Steps = job.Steps[:job.stepBase]
	} else {
		job.stepBase = len(job.Steps)
	}
	for _, name := range choice.StepNames() {
		job.Steps = append(job.Steps, jobStep{Name: name, State: StateQueued})
	}
	job.publish = in
	job.pubChoice = choice
	job.State = StateQueued
	job.QueuedAt = time.Now()
	job.StartedAt = time.Time{}
	job.FinishedAt = time.Time{}
	job.Err = ""
	job.PostErr = ""
	job.Live = agent.Usage{}
	m.mu.Unlock()

	select {
	case m.queue <- job:
	default:
		m.failPublish(job, "queue is full — wait for the reviews in flight")
		return m.mustView(job.ID), errors.New("queue is full")
	}
	m.publish(job)
	return m.mustView(job.ID), nil
}

// PublishSaved leva ao PR um review que está em disco — o de uma sessão
// anterior, aberto na aba dos salvos. É o mesmo caminho do PublishWithAgent,
// mas partindo do arquivo: um review sobrevive ao servidor, e a chance de
// publicá-lo tem de sobreviver junto.
func (m *Manager) PublishSaved(pr gh.PR, mine bool, path, body string, skip []int) (jobView, error) {
	if strings.TrimSpace(path) == "" {
		return jobView{}, errors.New("no review file to publish")
	}
	in, err := m.publishInputFor(path, body, skip, false)
	if err != nil {
		return jobView{}, err
	}
	m.mu.Lock()
	// Já publicando este mesmo arquivo: devolve o job que está em curso.
	for _, id := range m.order {
		j := m.jobs[id]
		if j.publish != nil && j.publish.Path == path && (j.State == StateQueued || j.State == StateRunning) {
			v := j.view(false)
			m.mu.Unlock()
			return v, nil
		}
	}

	choice := m.cfg.PostChoice()
	m.seq++
	job := &Job{
		ID:        fmt.Sprintf("j%d", m.seq),
		PR:        pr,
		Mine:      mine,
		Choice:    choice,
		Steps:     []jobStep{{Name: choice.Steps[0].Name, State: StateQueued}},
		publish:   in,
		pubChoice: choice,
		State:     StateQueued,
		QueuedAt:  time.Now(),
	}
	m.jobs[job.ID] = job
	m.order = append(m.order, job.ID)
	m.trimLocked()
	m.mu.Unlock()

	select {
	case m.queue <- job:
	default:
		m.finish(job, StateFailed, "queue is full — wait for the reviews in flight")
		return m.mustView(job.ID), errors.New("queue is full")
	}
	m.publish(job)
	return m.mustView(job.ID), nil
}

// CommentSaved cola no PR um review que está em disco, sem agente nenhum. É o
// caminho barato do arquivo para o GitHub.
func (m *Manager) CommentSaved(ctx context.Context, pr gh.PR, body string, skip []int) error {
	body = store.DropFindings(body, skip)
	if strings.TrimSpace(body) == "" {
		return errors.New("that review has no body to publish")
	}
	err := gh.Comment(ctx, pr.Repo, pr.Number, store.CommentBody(agent.Result{PR: pr, Body: body}))
	if err == nil {
		_ = store.MarkPosted(m.reviewsDir, pr.Key())
	}
	return err
}

// Post publica o review como comentário no PR, sem os achados em skip.
func (m *Manager) Post(ctx context.Context, id string, skip []int) (jobView, error) {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return jobView{}, fmt.Errorf("job %q does not exist", id)
	}
	if job.State != StateDone {
		m.mu.Unlock()
		return jobView{}, fmt.Errorf("that review has not finished")
	}
	if job.Posted {
		m.mu.Unlock()
		return jobView{}, fmt.Errorf("that review was already published")
	}
	if !job.Choice.Publishable {
		m.mu.Unlock()
		return jobView{}, fmt.Errorf("what %q produces does not go to the PR", job.Choice.Name)
	}
	res := job.Result
	// Colar como comentário é o mesmo destino por outra porta: da pipeline vai
	// só o que é publicável, igual ao caminho do agente de post.
	body, _, err := agent.PublishableBody(res, job.Choice)
	if err != nil {
		m.mu.Unlock()
		return jobView{}, err
	}
	res.Body = store.DropFindings(body, skip)
	m.mu.Unlock()

	err = gh.Comment(ctx, res.PR.Repo, res.PR.Number, store.CommentBody(res))

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

// Limits é a última leitura da cota do Claude, vazia até o primeiro review
// desta sessão — ela só chega no stream de um agente rodando.
func (m *Manager) Limits() agent.Limits {
	m.limitsMu.RLock()
	defer m.limitsMu.RUnlock()
	return m.limits
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
