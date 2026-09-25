package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Um config novo nasce sem agente nenhum: a lista é montada na página, a
// partir das skills instaladas — não há lista de fábrica que chute o que este
// computador tem.
func TestDefaultComecaSemAgentes(t *testing.T) {
	cfg := Default()
	if len(cfg.Agents) != 0 || len(cfg.Pipelines) != 0 {
		t.Fatalf("config novo devia vir sem agents e sem pipelines: %+v / %+v", cfg.Agents, cfg.Pipelines)
	}
	if len(cfg.Choices()) != 0 {
		t.Errorf("sem agents e com o molde de fábrica não há o que rodar: %+v", cfg.Choices())
	}
	if len(cfg.DefaultChoice().Steps) != 0 {
		t.Error("o padrão de um config vazio não pode ter passo nenhum")
	}
	if _, err := cfg.ChoiceByName("review-fleet"); !errors.Is(err, ErrNoAgents) {
		t.Errorf("escolher um agente numa lista vazia devia dar ErrNoAgents, deu %v", err)
	}
	// E o agente de publicação continua de pé: ele não é uma escolha da
	// lista, é o que roda quando você manda publicar um review já lido.
	if cfg.PostChoice().Name != "bazel-post-report" {
		t.Errorf("o post_agent devia continuar no padrão, veio %q", cfg.PostChoice().Name)
	}
}

// O seletor mostra os agents na ordem do arquivo e depois as pipelines, com a
// primeira escolha valendo como padrão.
func TestChoicesOrderAndDefault(t *testing.T) {
	cfg := Default()
	cfg.Agents = []AgentDef{
		{Name: "review-fleet", Task: "/review-fleet {{number}}"},
		{Name: "exploit-digger", Task: "/exploit-digger {{number}}"},
		{Name: "lazy-senior-dev", Task: "/lazy-senior-dev {{number}}"},
	}
	cfg.Pipelines = []Pipeline{{
		Name:  "frota-em-série",
		Steps: []string{"review-fleet", "exploit-digger", "lazy-senior-dev"},
	}}

	choices := cfg.Choices()
	if len(choices) != len(cfg.Agents)+len(cfg.Pipelines) {
		t.Fatalf("esperava %d escolhas, vieram %d", len(cfg.Agents)+len(cfg.Pipelines), len(choices))
	}
	if choices[0].Name != cfg.Agents[0].Name {
		t.Errorf("a primeira escolha devia ser %q, é %q", cfg.Agents[0].Name, choices[0].Name)
	}
	if cfg.DefaultChoice().Name != choices[0].Name {
		t.Errorf("o padrão devia ser a primeira escolha")
	}
	last := choices[len(choices)-1]
	if !last.Pipeline || len(last.Steps) != 3 {
		t.Errorf("a última escolha devia ser a pipeline de 3 passos, é %+v", last)
	}
}

// Adicionar uma skill é o caminho normal de montar a lista: o agente sai com o
// nome da skill e a task que a invoca sobre o PR.
func TestAddAgentFromSkill(t *testing.T) {
	cfg := Default()
	def, err := cfg.AddAgentFromSkill("/review-fleet", "as três lentes", false)
	if err != nil {
		t.Fatalf("AddAgentFromSkill: %v", err)
	}
	if def.Name != "review-fleet" || def.Task != "/review-fleet {{number}}" {
		t.Errorf("agente montado errado: %+v", def)
	}
	if def.Posts || def.Prompt != "" {
		t.Errorf("agente de leitura não publica nem troca o molde: %+v", def)
	}
	if len(cfg.Choices()) != 1 || cfg.DefaultChoice().Name != "review-fleet" {
		t.Error("o primeiro agente adicionado devia virar o padrão")
	}

	// A mesma skill pode virar também um agente que publica sozinho: o nome
	// muda, e é o sufixo que deixa os dois conviverem.
	post, err := cfg.AddAgentFromSkill("review-fleet", "as três lentes", true)
	if err != nil {
		t.Fatalf("AddAgentFromSkill com posts: %v", err)
	}
	if post.Name != "review-fleet-post" || !post.Posts {
		t.Errorf("o agente que publica devia se chamar review-fleet-post: %+v", post)
	}
	if !strings.Contains(post.Task, "--post") {
		t.Errorf("a task devia levar o --post: %q", post.Task)
	}
	if strings.Contains(post.Prompt, "do not publish anything to GitHub") {
		t.Error("o molde de quem publica não pode proibir publicar")
	}

	if _, err := cfg.AddAgentFromSkill("review-fleet", "", false); err == nil {
		t.Error("adicionar a mesma skill duas vezes devia dar erro")
	}
	if _, err := cfg.AddAgentFromSkill("  ", "", false); err == nil {
		t.Error("skill sem nome devia dar erro")
	}
	if _, err := cfg.AddAgentFromSkill("../../etc/passwd", "", false); err == nil {
		t.Error("nome de skill com barra devia dar erro")
	}
}

