// Package agent executa os agentes de IA configurados sobre um pull request.
package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/beroni/bazel/internal/config"
	"github.com/beroni/bazel/internal/gh"
	"github.com/beroni/bazel/internal/workspace"
)

// EventKind é o tipo de aviso que o Runner emite durante um review.
type EventKind string

const (
	// EventClone é disparado antes de clonar o repositório do PR.
	EventClone EventKind = "clone"
	// EventStep marca o início de um passo.
	EventStep EventKind = "step"
	// EventStepDone marca o fim de um passo, com ou sem erro.
	EventStepDone EventKind = "step_done"
	// EventLog é uma linha que o agente escreveu enquanto rodava.
	EventLog EventKind = "log"
	// EventUsage é o gasto parcial do review, atualizado enquanto o agente
	// trabalha. Só quem fala stream-json emite isto.
	EventUsage EventKind = "usage"
	// EventLimits é a cota do Claude, que vem de carona no stream de quem
	// está rodando.
	EventLimits EventKind = "limits"
)

// Event é o andamento de um review. É o que permite a TUI e a interface web
// mostrarem qual agente está rodando agora em vez de um spinner cego.
type Event struct {
	Kind  EventKind
	Index int // posição do passo, começando em 0
	Total int
	Name  string
	// Duration só vem preenchida no EventStepDone.
	Duration time.Duration
	Err      error
	// Text e Stream ("stdout" ou "stderr") só vêm no EventLog. Agent é quem
	// escreveu a linha: o agente do passo ou um sub-agente que ele disparou.
	Text   string
	Stream string
	Agent  string
	// Usage só vem no EventUsage: o gasto do review inteiro até agora,
	// passos anteriores incluídos.
	Usage Usage
	// Limits só vem no EventLimits.
	Limits Limits
}

// StepResult é a saída de um passo do review.
type StepResult struct {
	Name     string
	Body     string
	Duration time.Duration
	// Usage é o que este passo gastou de modelo.
	Usage Usage
	Err   error
}

// Result é a saída de um review.
type Result struct {
	PR gh.PR
	// Agent é o nome do agente ou pipeline que rodou.
	Agent string
	// Posts diz se esse agente publica no PR por conta própria — quem for
	// publicar de novo por cima precisa saber.
	Posts bool
	// Body é o relatório: a saída do passo único, ou os passos concatenados.
	Body  string
	Steps []StepResult
	// Usage é o gasto do review inteiro: a soma dos passos.
	Usage     Usage
	Duration  time.Duration
	Truncated bool
	// Workdir é o clone temporário onde os agentes rodaram. Vazio quando o
	// checkout está desligado; só sobrevive ao review com KeepWorkspace.
	Workdir string
}

// Runner roda os agentes configurados.
type Runner struct {
	cfg *config.Config
	// KeepWorkspace preserva o clone temporário depois do review.
	KeepWorkspace bool
}

// New cria um Runner.
func New(cfg *config.Config) *Runner { return &Runner{cfg: cfg} }

// Review prepara o material do PR e executa a escolha do usuário — um agente
// sozinho ou uma pipeline inteira, sempre sobre o mesmo clone.
//
// Com checkout ligado, o repositório é clonado numa pasta temporária com o PR
// em checkout e os agentes rodam lá dentro — é o que permite usar um agente
// que navega no código (a skill review-fleet, por exemplo) em vez de só ler um
// diff colado no prompt.
//
// onEvent, se não for nil, recebe o andamento passo a passo. É chamada da
// goroutine do review, então quem escuta não pode bloquear nela.
func (r *Runner) Review(ctx context.Context, pr gh.PR, choice config.Choice, onEvent func(Event)) (Result, error) {
	return r.run(ctx, pr, choice, nil, onEvent)
}

// Publish roda o agente de publicação sobre um review que já está pronto — o
// que você leu na tela. O caminho do arquivo e o texto entram no prompt pelos
// placeholders {{review_file}} e {{review}}; o agente não refaz o review,
// publica esse.
func (r *Runner) Publish(ctx context.Context, pr gh.PR, choice config.Choice, reviewPath, reviewBody string, onEvent func(Event)) (Result, error) {
	return r.run(ctx, pr, choice, map[string]string{
		"{{review_file}}": reviewPath,
		"{{review}}":      reviewBody,
	}, onEvent)
}

