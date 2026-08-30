package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/beroni/bazel/internal/config"
	"github.com/beroni/bazel/internal/gh"
)

func testPR() gh.PR {
	pr := gh.PR{
		Repo: "acme/api-core", Number: 482, Title: "feat: rate limiting",
		URL: "https://github.com/acme/api-core/pull/482", ChangedFiles: 2,
		HeadRefName: "feat/rate-limit", BaseRefName: "main",
	}
	pr.Author.Login = "maria"
	return pr
}

// A task da lente entra no {{task}} do molde — é só isso que muda de um agente
// para o outro.
func TestBuildPromptFillsTask(t *testing.T) {
	got := buildPrompt("{{task}}\n\nPR {{number}} de {{repo}}", "/exploit-digger {{number}}", testPR(), "", "", nil)
	want := "/exploit-digger 482\n\nPR 482 de acme/api-core"
	if got != want {
		t.Errorf("prompt errado:\n%q\nesperava:\n%q", got, want)
	}
}

// Molde sem {{task}} — escrito antes dos agents nomeados — recebe a instrução
// na primeira linha, que é onde ela ficava.
func TestBuildPromptPrependsTaskToLegacyTemplate(t *testing.T) {
	got := buildPrompt("revise o PR {{number}}", "/lazy-senior-dev {{number}}", testPR(), "", "", nil)
	if !strings.HasPrefix(got, "/lazy-senior-dev 482\n\n") {
		t.Errorf("a task devia abrir o prompt:\n%q", got)
	}
	if !strings.HasSuffix(got, "revise o PR 482") {
		t.Errorf("o molde original devia continuar ali:\n%q", got)
	}
}

// Sem task, o molde não fica com uma linha em branco na frente.
func TestBuildPromptWithoutTask(t *testing.T) {
	if got := buildPrompt("{{task}}\n\nrevise", "", testPR(), "", "", nil); got != "revise" {
		t.Errorf("esperava %q, veio %q", "revise", got)
	}
}

// Publish leva ao agente o review que já foi lido, pelos placeholders — e não
// manda ele revisar de novo.
func TestPublishFillsReviewPlaceholders(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Checkout = false
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agent.Command = "cat"
	cfg.Agent.Args = nil
	// O `cat` devolve o prompt: dá para conferir o que o agente viu.
	cfg.PostAgent = config.AgentDef{
		Name:   "post-report",
		Task:   "/post-report {{review_file}}",
		Prompt: "{{task}}\n\nreview lido:\n{{review}}\n\nPR {{number}}",
		Posts:  true,
	}
	choice := cfg.PostChoice()
	// O clone é obrigatório para inline, mas neste teste não há repositório.
	choice.Steps[0].Checkout = false

	res, err := New(cfg).Publish(context.Background(), testPR(), choice,
		"/tmp/reviews/acme-482.md", "# Veredito\n\n1 achado menor.", nil)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	for _, want := range []string{
		"/post-report /tmp/reviews/acme-482.md",
		"# Veredito",
		"1 achado menor.",
		"PR 482",
	} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("o prompt de publicação não tem %q:\n%s", want, res.Body)
		}
	}
	if !res.Posts {
		t.Error("o resultado de uma publicação devia estar marcado como tal")
	}
}