// Tirar da lista e trocar o padrão são as outras duas mexidas que a página faz.
func TestRemoveAgentAndSetDefault(t *testing.T) {
	cfg := Default()
	for _, name := range []string{"a", "b", "c"} {
		if _, err := cfg.AddAgentFromSkill(name, "", false); err != nil {
			t.Fatalf("montando a lista: %v", err)
		}
	}
	if !cfg.SetDefaultAgent("C") {
		t.Fatal("tornar padrão não devia diferenciar caixa")
	}
	if got := cfg.DefaultChoice().Name; got != "c" {
		t.Errorf("o padrão devia ser c, é %q", got)
	}
	if names := len(cfg.Agents); names != 3 {
		t.Errorf("tornar padrão só reordena — a lista ficou com %d", names)
	}
	if !cfg.RemoveAgent("c") || cfg.RemoveAgent("c") {
		t.Error("remover devia valer uma vez só")
	}
	if got := cfg.DefaultChoice().Name; got != "a" {
		t.Errorf("removido o padrão, o próximo assume: veio %q", got)
	}
}

// Um agent nomeado herda comando, args, checkout e timeout do bloco `agent`.
func TestResolveInheritsBase(t *testing.T) {
	cfg := Default()
	cfg.Agent.Command = "codex"
	cfg.Agent.Args = []string{"exec", "-"}
	cfg.Agent.TimeoutSeconds = 900

	off := false
	cfg.Agents = []AgentDef{
		{Name: "herdeiro", Task: "/review-fleet {{number}}"},
		{Name: "proprio", Command: "claude", TimeoutSeconds: 60, Checkout: &off},
	}
	cfg.Pipelines = nil

	got := cfg.Choices()
	herdeiro := got[0].Steps[0]
	if herdeiro.Command != "codex" || strings.Join(herdeiro.Args, " ") != "exec -" {
		t.Errorf("não herdou o comando base: %+v", herdeiro)
	}
	if herdeiro.TimeoutSeconds != 900 || !herdeiro.Checkout {
		t.Errorf("não herdou timeout/checkout: %+v", herdeiro)
	}
	if herdeiro.Prompt != cfg.Agent.Prompt {
		t.Error("não herdou o molde do prompt")
	}

	proprio := got[1].Steps[0]
	if proprio.Command != "claude" || proprio.TimeoutSeconds != 60 || proprio.Checkout {
		t.Errorf("não respeitou os campos próprios: %+v", proprio)
	}
	// Args do claude não podem vir do bloco de outro executável.
	if len(proprio.Args) != 0 {
		t.Errorf("args do comando base vazaram para outro executável: %v", proprio.Args)
	}
	if got[0].NeedsCheckout() != true || got[1].NeedsCheckout() != false {
		t.Error("NeedsCheckout não seguiu o checkout de cada passo")
	}
}

func TestResolveKeepsAgentEnv(t *testing.T) {
	cfg := Default()
	cfg.Agent.Env = map[string]string{"FROM_BASE": "1"}
	cfg.Agents = []AgentDef{
		{Name: "herda"},
		{
			Name:    "grok-review",
			Command: "grok",
			Env:     map[string]string{"GROK_MCP_STARTUP_TIMEOUT_SECS": "2"},
		},
	}
	cfg.Pipelines = nil

	choices := cfg.Choices()
	if choices[0].Steps[0].Env["FROM_BASE"] != "1" {
		t.Errorf("did not inherit base env: %v", choices[0].Steps[0].Env)
	}
	got := choices[1].Steps[0]
	if got.Env["GROK_MCP_STARTUP_TIMEOUT_SECS"] != "2" {
		t.Errorf("own env was dropped: %v", got.Env)
	}
	if _, ok := got.Env["FROM_BASE"]; ok {
		t.Errorf("own env should replace the base map, not merge: %v", got.Env)
	}
}

func TestResolveFormatInheritanceAndOverride(t *testing.T) {
	cfg := Default()
	if cfg.Agent.Format != "" {
		t.Fatalf("default agent format must stay empty, got %q", cfg.Agent.Format)
	}
	cfg.Agent.Format = "claude-stream"
	cfg.Agents = []AgentDef{
		{Name: "inherits"},
		{Name: "overrides", Format: "codex-json"},
		{Name: "command-only", Command: "codex"},
	}

	choices := cfg.Choices()
	if got := choices[0].Steps[0].Format; got != "claude-stream" {
		t.Errorf("inherited format = %q, want claude-stream", got)
	}
	if got := choices[1].Steps[0].Format; got != "codex-json" {
		t.Errorf("override format = %q, want codex-json", got)
	}
	if got := choices[2].Steps[0].Format; got != "claude-stream" {
		t.Errorf("command-only override format = %q, want claude-stream", got)
	}

	cfg.Agent.Format = ""
	if got := cfg.Choices()[2].Steps[0].Format; got != "" {
		t.Errorf("command-only override without a configured format = %q, want empty", got)
	}
}