func (r *Runner) run(ctx context.Context, pr gh.PR, choice config.Choice, extra map[string]string, onEvent func(Event)) (Result, error) {
	start := time.Now()
	emit := func(e Event) {
		if onEvent != nil {
			onEvent(e)
		}
	}

	if len(choice.Steps) == 0 {
		choice = r.cfg.DefaultChoice()
	}
	if len(choice.Steps) == 0 {
		return Result{}, config.ErrNoAgents
	}
	total := len(choice.Steps)

	var workdir string
	if choice.NeedsCheckout() {
		emit(Event{Kind: EventClone, Total: total, Name: pr.Key()})
		ws, err := workspace.Prepare(ctx, pr)
		if err != nil {
			return Result{}, err
		}
		ws.Keep = r.KeepWorkspace
		defer ws.Cleanup()
		workdir = ws.Dir
	}

	diff, truncated, err := r.material(ctx, pr, choice)
	if err != nil {
		return Result{}, err
	}

	steps := make([]StepResult, 0, total)
	// fechado é o gasto dos passos que já terminaram: o parcial de um passo
	// em curso é somado a ele, para o número na tela ser o do review todo.
	var fechado Usage
	for i, step := range choice.Steps {
		emit(Event{Kind: EventStep, Index: i, Total: total, Name: step.Name})

		dir := workdir
		if !step.Checkout {
			dir = "" // herda o diretório atual
		}
		stepStart := time.Now()
		body, used, err := r.exec(ctx, step, buildPrompt(step.Prompt, step.Task, pr, diff, workdir, extra), dir,
			func(stream, who, text string) {
				if who == "" {
					who = step.Name
				}
				emit(Event{Kind: EventLog, Index: i, Total: total, Name: step.Name,
					Stream: stream, Agent: who, Text: text})
			},
			func(parcial Usage) {
				total := fechado
				total.add(parcial)
				emit(Event{Kind: EventUsage, Index: i, Total: total.Total(), Name: step.Name, Usage: total})
			},
			func(l Limits) {
				emit(Event{Kind: EventLimits, Index: i, Name: step.Name, Limits: l})
			})
		res := StepResult{Name: step.Name, Body: strings.TrimSpace(body), Duration: time.Since(stepStart), Usage: used, Err: err}
		if err == nil && res.Body == "" {
			res.Err = fmt.Errorf("the agent `%s` returned nothing", step.Command)
		}
		fechado.add(used)
		steps = append(steps, res)
		emit(Event{Kind: EventStepDone, Index: i, Total: total, Name: step.Name, Duration: res.Duration, Err: res.Err})

		// Cancelamento aborta a fila inteira: o clone já era, e insistir nos
		// próximos passos só queima tempo de um review que ninguém quer mais.
		if errors.Is(res.Err, context.Canceled) || ctx.Err() != nil {
			return Result{}, context.Canceled
		}
	}

	body, err := joinSteps(steps)
	if err != nil {
		return Result{}, err
	}

	var used Usage
	for _, s := range steps {
		used.add(s.Usage)
	}

	return Result{
		PR:        pr,
		Agent:     choice.Name,
		Posts:     choice.Posts,
		Body:      body,
		Steps:     steps,
		Usage:     used,
		Duration:  time.Since(start),
		Truncated: truncated,
		Workdir:   workdir,
	}, nil
}