// Uma pipeline roda os passos na ordem, sobre o mesmo material, e o relatório
// sai em seções.
func TestReviewRunsEveryStep(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Command = "echo"
	cfg.Agent.Checkout = false
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{
		{Name: "primeiro", Args: []string{"achado do primeiro"}, Command: "echo"},
		{Name: "segundo", Args: []string{"achado do segundo"}, Command: "echo"},
	}
	cfg.Pipelines = []config.Pipeline{{Name: "dupla", Steps: []string{"primeiro", "segundo"}}}

	choice, err := cfg.ChoiceByName("dupla")
	if err != nil {
		t.Fatalf("ChoiceByName: %v", err)
	}

	var events []Event
	res, err := New(cfg).Review(context.Background(), testPR(), choice, func(e Event) {
		events = append(events, e)
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}

	if res.Agent != "dupla" || len(res.Steps) != 2 {
		t.Fatalf("esperava 2 passos de 'dupla', veio %q com %d", res.Agent, len(res.Steps))
	}
	for _, want := range []string{"## primeiro", "achado do primeiro", "## segundo", "achado do segundo"} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("o relatório não tem %q:\n%s", want, res.Body)
		}
	}

	// Sem checkout não há clone: os eventos de passo saem em ordem...
	var kinds []string
	for _, e := range events {
		if e.Kind == EventLog {
			continue
		}
		kinds = append(kinds, string(e.Kind)+":"+e.Name)
	}
	want := "step:primeiro,step_done:primeiro,step:segundo,step_done:segundo"
	if got := strings.Join(kinds, ","); got != want {
		t.Errorf("eventos fora de ordem:\n%s\nesperava:\n%s", got, want)
	}

	// ...e o que cada agente escreveu chega como log, atribuído ao seu passo.
	logs := map[int][]string{}
	for _, e := range events {
		if e.Kind == EventLog {
			logs[e.Index] = append(logs[e.Index], e.Stream+":"+e.Text)
		}
	}
	if len(logs[0]) != 1 || logs[0][0] != "stdout:achado do primeiro" {
		t.Errorf("log do primeiro passo: %v", logs[0])
	}
	if len(logs[1]) != 1 || logs[1][0] != "stdout:achado do segundo" {
		t.Errorf("log do segundo passo: %v", logs[1])
	}
}

// O log sai enquanto o agente roda, não só no fim — é o que a interface web
// mostra ao vivo.
func TestLogStreamsWhileAgentRuns(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Checkout = false
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{{
		Name:    "conversador",
		Command: "sh",
		Args:    []string{"-c", "echo primeira; sleep 1; echo segunda"},
	}}
	cfg.Pipelines = nil
	choice, _ := cfg.ChoiceByName("conversador")

	type mark struct {
		text string
		at   time.Duration
	}
	start := time.Now()
	var marks []mark
	if _, err := New(cfg).Review(context.Background(), testPR(), choice, func(e Event) {
		if e.Kind == EventLog {
			marks = append(marks, mark{e.Text, time.Since(start)})
		}
	}); err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(marks) != 2 {
		t.Fatalf("esperava 2 linhas de log, vieram %v", marks)
	}
	if marks[0].at > 700*time.Millisecond {
		t.Errorf("a primeira linha só chegou em %s — está bufferizando até o fim", marks[0].at)
	}
	if marks[1].at < 900*time.Millisecond {
		t.Errorf("a segunda linha chegou cedo demais (%s)", marks[1].at)
	}
}