func TestPostChoiceInheritsAndOverridesFormat(t *testing.T) {
	cfg := Default()
	cfg.Agent.Format = "claude-stream"
	if got := cfg.PostChoice().Steps[0].Format; got != "claude-stream" {
		t.Errorf("post agent inherited format = %q, want claude-stream", got)
	}
	cfg.PostAgent.Format = "plain"
	if got := cfg.PostChoice().Steps[0].Format; got != "plain" {
		t.Errorf("post agent override format = %q, want plain", got)
	}
}

// Passo apontando para agent inexistente é ignorado; pipeline sem passo válido
// nem aparece.
func TestPipelineSkipsUnknownSteps(t *testing.T) {
	cfg := Default()
	cfg.Agents = []AgentDef{{Name: "a"}, {Name: "b"}}
	cfg.Pipelines = []Pipeline{
		{Name: "meia", Steps: []string{"a", "fantasma", "b"}},
		{Name: "vazia", Steps: []string{"fantasma"}},
	}
	choices := cfg.Choices()
	if len(choices) != 3 {
		t.Fatalf("esperava 2 agents + 1 pipeline, vieram %d", len(choices))
	}
	if names := strings.Join(choices[2].StepNames(), ","); names != "a,b" {
		t.Errorf("a pipeline devia ficar com a,b — ficou com %q", names)
	}
}

func TestChoiceByName(t *testing.T) {
	cfg := Default()
	cfg.Agents = []AgentDef{
		{Name: "review-fleet", Task: "/review-fleet {{number}}"},
		{Name: "exploit-digger", Task: "/exploit-digger {{number}}"},
	}
	if _, err := cfg.ChoiceByName("EXPLOIT-digger"); err != nil {
		t.Errorf("o nome não devia diferenciar caixa: %v", err)
	}
	err := func() error { _, e := cfg.ChoiceByName("nada"); return e }()
	if err == nil {
		t.Fatal("nome desconhecido devia dar erro")
	}
	if !strings.Contains(err.Error(), "review-fleet") {
		t.Errorf("o erro devia listar os nomes disponíveis: %v", err)
	}
}

// Sem `agents:` mas com um prompt seu, sobra a escolha única do bloco `agent`
// — que é como o Bazel se comportava antes do seletor. Com o molde de fábrica
// não sobra nada: ele é uma casca em volta do {{task}} de um agente.
func TestChoicesFallsBackToBaseAgent(t *testing.T) {
	cfg := Default()
	cfg.Agents = nil
	cfg.Pipelines = nil
	if len(cfg.Choices()) != 0 {
		t.Fatal("o molde de fábrica sem agente não é escolha nenhuma")
	}
	cfg.Agent.Prompt = "revise o PR {{number}} de {{repo}}"
	choices := cfg.Choices()
	if len(choices) != 1 || len(choices[0].Steps) != 1 {
		t.Fatalf("esperava uma escolha de um passo, vieram %+v", choices)
	}
	if choices[0].Steps[0].Command != cfg.Agent.Command {
		t.Error("a escolha única devia ser o próprio bloco agent")
	}
}

// O que está no arquivo é o que vale: o Load não inventa agente para ninguém.
func TestLoadNaoInventaAgentes(t *testing.T) {
	t.Run("arquivo sem agents", func(t *testing.T) {
		cfg := loadFrom(t, "repos: [acme/api-core]\nagent:\n  command: claude\n")
		if len(cfg.Agents) != 0 {
			t.Errorf("config sem agents devia continuar sem: %+v", cfg.Agents)
		}
	})

	t.Run("prompt customizado", func(t *testing.T) {
		cfg := loadFrom(t, "repos: [acme/api-core]\nagent:\n  command: claude\n  prompt: revise o PR {{number}}\n")
		if cfg.Agent.Prompt != "revise o PR {{number}}" {
			t.Errorf("o prompt do arquivo não podia ser tocado: %q", cfg.Agent.Prompt)
		}
		if len(cfg.Choices()) != 1 {
			t.Error("com prompt seu e sem agents, o seletor mostra a escolha única")
		}
	})

	t.Run("agents do arquivo", func(t *testing.T) {
		cfg := loadFrom(t, "repos: [acme/api-core]\nagents:\n  - name: meu\n    task: /meu {{number}}\n")
		if len(cfg.Agents) != 1 || cfg.Agents[0].Name != "meu" {
			t.Errorf("os agents do arquivo é que valem: %+v", cfg.Agents)
		}
	})
}