// material baixa o que o review precisa antes de rodar qualquer passo. O diff
// só entra no prompt se algum molde pedir: com o clone em mãos, o agente lê o
// que precisar e mandar centenas de KB junto só queima contexto.
func (r *Runner) material(ctx context.Context, pr gh.PR, choice config.Choice) (string, bool, error) {
	wantsDiff := false
	for _, step := range choice.Steps {
		if strings.Contains(step.Prompt, "{{diff}}") {
			wantsDiff = true
			break
		}
	}
	if !wantsDiff {
		if pr.ChangedFiles == 0 {
			return "", false, fmt.Errorf("%s has no changed files — nothing to review", pr.Key())
		}
		return "", false, nil
	}

	diff, truncated, err := gh.Diff(ctx, pr.Repo, pr.Number, r.cfg.MaxDiffBytes)
	if err != nil {
		return "", false, fmt.Errorf("baixando diff de %s: %w", pr.Key(), err)
	}
	if strings.TrimSpace(diff) == "" {
		return "", false, fmt.Errorf("%s has no diff — nothing to review", pr.Key())
	}
	if truncated {
		diff += "\n\n[... diff truncado em " + strconv.Itoa(r.cfg.MaxDiffBytes) + " bytes ...]"
	}
	return diff, truncated, nil
}

// joinSteps monta o relatório final. Um passo só sai cru, como sempre saiu;
// vários viram seções. Todos falharem é o review inteiro falhando.
func joinSteps(steps []StepResult) (string, error) {
	var ok int
	for _, s := range steps {
		if s.Err == nil {
			ok++
		}
	}
	if ok == 0 {
		errs := make([]error, 0, len(steps))
		for _, s := range steps {
			if s.Err != nil {
				errs = append(errs, s.Err)
			}
		}
		if len(errs) == 0 {
			return "", errors.New("no agent ran")
		}
		return "", errors.Join(errs...)
	}
	if len(steps) == 1 {
		return steps[0].Body, nil
	}

	var b strings.Builder
	for i, s := range steps {
		if i > 0 {
			b.WriteString("\n\n---\n\n")
		}
		fmt.Fprintf(&b, "## %s\n\n", s.Name)
		if s.Err != nil {
			fmt.Fprintf(&b, "> ✗ failed: %s\n", firstLine(s.Err.Error()))
			continue
		}
		b.WriteString(s.Body + "\n")
	}
	return strings.TrimSpace(b.String()), nil
}

// exec roda um agente e vai entregando o que ele escreve, linha a linha, para
// onLog. É o que alimenta o log ao vivo: com o stdout num buffer ninguém veria
// nada até o processo morrer.
//
// No modo stream-json o stdout é JSONL de eventos — o log recebe a versão
// legível e o relatório sai do evento final. Em qualquer outro agente o stdout
// é o próprio relatório, e vai cru para os dois.
// O gasto de modelo sai junto do relatório — quem lê a saída é quem sabe
// contá-lo — e vai saindo pelo caminho por onUsage, que é o que faz a conta
// andar na tela em vez de aparecer só no fim.
func (r *Runner) exec(ctx context.Context, step config.ResolvedAgent, prompt, workdir string, onLog func(stream, agent, text string), onUsage func(Usage), onLimits func(Limits)) (string, Usage, error) {
	if step.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(step.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	if _, err := exec.LookPath(step.Command); err != nil {
		return "", Usage{}, fmt.Errorf("agent `%s` not found in PATH", step.Command)
	}

	cmd := exec.CommandContext(ctx, step.Command, step.Args...)
	cmd.Dir = workdir // vazio = herda o diretório atual
	cmd.Stdin = strings.NewReader(prompt)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", Usage{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", Usage{}, err
	}
	if err := cmd.Start(); err != nil {
		return "", Usage{}, fmt.Errorf("agent `%s` did not start: %w", step.Command, err)
	}

	log := func(stream, who, text string) {
		if onLog != nil && text != "" {
			onLog(stream, who, text)
		}
	}

	var (
		raw    strings.Builder
		parser streamParser
		stream = isStreamJSON(step.Args)
		wg     sync.WaitGroup
		errBuf lastLines
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		visto := 0
		var cota time.Time
		eachLine(stdout, func(line string) {
			if !stream {
				raw.WriteString(line)
				raw.WriteByte('\n')
				log("stdout", "", line)
				return
			}
			for _, out := range parser.line(line) {
				log("stdout", out.Agent, out.Text)
			}
			// A conta só sobe quando muda: uma chamada de ferramenta não
			// gasta token nenhum e não precisa acordar o navegador.
			if gasto := parser.spend(); onUsage != nil && gasto.Total() != visto {
				visto = gasto.Total()
				onUsage(gasto)
			}
			if onLimits != nil && parser.limits.At.After(cota) {
				cota = parser.limits.At
				onLimits(parser.limits)
			}
		})
	}()
	go func() {
		defer wg.Done()
		eachLine(stderr, func(line string) {
			errBuf.add(line)
			log("stderr", "", line)
		})
	}()

	// Os pipes têm de chegar ao EOF antes do Wait: ele os fecha.
	wg.Wait()
	err = cmd.Wait()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", Usage{}, fmt.Errorf("`%s` blew past its %ds timeout", step.Name, step.TimeoutSeconds)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return "", Usage{}, context.Canceled
	}
	if err != nil {
		msg := errBuf.String()
		if msg == "" {
			msg = err.Error()
		}
		return "", Usage{}, fmt.Errorf("agent `%s` failed: %s", step.Command, msg)
	}
	if stream {
		return parser.report(), parser.spend(), nil
	}
	return raw.String(), Usage{}, nil
}