// Com --output-format stream-json o stdout é JSONL: o log recebe a versão
// legível e o relatório sai do evento final, não do JSON cru.
func TestStreamJSONBecomesLogAndReport(t *testing.T) {
	events := []string{
		`{"type":"system","subtype":"init","tools":["Read","Grep"]}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"..."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"internal/gh/gh.go"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Task","input":{"subagent_type":"exploit-digger","description":"varrer o diff"}}]}}`,
		`{"type":"assistant","parent_tool_use_id":"toolu_1","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"Grep","input":{"pattern":"exec.Command"}}]}}`,
		`{"type":"assistant","parent_tool_use_id":"toolu_1","message":{"content":[{"type":"text","text":"achei uma brecha"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":"No files found"}]}}`,
		`{"type":"result","subtype":"success","result":"# Veredito\n\nNada de grave.","duration_ms":12000,"total_cost_usd":0.42,` +
			`"usage":{"input_tokens":1200,"output_tokens":8000,"cache_creation_input_tokens":4000,"cache_read_input_tokens":20000},` +
			`"modelUsage":{"claude-opus-5":{"inputTokens":1200,"outputTokens":8000,"cacheCreationInputTokens":4000,` +
			`"cacheReadInputTokens":20000,"costUSD":0.41},"claude-haiku-4-5":{"inputTokens":900,"outputTokens":100,"costUSD":0.01}}}`,
	}

	cfg := config.Default()
	cfg.Agent.Checkout = false
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{{
		Name:    "stream",
		Command: "sh",
		// As flags de formato vão depois do script: para o `sh` são só
		// parâmetros posicionais, e para o Bazel são o que liga o modo stream.
		Args: []string{"-c", "cat <<'FIM'\n" + strings.Join(events, "\n") + "\nFIM\n", "sh", "--output-format", "stream-json"},
	}}
	cfg.Pipelines = nil
	choice, _ := cfg.ChoiceByName("stream")

	var log []string
	byAgent := map[string][]string{}
	res, err := New(cfg).Review(context.Background(), testPR(), choice, func(e Event) {
		if e.Kind == EventLog {
			log = append(log, e.Text)
			byAgent[e.Agent] = append(byAgent[e.Agent], e.Text)
		}
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}

	// Cada linha diz quem a escreveu: o agente do passo ou a lente que ele
	// subiu. Sem isso as três lentes da frota chegam embaralhadas e anônimas.
	if len(byAgent["stream"]) == 0 {
		t.Errorf("o agente do passo devia assinar as próprias linhas: %v", byAgent)
	}
	if got := byAgent["exploit-digger"]; len(got) != 2 {
		t.Errorf("as linhas de dentro do Task deviam ser do exploit-digger, vieram %v", byAgent)
	}

	if res.Body != "# Veredito\n\nNada de grave." {
		t.Errorf("o relatório devia vir do evento final, veio %q", res.Body)
	}
	if strings.Contains(res.Body, "achei uma brecha") {
		t.Error("o texto solto do sub-agente não é o relatório")
	}
	// O gasto sai do detalhe por modelo, não do `usage`: é o único que conta
	// as lentes que rodaram como sub-agente e os modelos auxiliares. Aqui são
	// os 33.200 da conversa principal mais os 1.000 do haiku.
	if res.Usage.Total() != 34200 || res.Usage.CostUSD != 0.42 {
		t.Errorf("o gasto devia somar todos os modelos: %+v", res.Usage)
	}

	got := strings.Join(log, "\n")
	for _, want := range []string{
		"· sessão iniciada · 2 ferramentas",
		"→ Read internal/gh/gh.go",
		"→ Agent(exploit-digger): varrer o diff",
		"achei uma brecha",
		"✗ No files found",
		"✓ pronto em 12s · 34,2k tokens · $0.42",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("o log não tem %q:\n%s", want, got)
		}
	}
	// Nem o JSON cru, nem o thinking, nem o rate limit viram log.
	for _, noise := range []string{`{"type":`, "thinking", "rate_limit"} {
		if strings.Contains(got, noise) {
			t.Errorf("o log vazou %q:\n%s", noise, got)
		}
	}
}

// A ferramenta que sobe sub-agente se chama `Agent` (e já se chamou `Task`).
// Cada chamada vira um nome próprio: a frota sobe três lentes em paralelo e
// elas não podem cair todas num balde só chamado "sub-agente".
func TestStreamParserNamesEverySubagent(t *testing.T) {
	var p streamParser
	// Formato conferido contra o `claude -p --output-format stream-json`.
	eventos := []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_A","name":"Agent","input":{"description":"review de precisão","subagent_type":"senior-code-reviewer","prompt":"..."}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_B","name":"Agent","input":{"description":"varredura","subagent_type":"exploit-digger","prompt":"..."}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_C","name":"Agent","input":{"description":"cortes","subagent_type":"lazy-senior-dev","prompt":"..."}}]}}`,
		`{"type":"assistant","parent_tool_use_id":"toolu_B","message":{"content":[{"type":"tool_use","id":"x1","name":"Glob","input":{"pattern":"**/*.go"}}]}}`,
		`{"type":"assistant","parent_tool_use_id":"toolu_A","message":{"content":[{"type":"text","text":"1 achado menor"}]}}`,
		`{"type":"assistant","parent_tool_use_id":"toolu_C","message":{"content":[{"type":"text","text":"40 linhas a menos"}]}}`,
		`{"type":"assistant","parent_tool_use_id":"toolu_B","message":{"content":[{"type":"text","text":"nenhuma brecha"}]}}`,
	}
	porAgente := map[string]int{}
	for _, ev := range eventos {
		for _, entry := range p.line(ev) {
			porAgente[entry.Agent]++
		}
	}

	for _, lente := range []string{"senior-code-reviewer", "exploit-digger", "lazy-senior-dev"} {
		if porAgente[lente] == 0 {
			t.Errorf("nenhuma linha atribuída a %q: %v", lente, porAgente)
		}
	}
	if porAgente["sub-agente"] != 0 {
		t.Errorf("ninguém devia sobrar como \"sub-agente\": %v", porAgente)
	}
	// As três chamadas ficam no terminal de quem as disparou.
	if porAgente[""] != 3 {
		t.Errorf("as chamadas de Agent são do agente principal: %v", porAgente)
	}
}