// O agente que publica sozinho é declarado como tal — a interface precisa
// disso para avisar antes de escrever no PR de outra pessoa.
func TestPostingAgentIsMarked(t *testing.T) {
	cfg := Default()
	if _, err := cfg.AddAgentFromSkill("review-fleet", "", false); err != nil {
		t.Fatalf("montando a lista: %v", err)
	}
	if _, err := cfg.AddAgentFromSkill("review-fleet", "", true); err != nil {
		t.Fatalf("montando a lista: %v", err)
	}
	post, err := cfg.ChoiceByName("review-fleet-post")
	if err != nil {
		t.Fatalf("ChoiceByName: %v", err)
	}
	if !post.Posts {
		t.Error("o agente de review+post devia estar marcado como publicador")
	}
	if !strings.Contains(post.Steps[0].Task, "--post") {
		t.Errorf("a task devia disparar a frota com --post: %q", post.Steps[0].Task)
	}
	if strings.Contains(post.Steps[0].Prompt, "do not publish anything to GitHub") {
		t.Error("o molde dele não pode proibir publicar")
	}

	plain, _ := cfg.ChoiceByName("review-fleet")
	if plain.Posts {
		t.Error("a frota normal não publica sozinha")
	}
	if !strings.Contains(plain.Steps[0].Prompt, "do not publish anything to GitHub") {
		t.Error("o molde padrão devia continuar proibindo publicar")
	}
}

// Uma pipeline herda o aviso de publicação de qualquer passo seu.
func TestPipelineInheritsPostsFlag(t *testing.T) {
	cfg := Default()
	cfg.Agents = []AgentDef{{Name: "lê"}, {Name: "publica", Posts: true}}
	cfg.Pipelines = []Pipeline{
		{Name: "só-lê", Steps: []string{"lê"}},
		{Name: "lê-e-publica", Steps: []string{"lê", "publica"}},
	}
	quieta, _ := cfg.ChoiceByName("só-lê")
	barulhenta, _ := cfg.ChoiceByName("lê-e-publica")
	if quieta.Posts {
		t.Error("pipeline sem passo publicador não devia marcar Posts")
	}
	if !barulhenta.Posts {
		t.Error("pipeline com passo publicador devia marcar Posts")
	}
}

// Args de fábrica antigos ganham o modo stream; args customizados ficam.
func TestLoadUpgradesLegacyArgs(t *testing.T) {
	cfg := loadFrom(t, "repos: [acme/api]\nagent:\n  command: claude\n  args: [-p, --allowedTools, \"Read,Grep,Glob,Bash,Agent\"]\n")
	if !sameArgs(cfg.Agent.Args, defaultArgs()) {
		t.Errorf("args de fábrica antigos deviam virar os novos, vieram %v", cfg.Agent.Args)
	}

	custom := loadFrom(t, "repos: [acme/api]\nagent:\n  command: codex\n  args: [exec, -]\n")
	if len(custom.Agent.Args) != 2 || custom.Agent.Args[0] != "exec" {
		t.Errorf("args customizados não podiam ser tocados, viraram %v", custom.Agent.Args)
	}
}

func TestFormatOmissionAndRoundTrip(t *testing.T) {
	cfg := loadFrom(t, "repos: [acme/api]\nagent:\n  command: codex\nagents:\n  - name: custom\n    command: gemini\n")
	if cfg.Agent.Format != "" || cfg.Agents[0].Format != "" {
		t.Fatalf("omitted formats must remain empty: %+v / %+v", cfg.Agent, cfg.Agents[0])
	}
	if got := cfg.Choices()[0].Steps[0].Format; got != "" {
		t.Errorf("command-only config inferred format %q", got)
	}

	cfg.Agent.Format = "claude-stream"
	cfg.Agents[0].Format = "codex-json"
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load()
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if reloaded.Agent.Format != "claude-stream" || reloaded.Agents[0].Format != "codex-json" {
		t.Errorf("formats did not round trip: %+v / %+v", reloaded.Agent, reloaded.Agents[0])
	}
}

func TestFormatValidationOnLoadAndSave(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want string
	}{
		{"base", "agent:\n  format: nope\n", "base agent"},
		{"agent", "agents:\n  - name: custom\n    format: nope\n", "agents[0]"},
		{"post", "post_agent:\n  name: publish\n  format: nope\n", "post_agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("BAZEL_HOME", dir)
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load error = %v, want context %q", err, tc.want)
			}
		})
	}

	cfg := Default()
	cfg.PostAgent.Format = "nope"
	if err := cfg.Save(); err == nil || !strings.Contains(err.Error(), "post_agent") {
		t.Errorf("Save error = %v, want post_agent context", err)
	}
}