// eachLine chama fn para cada linha lida, sem limite de tamanho — um evento do
// stream-json com um resultado de ferramenta grande estoura o bufio.Scanner.
func eachLine(r io.Reader, fn func(string)) {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadString('\n')
		if line = strings.TrimRight(line, "\r\n"); line != "" {
			fn(line)
		}
		if err != nil {
			return
		}
	}
}

// lastLines guarda o fim do stderr, que é onde costuma estar a mensagem que
// explica a falha.
type lastLines struct {
	lines []string
}

func (l *lastLines) add(line string) {
	l.lines = append(l.lines, line)
	if len(l.lines) > 5 {
		l.lines = l.lines[1:]
	}
}

func (l *lastLines) String() string {
	return strings.TrimSpace(strings.Join(l.lines, "\n"))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// buildPrompt substitui os placeholders do molde pelo conteúdo do PR.
// workdir é o clone temporário, ou vazio se o checkout estiver desligado.
//
// task é a instrução da lente: entra no {{task}} do molde. Molde sem {{task}}
// — um prompt customizado escrito antes dos agents nomeados — recebe a
// instrução na primeira linha, que é onde ela estava quando o molde era um só.
// extra traz os placeholders de um review já pronto, que só o agente de
// publicação usa.
func buildPrompt(tmpl, task string, pr gh.PR, diff, workdir string, extra map[string]string) string {
	body := strings.TrimSpace(pr.Body)
	if body == "" {
		body = "(no description)"
	}
	if workdir == "" {
		workdir = "(no local clone)"
	}
	// A task entra no molde antes da substituição, e não como mais um par do
	// Replacer: ela também tem placeholders ("/review-fleet {{number}}"), e o
	// Replacer não reexpande o que acabou de inserir.
	task = strings.TrimSpace(task)
	if task != "" && !strings.Contains(tmpl, "{{task}}") {
		tmpl = "{{task}}\n\n" + tmpl
	}
	tmpl = strings.ReplaceAll(tmpl, "{{task}}", task)

	pairs := []string{}
	for k, v := range extra {
		pairs = append(pairs, k, v)
	}
	// Os extras entram antes: o {{review_file}} pode estar dentro da task.
	if len(pairs) > 0 {
		tmpl = strings.NewReplacer(pairs...).Replace(tmpl)
	}

	rep := strings.NewReplacer(
		"{{repo}}", pr.Repo,
		"{{number}}", strconv.Itoa(pr.Number),
		"{{title}}", pr.Title,
		"{{author}}", pr.Author.Login,
		"{{url}}", pr.URL,
		"{{branch}}", pr.HeadRefName,
		"{{base}}", pr.BaseRefName,
		"{{workdir}}", workdir,
		"{{body}}", body,
		"{{diff}}", diff,
	)
	return strings.TrimSpace(rep.Replace(tmpl))
}