// Duas chamadas do mesmo tipo são dois sub-agentes, não um.
func TestStreamParserSeparatesTwinSubagents(t *testing.T) {
	var p streamParser
	p.line(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Agent","input":{"subagent_type":"exploit-digger"}}]}}`)
	p.line(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Agent","input":{"subagent_type":"exploit-digger"}}]}}`)

	um := p.line(`{"type":"assistant","parent_tool_use_id":"t1","message":{"content":[{"type":"text","text":"a"}]}}`)
	dois := p.line(`{"type":"assistant","parent_tool_use_id":"t2","message":{"content":[{"type":"text","text":"b"}]}}`)
	if um[0].Agent != "exploit-digger" || dois[0].Agent != "exploit-digger 2" {
		t.Errorf("gêmeos deviam ser distinguidos: %q e %q", um[0].Agent, dois[0].Agent)
	}
}

// Sub-agente que o parser não viu nascer ainda ganha nome próprio por chamada.
func TestStreamParserUnknownParentsStaySeparate(t *testing.T) {
	var p streamParser
	um := p.line(`{"type":"assistant","parent_tool_use_id":"perdido1","message":{"content":[{"type":"text","text":"a"}]}}`)
	dois := p.line(`{"type":"assistant","parent_tool_use_id":"perdido2","message":{"content":[{"type":"text","text":"b"}]}}`)
	if um[0].Agent == dois[0].Agent {
		t.Errorf("dois desconhecidos não podem virar um: %q", um[0].Agent)
	}
}

// Ferramenta comum não é sub-agente.
func TestStreamParserIgnoresPlainTools(t *testing.T) {
	var p streamParser
	p.line(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"r1","name":"Read","input":{"file_path":"main.go"}}]}}`)
	if len(p.subagents) != 0 {
		t.Errorf("Read não sobe agente: %v", p.subagents)
	}
}

// Linha que não é JSON passa direto — um agente que não fala esse formato não
// pode sumir do log.
func TestStreamParserPassesThroughNonJSON(t *testing.T) {
	var p streamParser
	if got := p.line("apenas texto"); len(got) != 1 || got[0].Text != "apenas texto" {
		t.Errorf("linha crua devia passar direto, veio %v", got)
	}
}

// Sem evento final, o relatório é o que o assistente escreveu no caminho.
func TestStreamParserFallsBackToAssistantText(t *testing.T) {
	var p streamParser
	p.line(`{"type":"assistant","message":{"content":[{"type":"text","text":"parcial"}]}}`)
	if got := p.report(); got != "parcial" {
		t.Errorf("esperava o texto do assistente, veio %q", got)
	}
}

func TestIsStreamJSON(t *testing.T) {
	cases := map[bool][][]string{
		true: {
			{"-p", "--output-format", "stream-json", "--verbose"},
			{"--output-format=stream-json"},
		},
		false: {
			{"-p"},
			{"--output-format", "json"},
			{"--output-format"},
		},
	}
	for want, sets := range cases {
		for _, args := range sets {
			if got := isStreamJSON(args); got != want {
				t.Errorf("isStreamJSON(%v) = %v", args, got)
			}
		}
	}
}

// Um passo que falha não derruba o review: vira uma seção com o erro, e o que
// deu certo continua no relatório.
func TestReviewKeepsGoingAfterFailedStep(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Checkout = false
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{
		{Name: "quebrado", Command: "false"},
		{Name: "inteiro", Command: "echo", Args: []string{"achado"}},
	}
	cfg.Pipelines = []config.Pipeline{{Name: "dupla", Steps: []string{"quebrado", "inteiro"}}}
	choice, _ := cfg.ChoiceByName("dupla")

	res, err := New(cfg).Review(context.Background(), testPR(), choice, nil)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if res.Steps[0].Err == nil {
		t.Error("o primeiro passo devia ter falhado")
	}
	if !strings.Contains(res.Body, "✗ falhou") || !strings.Contains(res.Body, "achado") {
		t.Errorf("o relatório devia trazer a falha e o achado:\n%s", res.Body)
	}
}

// Todos os passos falhando é o review inteiro falhando.
func TestReviewFailsWhenEveryStepFails(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Checkout = false
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agents = []config.AgentDef{{Name: "quebrado", Command: "false"}}
	cfg.Pipelines = nil
	choice, _ := cfg.ChoiceByName("quebrado")

	if _, err := New(cfg).Review(context.Background(), testPR(), choice, nil); err == nil {
		t.Fatal("review sem nenhum passo bom devia falhar")
	}
}

// Cancelar solta o review na hora, sem tocar nos passos seguintes.
func TestReviewStopsOnCancel(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Checkout = false
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{
		{Name: "demorado", Command: "sleep", Args: []string{"30"}},
		{Name: "nunca", Command: "echo", Args: []string{"não devia rodar"}},
	}
	cfg.Pipelines = []config.Pipeline{{Name: "dupla", Steps: []string{"demorado", "nunca"}}}
	choice, _ := cfg.ChoiceByName("dupla")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	var ran []string
	_, err := New(cfg).Review(ctx, testPR(), choice, func(e Event) {
		if e.Kind == EventStep {
			ran = append(ran, e.Name)
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("esperava cancelamento, veio %v", err)
	}
	if len(ran) != 1 || ran[0] != "demorado" {
		t.Errorf("o segundo passo não devia começar: %v", ran)
	}
}

// O gasto de cada passo soma no do review — uma pipeline de três lentes
// devolve um número só no fim.
func TestUsageSomaEFormata(t *testing.T) {
	var u Usage
	u.add(Usage{InputTokens: 500, OutputTokens: 250, CostUSD: 0.1})
	u.add(Usage{CacheRead: 1_000_000, CostUSD: 0.2})
	if u.Total() != 1_000_750 {
		t.Errorf("Total somou errado: %d", u.Total())
	}
	if got := u.String(); !strings.Contains(got, "1M tokens") || !strings.Contains(got, "$0.30") {
		t.Errorf("linha de gasto inesperada: %q", got)
	}

	// Um agente que não fala stream-json não reporta nada, e aí não há linha.
	if got := (Usage{}).String(); got != "" {
		t.Errorf("sem gasto não há linha: %q", got)
	}

	for _, c := range []struct {
		n    int
		want string
	}{{0, "0"}, {999, "999"}, {1000, "1k"}, {45_230, "45,2k"}, {1_800_000, "1,8M"}} {
		if got := FormatTokens(c.n); got != c.want {
			t.Errorf("FormatTokens(%d) = %q, queria %q", c.n, got, c.want)
		}
	}
}

// Sem modelUsage — um agente mais simples, ou uma versão antiga do Claude Code
// — o `usage` da conversa principal ainda vale.
func TestUsageCaiNoUsageQuandoNaoHaModelUsage(t *testing.T) {
	var p streamParser
	p.line(`{"type":"result","subtype":"success","result":"ok","duration_ms":1000,` +
		`"total_cost_usd":0.05,"usage":{"input_tokens":10,"output_tokens":20,` +
		`"cache_creation_input_tokens":30,"cache_read_input_tokens":40}}`)
	if p.usage.Total() != 100 || p.usage.CostUSD != 0.05 {
		t.Errorf("devia cair no usage: %+v", p.usage)
	}
}