// O agente de publicação é o que roda depois de você ler o review: clona por
// padrão (inline precisa do diff) e leva o arquivo do review no prompt.
func TestPostChoice(t *testing.T) {
	cfg := Default()
	post := cfg.PostChoice()
	if !post.Posts || len(post.Steps) != 1 {
		t.Fatalf("escolha de publicação inesperada: %+v", post)
	}
	if !post.Steps[0].Checkout {
		t.Error("publicar devia clonar por padrão: inline precisa do diff")
	}
	if !strings.Contains(post.Steps[0].Task, "{{review_file}}") {
		t.Errorf("a task devia receber o arquivo do review: %q", post.Steps[0].Task)
	}
	if !strings.Contains(post.Steps[0].Prompt, "Do not redo the review") {
		t.Error("o molde precisa proibir refazer o review — o que vai ao PR é o que foi lido")
	}

	// checkout: false explícito é respeitado.
	no := false
	cfg.PostAgent.Checkout = &no
	if cfg.PostChoice().Steps[0].Checkout {
		t.Error("checkout: false no post_agent devia valer")
	}

	// Config sem post_agent ganha o padrão.
	fromDisk := loadFrom(t, "repos: [acme/api]\n")
	if fromDisk.PostChoice().Name != "bazel-post-report" {
		t.Errorf("config sem post_agent devia cair no padrão, veio %q", fromDisk.PostChoice().Name)
	}
}

// O pause saiu: quem para para você escolher o que vai é o passo publish. Uma
// config gravada com ele sai sem ele, e publicando uma vez só.
func TestMigraPipelines(t *testing.T) {
	old := loadFrom(t, `repos: [acme/api]
agents:
  - name: review-fleet
    task: /review-fleet {{number}}
pipelines:
  - name: pausada
    steps: [review-fleet, pause, publish]
  - name: dupla
    steps: [review-fleet, publish, publish]
`)
	if got := strings.Join(old.Pipelines[0].Steps, ","); got != "review-fleet,publish" {
		t.Errorf("a pausa devia sair: %s", got)
	}
	if got := strings.Join(old.Pipelines[1].Steps, ","); got != "review-fleet,publish" {
		t.Errorf("publicar uma vez só: %s", got)
	}
}

