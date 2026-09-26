// Package agent executa os agentes de IA configurados sobre um pull request.
package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/beroni/bazel/internal/config"
	"github.com/beroni/bazel/internal/gh"
	"github.com/beroni/bazel/internal/pricing"
	"github.com/beroni/bazel/internal/skills"
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
	// checkout está desligado; só sobrevive ao review com KeepWorkspace — ou
	// quando a execução parou num passo reservado, e aí o clone é de quem
	// chamou: é nele que o resto da pipeline vai continuar.
	Workdir string
	// StoppedAt é o índice do passo reservado onde a execução parou. -1 quando
	// a escolha rodou até o fim. O Runner não executa `pause` nem `publish`:
	// o primeiro espera uma pessoa e o segundo precisa do relatório em disco,
	// e nenhuma das duas coisas é trabalho dele.
	StoppedAt int
}

// Cont é por onde uma execução interrompida recomeça: o passo seguinte ao que
// parou, o clone que ficou de pé e o que já tinha rodado.
type Cont struct {
	From    int
	Workdir string
	Steps   []StepResult
}

// Runner roda os agentes configurados.
type Runner struct {
	cfg *config.Config
	// KeepWorkspace preserva o clone temporário depois do review.
	KeepWorkspace bool
	// PricingTable é a tabela de preços usada para calcular o custo em USD.
	// Se nil, o custo vem do campo total_cost_usd do stream-json (Claude Code).
	PricingTable *pricing.Table
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
	return r.run(ctx, pr, choice, nil, Cont{}, onEvent)
}

// Continue retoma uma escolha que parou num passo reservado, do passo indicado
// em diante e dentro do mesmo clone. O relatório sai com os passos de todas as
// rodadas juntos: quem lê não tem por que saber que houve uma pausa no meio.
func (r *Runner) Continue(ctx context.Context, pr gh.PR, choice config.Choice, cont Cont, onEvent func(Event)) (Result, error) {
	return r.run(ctx, pr, choice, nil, cont, onEvent)
}

// Publish roda o agente de publicação sobre um review que já está pronto — o
// que você leu na tela. O caminho do arquivo e o texto entram no prompt pelos
// placeholders {{review_file}} e {{review}}; o agente não refaz o review,
// publica esse.
func (r *Runner) Publish(ctx context.Context, pr gh.PR, choice config.Choice, reviewPath, reviewBody string, onEvent func(Event)) (Result, error) {
	return r.run(ctx, pr, choice, map[string]string{
		"{{review_file}}": reviewPath,
		"{{review}}":      reviewBody,
	}, Cont{}, onEvent)
}

func (r *Runner) run(ctx context.Context, pr gh.PR, choice config.Choice, extra map[string]string, cont Cont, onEvent func(Event)) (Result, error) {
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

	// Retomando, o clone é o que ficou de pé na parada e o dono dele é quem
	// nos chamou — clonar de novo daria outro código, e os passos já corridos
	// falariam de um repositório que não é este.
	var (
		workdir = cont.Workdir
		ws      *workspace.Workspace
	)
	if workdir == "" && choice.NeedsCheckout() {
		emit(Event{Kind: EventClone, Total: total, Name: pr.Key()})
		var err error
		ws, err = workspace.Prepare(ctx, pr)
		if err != nil {
			return Result{}, err
		}
		ws.Keep = r.KeepWorkspace
		defer ws.Cleanup()
		workdir = ws.Dir
	}

	// Skill embarcada só existe dentro do binário; o Claude Code a acha em
	// disco, no .claude/skills do diretório de trabalho. O lugar dela é o
	// clone do PR: nasce e morre com ele, e a máquina de quem roda o Bazel
	// não ganha nada em ~/.claude que ninguém pediu.
	if step := builtinSemClone(choice); step != "" {
		return Result{}, fmt.Errorf("`%s` runs a skill built into Bazel, which needs the PR clone to be found — turn checkout on for it, or point it at a skill of your own", step)
	}
	if workdir != "" && usaBuiltin(choice) {
		if err := skills.Materialize(workdir); err != nil {
			return Result{}, err
		}
	}

	diff, truncated, err := r.material(ctx, pr, choice)
	if err != nil {
		return Result{}, err
	}

	// Os passos das rodadas anteriores entram no relatório final: quem lê não
	// tem por que saber que houve uma pausa no meio.
	steps := append(make([]StepResult, 0, total), cont.Steps...)
	// fechado é o gasto dos passos que já terminaram: o parcial de um passo
	// em curso é somado a ele, para o número na tela ser o do review todo.
	var fechado Usage
	for _, s := range steps {
		fechado.add(s.Usage)
	}
	for i := cont.From; i < len(choice.Steps); i++ {
		step := choice.Steps[i]
		// Passo reservado é do Bazel, não do Runner: devolve o que já saiu e
		// quem chamou decide — esperar uma pessoa, ou publicar.
		if step.Reserved != "" {
			if ws != nil {
				// O clone não morre aqui: é nele que o resto vai continuar.
				ws.Keep = true
			}
			return r.parcial(pr, choice, steps, workdir, truncated, i, start)
		}
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
		StoppedAt: -1,
	}, nil
}