func loadFrom(t *testing.T, yaml string) *Config {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BAZEL_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("escrevendo config: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// Nem toda skill produz review. A que não produz — history-pr conta a história
// de um PR — não pode ter caminho para o GitHub: publicá-la diria ao time que
// o código foi revisado quando ninguém revisou nada.
func TestPublishable(t *testing.T) {
	t.Run("padrão é publicável", func(t *testing.T) {
		cfg := Default()
		if _, err := cfg.AddAgentFromSkill("review-fleet", "", false); err != nil {
			t.Fatalf("montando a lista: %v", err)
		}
		ch, err := cfg.ChoiceByName("review-fleet")
		if err != nil {
			t.Fatalf("ChoiceByName: %v", err)
		}
		if !ch.Publishable {
			t.Error("agente sem o campo no yaml continua indo ao PR, como sempre foi")
		}
	})

	t.Run("desligado no painel", func(t *testing.T) {
		cfg := Default()
		if _, err := cfg.AddAgentFromSkill("history-pr", "", false); err != nil {
			t.Fatalf("montando a lista: %v", err)
		}
		if err := cfg.SetAgentPublishable("history-pr", false); err != nil {
			t.Fatalf("SetAgentPublishable: %v", err)
		}
		ch, _ := cfg.ChoiceByName("history-pr")
		if ch.Publishable || ch.Steps[0].Publishable {
			t.Error("desligado no painel, a saída não vai ao PR")
		}
		// E o tique volta.
		if err := cfg.SetAgentPublishable("history-pr", true); err != nil {
			t.Fatalf("religando: %v", err)
		}
		if ch, _ := cfg.ChoiceByName("history-pr"); !ch.Publishable {
			t.Error("religado, volta a publicar")
		}
	})

	t.Run("quem publica sozinho não pode ser desligado", func(t *testing.T) {
		cfg := Default()
		if _, err := cfg.AddAgentFromSkill("review-fleet", "", true); err != nil {
			t.Fatalf("montando a lista: %v", err)
		}
		if err := cfg.SetAgentPublishable("review-fleet-post", false); err == nil {
			t.Error("agente que escreve no PR sozinho não é contido por um tique da interface")
		}
	})

	t.Run("agente que não existe", func(t *testing.T) {
		cfg := Default()
		if err := cfg.SetAgentPublishable("fantasma", false); err == nil {
			t.Error("agente fora da lista devia dar erro")
		}
	})

	t.Run("pipeline publica se algum passo publicar", func(t *testing.T) {
		cfg := loadFrom(t, `repos: [acme/api-core]
agents:
  - name: history-pr
    task: /history-pr {{number}}
    publishable: false
  - name: review-fleet
    task: /review-fleet {{number}}
pipelines:
  - name: história e review
    steps: [history-pr, review-fleet]
  - name: só história
    steps: [history-pr]
`)
		mista, err := cfg.ChoiceByName("história e review")
		if err != nil {
			t.Fatalf("ChoiceByName: %v", err)
		}
		if !mista.Publishable {
			t.Error("pipeline com um passo publicável tem o que levar ao PR")
		}
		if mista.StepPublishes("history-pr") {
			t.Error("o passo de história continua não publicando, mesmo na pipeline")
		}
		if !mista.StepPublishes("review-fleet") {
			t.Error("o passo de review é o que sobe")
		}
		if mista.StepPublishes("não-existe") {
			t.Error("passo fora da escolha não publica nada")
		}

		so, err := cfg.ChoiceByName("só história")
		if err != nil {
			t.Fatalf("ChoiceByName: %v", err)
		}
		if so.Publishable {
			t.Error("pipeline inteira de passos não-publicáveis não tem o que levar ao PR")
		}
	})
}

// A skill de publicação passou a viajar no binário. Quem está com o padrão
// antigo intocado — que chamava a /post-report de ~/.claude/skills e só
// funcionava em quem a tivesse instalado — ganha a embarcada; quem editou o
// seu fica com o que escreveu.
func TestPostAgentMigraParaAEmbarcada(t *testing.T) {
	t.Run("padrão antigo vira o novo", func(t *testing.T) {
		cfg := loadFrom(t, `repos: [acme/api-core]
post_agent:
  name: post-report
  description: publishes the review you have just read, with inline comments
  task: /post-report {{review_file}}
  posts: true
`)
		if got := cfg.PostChoice().Name; got != "bazel-post-report" {
			t.Errorf("o padrão antigo intocado devia migrar, veio %q", got)
		}
	})

	t.Run("o que foi customizado sobrevive", func(t *testing.T) {
		cfg := loadFrom(t, `repos: [acme/api-core]
post_agent:
  name: post-report
  task: /post-report {{review_file}}
  posts: true
  timeout_seconds: 900
  prompt: publique isso do meu jeito
`)
		post := cfg.PostChoice()
		if post.Name != "bazel-post-report" {
			t.Errorf("a skill devia migrar, veio %q", post.Name)
		}
		if post.Steps[0].Prompt != "publique isso do meu jeito" {
			t.Errorf("o prompt escrito à mão devia sobreviver: %q", post.Steps[0].Prompt)
		}
		if post.Steps[0].TimeoutSeconds != 900 {
			t.Errorf("o timeout escrito à mão devia sobreviver: %d", post.Steps[0].TimeoutSeconds)
		}
	})

	t.Run("post_agent editado fica como está", func(t *testing.T) {
		cfg := loadFrom(t, `repos: [acme/api-core]
post_agent:
  name: post-report
  task: /post-report {{review_file}} --mine
  posts: true
`)
		if got := cfg.PostChoice().Name; got != "post-report" {
			t.Errorf("post_agent seu não se mexe, veio %q", got)
		}
		if !strings.Contains(cfg.PostChoice().Steps[0].Task, "--mine") {
			t.Error("a task escrita à mão devia sobreviver")
		}
	})
}

// A pipeline é montada na página a partir dos agentes que já existem. Passo
// que não é agente, nome repetido e agente duas vezes na mesma sequência são
// recusados na criação — descobrir isso no meio de um review é tarde demais.
func TestAddPipeline(t *testing.T) {
	novo := func(t *testing.T) *Config {
		t.Helper()
		cfg := Default()
		for _, n := range []string{"history-pr", "review-fleet", "lazy-senior-dev"} {
			if _, err := cfg.AddAgentFromSkill(n, "", false); err != nil {
				t.Fatalf("montando a lista: %v", err)
			}
		}
		return cfg
	}

	t.Run("encadeia na ordem dada", func(t *testing.T) {
		cfg := novo(t)
		p, err := cfg.AddPipeline("  história e review  ", "lê o PR antes de revisar",
			[]string{"history-pr", "review-fleet"})
		if err != nil {
			t.Fatalf("AddPipeline: %v", err)
		}
		if p.Name != "história e review" {
			t.Errorf("o nome devia vir aparado: %q", p.Name)
		}
		ch, err := cfg.ChoiceByName("história e review")
		if err != nil {
			t.Fatalf("ChoiceByName: %v", err)
		}
		if !ch.Pipeline || len(ch.Steps) != 2 {
			t.Fatalf("devia ser uma pipeline de 2 passos: %+v", ch)
		}
		if ch.Steps[0].Name != "history-pr" || ch.Steps[1].Name != "review-fleet" {
			t.Errorf("a ordem escolhida é a ordem de execução: %v", ch.StepNames())
		}
	})

	t.Run("passo que não é agente", func(t *testing.T) {
		cfg := novo(t)
		if _, err := cfg.AddPipeline("x", "", []string{"history-pr", "exploit-digger"}); err == nil {
			t.Error("passo fora da lista de agentes devia ser recusado")
		}
	})

	t.Run("o mesmo agente duas vezes", func(t *testing.T) {
		cfg := novo(t)
		if _, err := cfg.AddPipeline("x", "", []string{"review-fleet", "review-fleet"}); err == nil {
			t.Error("o mesmo trabalho duas vezes sobre o mesmo clone devia ser recusado")
		}
	})

	t.Run("sem passo e sem nome", func(t *testing.T) {
		cfg := novo(t)
		if _, err := cfg.AddPipeline("x", "", nil); err == nil {
			t.Error("pipeline sem passo não roda nada")
		}
		if _, err := cfg.AddPipeline("  ", "", []string{"review-fleet"}); err == nil {
			t.Error("pipeline sem nome não entra no seletor")
		}
	})

	t.Run("nome que já existe", func(t *testing.T) {
		cfg := novo(t)
		if _, err := cfg.AddPipeline("review-fleet", "", []string{"history-pr", "review-fleet"}); err == nil {
			t.Error("nome de agente não pode virar nome de pipeline — o agent ganharia e a pipeline sumiria")
		}
		if _, err := cfg.AddPipeline("dupla", "", []string{"history-pr", "review-fleet"}); err != nil {
			t.Fatalf("AddPipeline: %v", err)
		}
		if _, err := cfg.AddPipeline("DUPLA", "", []string{"review-fleet", "lazy-senior-dev"}); err == nil {
			t.Error("nome repetido, mesmo com outra caixa, devia ser recusado")
		}
	})

	t.Run("remover", func(t *testing.T) {
		cfg := novo(t)
		if _, err := cfg.AddPipeline("dupla", "", []string{"history-pr", "review-fleet"}); err != nil {
			t.Fatalf("AddPipeline: %v", err)
		}
		if !cfg.RemovePipeline("DUPLA") {
			t.Error("remover não devia diferenciar caixa")
		}
		if cfg.RemovePipeline("dupla") {
			t.Error("remover duas vezes devia dizer que não achou")
		}
	})
}

// O padrão é quem roda quando ninguém escolhe, e uma pipeline também pode ser:
// no seletor os agents vêm antes, então ela nunca seria a primeira por posição.
func TestPipelinePodeSerOPadrao(t *testing.T) {
	cfg := Default()
	for _, n := range []string{"history-pr", "review-fleet"} {
		if _, err := cfg.AddAgentFromSkill(n, "", false); err != nil {
			t.Fatalf("montando a lista: %v", err)
		}
	}
	if _, err := cfg.AddPipeline("dupla", "", []string{"history-pr", "review-fleet"}); err != nil {
		t.Fatalf("AddPipeline: %v", err)
	}
	if got := cfg.DefaultChoice().Name; got != "history-pr" {
		t.Errorf("sem padrão marcado vale a primeira da lista, veio %q", got)
	}

	if !cfg.SetDefaultAgent("dupla") {
		t.Fatal("uma pipeline devia poder virar o padrão")
	}
	if got := cfg.DefaultChoice().Name; got != "dupla" {
		t.Errorf("a pipeline marcada devia ser o padrão, veio %q", got)
	}
	if got := cfg.Choices()[0].Name; got != "dupla" {
		t.Errorf("o padrão vem na frente da lista — é assim que a página o marca: %q", got)
	}
	// E a lista continua inteira, só reordenada.
	if len(cfg.Choices()) != 3 {
		t.Errorf("reordenar não podia perder escolha: %d", len(cfg.Choices()))
	}

	if cfg.SetDefaultAgent("fantasma") {
		t.Error("escolha que não existe não vira padrão")
	}

	// Removida a pipeline, o padrão não pode continuar apontando para ela.
	cfg.RemovePipeline("dupla")
	if cfg.Default != "" {
		t.Errorf("o padrão devia ter sido esquecido junto: %q", cfg.Default)
	}
	if got := cfg.DefaultChoice().Name; got != "history-pr" {
		t.Errorf("sem o padrão, volta a primeira da lista: %q", got)
	}
}

// `publish` é um passo que o Bazel executa, não um agente. As regras existem
// porque cada uma delas produz, em runtime, algo que parece bug.
func TestPassosReservados(t *testing.T) {
	novo := func(t *testing.T) *Config {
		t.Helper()
		cfg := Default()
		for _, n := range []string{"review-fleet", "history-pr"} {
			if _, err := cfg.AddAgentFromSkill(n, "", false); err != nil {
				t.Fatalf("montando a lista: %v", err)
			}
		}
		return cfg
	}

	t.Run("a sequência que o usuário pediu", func(t *testing.T) {
		cfg := novo(t)
		if _, err := cfg.AddPipeline("read before sending", "", []string{"review-fleet", "publish"}); err != nil {
			t.Fatalf("AddPipeline: %v", err)
		}
		ch, err := cfg.ChoiceByName("read before sending")
		if err != nil {
			t.Fatalf("ChoiceByName: %v", err)
		}
		if len(ch.Steps) != 2 {
			t.Fatalf("os dois passos deviam estar lá: %v", ch.StepNames())
		}
		if len(ch.Agents()) != 1 {
			t.Errorf("só um passo é agente de verdade: %d", len(ch.Agents()))
		}
		if ch.Posts {
			t.Error("o publish espera você mandar — a pipeline não escreve no PR sozinha")
		}
		if ch.Steps[1].Reserved != StepPublish {
			t.Errorf("o reservado devia vir marcado: %+v", ch.Steps)
		}
		if ch.StepPublishes("publish") {
			t.Error("passo reservado não tem corpo para publicar")
		}
	})

	t.Run("publish não pode abrir a pipeline", func(t *testing.T) {
		cfg := novo(t)
		if _, err := cfg.AddPipeline("x", "", []string{"publish", "review-fleet"}); err == nil {
			t.Error("publish no começo não teria o que mostrar")
		}
	})

	t.Run("pause não existe mais", func(t *testing.T) {
		cfg := novo(t)
		if _, err := cfg.AddPipeline("x", "", []string{"review-fleet", "pause", "publish"}); err == nil {
			t.Error("pause não é agente da lista nem passo reservado")
		}
	})

	t.Run("publish tem de ser o último", func(t *testing.T) {
		cfg := novo(t)
		if _, err := cfg.AddPipeline("x", "", []string{"review-fleet", "publish", "history-pr"}); err == nil {
			t.Error("revisar depois de publicar deixaria em disco um review diferente do que o time leu")
		}
		if _, err := cfg.AddPipeline("y", "", []string{"review-fleet", "publish", "publish"}); err == nil {
			t.Error("publicar duas vezes devia ser recusado")
		}
	})

	t.Run("publish sem nada publicável antes", func(t *testing.T) {
		cfg := novo(t)
		if err := cfg.SetAgentPublishable("history-pr", false); err != nil {
			t.Fatalf("SetAgentPublishable: %v", err)
		}
		if _, err := cfg.AddPipeline("x", "", []string{"history-pr", "publish"}); err == nil {
			t.Error("history-pr não produz review — não há o que publicar")
		}
		// Com um agente publicável na frente, passa.
		if _, err := cfg.AddPipeline("y", "", []string{"history-pr", "review-fleet", "publish"}); err != nil {
			t.Errorf("com um review no meio devia passar: %v", err)
		}
	})

	t.Run("agente não pode se chamar publish", func(t *testing.T) {
		cfg := Default()
		for _, n := range []string{"publish", "PUBLISH"} {
			if _, err := cfg.AddAgentFromSkill(n, "", false); err == nil {
				t.Errorf("%q é passo reservado — um agente assim tornaria a pipeline ambígua", n)
			}
		}
	})

	t.Run("agente removido deixa o reservado órfão", func(t *testing.T) {
		cfg := novo(t)
		if _, err := cfg.AddPipeline("x", "", []string{"review-fleet", "publish"}); err != nil {
			t.Fatalf("AddPipeline: %v", err)
		}
		// Sem o review-fleet, o publish fica sem nada para publicar.
		cfg.RemoveAgent("review-fleet")
		if ch, err := cfg.ChoiceByName("x"); err == nil && len(ch.Steps) != 0 {
			t.Errorf("o publish órfão devia sumir junto: %v", ch.StepNames())
		}
	})
}

// Um molde de `agent` sem {{task}} ganha a task prefixada — e se ele já tiver
// um comando escrito à mão, todo agente passa a rodar dois: o escolhido e o do
// molde. O agente parece funcionar, e só a conta de tokens denuncia.
func TestStrayCommand(t *testing.T) {
	casos := []struct {
		nome   string
		prompt string
		want   string
	}{
		{"molde de fábrica", defaultPrompt, ""},
		{"molde antigo com a skill escrita", "/review-fleet {{number}}\n\nO repositório {{repo}}…", "/review-fleet"},
		{"comando sem argumento", "/review-fleet\n\nrevise aí", "/review-fleet"},
		{"molde seu, sem comando", "Revise o PR {{number}} do {{repo}}.", ""},
		{"tem {{task}}, o comando é intencional", "{{task}}\n\nse precisar, /review-fleet {{number}}", ""},
		{"caminho não é comando", "leia /etc/hosts e revise", ""},
	}
	for _, c := range casos {
		cfg := Default()
		cfg.Agent.Prompt = c.prompt
		if got := cfg.StrayCommand(); got != c.want {
			t.Errorf("%s: StrayCommand() = %q, queria %q", c.nome, got, c.want)
		}
	}
}