// parcial é o resultado de uma execução que parou num passo reservado: tem o
// relatório do que já rodou — é o que a pessoa lê antes de mandar continuar — e
// diz onde parar e onde recomeçar.
func (r *Runner) parcial(pr gh.PR, choice config.Choice, steps []StepResult, workdir string, truncated bool, at int, start time.Time) (Result, error) {
	body, err := joinSteps(steps)
	if err != nil {
		// Tudo que rodou até aqui falhou: não há o que ler nem o que publicar,
		// e parar para mostrar o nada seria pior do que falhar agora.
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
		StoppedAt: at,
	}, nil
}

// usaBuiltin diz se algum passo chama uma skill que veio no binário.
func usaBuiltin(choice config.Choice) bool {
	for _, s := range choice.Steps {
		if skills.IsBuiltin(skills.TaskSkill(s.Task)) {
			return true
		}
	}
	return false
}

// builtinSemClone devolve o passo que chama uma skill embarcada sem ter clone
// onde escrevê-la — sem diretório de trabalho do PR, ela não tem onde existir,
// e escrevê-la na pasta de quem rodou o Bazel seria sujar um projeto alheio.
func builtinSemClone(choice config.Choice) string {
	for _, s := range choice.Steps {
		if !s.Checkout && skills.IsBuiltin(skills.TaskSkill(s.Task)) {
			return s.Name
		}
	}
	return ""
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
		diff += "\n\n[... diff truncated at " + strconv.Itoa(r.cfg.MaxDiffBytes) + " bytes ...]"
	}
	return diff, truncated, nil
}

// PublishableBody é o que vai ao PR: o corpo só dos passos cuja saída é
// publicável. Numa pipeline `history-pr → review-fleet` a tela mostra as duas
// coisas — é para isso que você encadeou — mas o PR recebe só o review. A
// história é contexto para quem vai revisar, não um comentário para o autor.
//
// O bool diz se sobrou algo de fora: com ele falso o relatório inteiro é o que
// se publica, e o arquivo em disco já serve.
func PublishableBody(res Result, choice config.Choice) (string, bool, error) {
	kept := make([]StepResult, 0, len(res.Steps))
	for _, s := range res.Steps {
		if choice.StepPublishes(s.Name) {
			kept = append(kept, s)
		}
	}
	if len(kept) == len(res.Steps) {
		return res.Body, false, nil
	}
	if len(kept) == 0 {
		return "", true, errors.New("no step of this agent publishes to the PR")
	}
	body, err := joinSteps(kept)
	if err != nil {
		return "", true, err
	}
	return body, true, nil
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
	adapter, err := newOutputAdapter(step.Format, step.Args)
	if err != nil {
		return "", Usage{}, err
	}
	createdHome, err := isolateGrok(&step)
	if err != nil {
		return "", Usage{}, err
	}
	if createdHome != "" {
		defer os.RemoveAll(createdHome)
	}

	cmd := exec.CommandContext(ctx, step.Command, step.Args...)
	cmd.Dir = workdir // vazio = herda o diretório atual
	cmd.Stdin = strings.NewReader(prompt)
	if len(step.Env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range step.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
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
		wg     sync.WaitGroup
		errBuf lastLines
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		visto := 0
		var cota time.Time
		eachLine(stdout, func(line string) {
			for _, out := range adapter.line(line) {
				log("stdout", out.Agent, out.Text)
			}
			// A conta só sobe quando muda: uma chamada de ferramenta não
			// gasta token nenhum e não precisa acordar o navegador.
			if gasto := adapter.usage(); onUsage != nil && gasto.Total() != visto {
				visto = gasto.Total()
				onUsage(gasto)
			}
			if limits := adapter.limits(); onLimits != nil && limits.At.After(cota) {
				cota = limits.At
				onLimits(limits)
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
		if ae := adapter.err(); ae != nil && errBuf.String() == "" {
			return "", adapter.usage(), fmt.Errorf("agent `%s` failed: %w", step.Command, ae)
		}
		msg := errBuf.String()
		if msg == "" {
			msg = err.Error()
		}
		return "", adapter.usage(), agentFailure(step, msg)
	}
	u := adapter.usage()
	// Com pricing table configurada, recalcula o custo a partir dos tokens
	// e do modelo — é o que permite que o custo mude sem rebuild do binário.
	if r.PricingTable != nil && u.Model != "" {
		u.CostUSD = r.PricingTable.Cost(u.Model, u.InputTokens, u.OutputTokens, u.CacheWrite, u.CacheRead)
	}
	return adapter.report(), u, nil
}

func agentFailure(step config.ResolvedAgent, message string) error {
	if step.Format == "grok-stream" && strings.Contains(strings.ToLower(message), "max turns reached") {
		return fmt.Errorf("Grok reached its configured turn limit before completing the review")
	}
	return fmt.Errorf("agent `%s` failed: %s", step.Command, message)
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
