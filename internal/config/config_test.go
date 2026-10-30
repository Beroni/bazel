package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
	"testing"

	"github.com/beroni/bazel/internal/pricing"
)

// A new config is born with no agent at all: the list is built on the page,
// from the installed skills — there is no factory list guessing what this
// computer has.
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
	// And the publishing agent is still there: it is not a choice in the
	// list, it is what runs when you order an already read review published.
	if cfg.PostChoice().Name != "bazel-post-report" {
		t.Errorf("o post_agent devia continuar no padrão, veio %q", cfg.PostChoice().Name)
	}
}

// The selector shows the agents in file order and then the pipelines, with the
// first choice counting as the default.
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

// Adding a skill is the normal way to build the list: the agent comes out with
// the skill's name and the task that invokes it over the PR.
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

	// The same skill can also become an agent that publishes on its own: the
	// name changes, and it is the suffix that lets the two live together.
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

// Taking off the list and changing the default are the other two tweaks the
// page makes.
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

// A named agent inherits command, args, checkout and timeout from the `agent`
// block.
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
	// claude's args cannot come from another executable's block.
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

// A step pointing to a nonexistent agent is ignored; a pipeline with no valid
// step does not even show up.
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

// Without `agents:` but with a prompt of your own, the single choice of the
// `agent` block is left — which is how Bazel behaved before the selector. With
// the factory template nothing is left: it is a shell around an agent's
// {{task}}.
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

// What is in the file is what counts: Load does not invent an agent for anyone.
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

// The agent that publishes on its own is declared as such — the interface needs
// that to warn before writing on someone else's PR.
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

// A pipeline inherits the publishing warning from any of its steps.
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

// Old factory args get stream mode; custom args stay.
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

// PublishesBy finds the publishing paths that Posts does not declare, without
// mistaking a flag similar to --post for it.
func TestChoicePublishesBy(t *testing.T) {
	for _, c := range []struct {
		nome string
		ch   Choice
		quer bool
	}{
		{"lente comum", Choice{Steps: []ResolvedAgent{{Name: "l", Task: "/review {{number}}", Prompt: defaultPrompt}}}, false},
		{"flag parecida", Choice{Steps: []ResolvedAgent{{Name: "l", Task: "/review --poster"}}}, false},
		{"posts", Choice{Steps: []ResolvedAgent{{Name: "p", Posts: true}}}, true},
		{"--post", Choice{Steps: []ResolvedAgent{{Name: "p", Task: "/review {{number}} --post"}}}, true},
		{"--post=inline", Choice{Steps: []ResolvedAgent{{Name: "p", Task: "/review --post=inline"}}}, true},
		{"passo publish", Choice{Steps: []ResolvedAgent{{Name: "l"}, {Name: StepPublish, Reserved: StepPublish}}}, true},
		{"postPrompt", Choice{Steps: []ResolvedAgent{{Name: "p", Prompt: postPrompt}}}, true},
		{"publishPrompt", Choice{Steps: []ResolvedAgent{{Name: "p", Prompt: publishPrompt}}}, true},
		{"bloco agent sem proibição", Choice{Bare: true, Steps: []ResolvedAgent{{Name: "default", Prompt: "revise"}}}, true},
		{"bloco agent com o molde padrão", Choice{Bare: true, Steps: []ResolvedAgent{{Name: "default", Prompt: defaultPrompt}}}, false},
	} {
		if got := c.ch.PublishesBy() != ""; got != c.quer {
			t.Errorf("%s: PublishesBy = %q, queria publicar = %v", c.nome, c.ch.PublishesBy(), c.quer)
		}
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

// The publishing agent is what runs after you read the review: it clones by
// default (inline needs the diff) and carries the review file in the prompt.
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

	// An explicit checkout: false is respected.
	no := false
	cfg.PostAgent.Checkout = &no
	if cfg.PostChoice().Steps[0].Checkout {
		t.Error("checkout: false no post_agent devia valer")
	}

	// A config without post_agent gets the default.
	fromDisk := loadFrom(t, "repos: [acme/api]\n")
	if fromDisk.PostChoice().Name != "bazel-post-report" {
		t.Errorf("config sem post_agent devia cair no padrão, veio %q", fromDisk.PostChoice().Name)
	}
}

// The publishing skill does not become an agent: in an ordinary step it runs
// without the review or the publish prompt, and posts nothing. The one that
// publishes is the publish step — and it is the one that stops for you to
// choose what goes, which is why pause is gone.
func TestMigraPipelines(t *testing.T) {
	cfg := Default()
	for _, posts := range []bool{false, true} {
		if _, err := cfg.AddAgentFromSkill("bazel-post-report", "", posts); err == nil {
			t.Errorf("a skill de publicação não podia virar agente (posts=%v)", posts)
		}
	}

	// A config saved before this: the agent leaves and the step becomes
	// publish.
	old := loadFrom(t, `repos: [acme/api]
agents:
  - name: review-fleet
    task: /review-fleet {{number}}
  - name: bazel-post-report
    task: /bazel-post-report {{number}}
pipelines:
  - name: gravada
    steps: [review-fleet, pause, bazel-post-report]
  - name: pausada
    steps: [review-fleet, pause, publish]
  - name: dupla
    steps: [review-fleet, bazel-post-report, publish]
default: bazel-post-report
`)
	if len(old.Agents) != 1 || old.Agents[0].Name != "review-fleet" {
		t.Errorf("o agente de publicação devia sair da lista: %+v", old.Agents)
	}
	if got := strings.Join(old.Pipelines[0].Steps, ","); got != "review-fleet,publish" {
		t.Errorf("o passo devia virar publish, e a pausa sair: %s", got)
	}
	if got := strings.Join(old.Pipelines[1].Steps, ","); got != "review-fleet,publish" {
		t.Errorf("a pausa devia sair: %s", got)
	}
	if got := strings.Join(old.Pipelines[2].Steps, ","); got != "review-fleet,publish" {
		t.Errorf("publicar uma vez só: %s", got)
	}
	if old.Default != "" {
		t.Errorf("o padrão apontava para o agente removido: %q", old.Default)
	}

	var avisos bytes.Buffer
	warnOut = &avisos
	t.Cleanup(func() { warnOut = os.Stderr })

	t.Run("pausa antes de quem posta sozinho", func(t *testing.T) {
		avisos.Reset()
		cfg := loadFrom(t, migraPortao)
		if !strings.Contains(avisos.String(), "sem-portao-antes") {
			t.Errorf("o Load devia avisar no terminal o que descartou: %q", avisos.String())
		}
		if len(cfg.Agents) != 3 {
			t.Errorf("quem posta sozinho continua na lista: %+v", cfg.Agents)
		}
		want := map[string]string{
			"portao":    "review-fleet,publish",
			"longe":     "review-fleet,history-pr,publish",
			"sem-pausa": "review-fleet,security-audit-post",
			// An agent that posts on its own before publish was already posting
			// with no gate before the upgrade: publish goes, instead of becoming
			// a second post.
			"posta-e-publica":      "review-fleet,security-audit-post",
			"posta-antes-da-pausa": "security-audit-post,review-fleet",
		}
		if got := passosPorNome(cfg); !sameMap(got, want) {
			t.Errorf("a pausa antes de quem posta devia virar publish:\n got %v\nwant %v", got, want)
		}
		if cfg.Default != "" {
			t.Errorf("o padrão apontava para a pipeline descartada: %q", cfg.Default)
		}
		// The pipeline that was dropped, the two that lost their publish step
		// because an agent already posts before it, and the two whose posting
		// agent after a pause became the publish step.
		avisados := map[string]bool{}
		for _, w := range cfg.Warnings {
			for _, nome := range []string{"sem-portao-antes", "posta-e-publica", "posta-antes-da-pausa", "portao", "longe"} {
				if strings.Contains(w, `"`+nome+`"`) {
					avisados[nome] = true
				}
			}
		}
		if len(cfg.Warnings) != 5 || len(avisados) != 5 {
			t.Errorf("descartar uma pipeline, ou o publish dela, ou trocar quem postava pelo publish tem de avisar: %q", cfg.Warnings)
		}
		virou := `pipeline "portao": "security-audit-post" (after a pause) became the publish step and no longer runs`
		if !strings.Contains(strings.Join(cfg.Warnings, "\n"), virou) {
			t.Errorf("faltou o aviso %q: %q", virou, cfg.Warnings)
		}
	})

	t.Run("publish descartado porque alguém já posta", func(t *testing.T) {
		avisos.Reset()
		cfg := loadFrom(t, `repos: [acme/api]
agents:
  - name: x-post
    task: /x {{number}} --post
    posts: true
  - name: y-post
    task: /y {{number}} --post
    posts: true
pipelines:
  - name: duas-postagens
    steps: [x-post, pause, y-post]
`)
		if got := passosPorNome(cfg)["duas-postagens"]; got != "x-post" {
			t.Fatalf("o passo que postava depois da pausa devia sair sem virar publish: %q", got)
		}
		// One line for y-post turning into the publish step, one for that
		// publish being dropped because x-post already posts.
		if len(cfg.Warnings) != 2 {
			t.Fatalf("queria dois avisos: %q", cfg.Warnings)
		}
		if w := cfg.Warnings[0]; !strings.Contains(w, `"duas-postagens"`) || !strings.Contains(w, `"y-post" (after a pause) became the publish step`) {
			t.Errorf("trocar quem postava depois da pausa tem de avisar: %q", w)
		}
		if w := cfg.Warnings[1]; !strings.Contains(w, `"duas-postagens"`) || !strings.Contains(w, "x-post") {
			t.Errorf("descartar o publish tem de avisar, com o nome da pipeline e de quem posta: %q", w)
		}
		if !strings.Contains(avisos.String(), "duas-postagens") {
			t.Errorf("o Load devia avisar no terminal: %q", avisos.String())
		}
	})

	t.Run("publicação fora do fim", func(t *testing.T) {
		cfg := loadFrom(t, migraForaDoFim)
		want := map[string]string{
			"meio":             "review-fleet,history-pr,publish",
			"primeiro":         "review-fleet,publish",
			"publish-solto":    "review-fleet,history-pr,publish",
			"so-historia":      "history-pr",
			"historia-publish": "history-pr",
		}
		if got := passosPorNome(cfg); !sameMap(got, want) {
			t.Errorf("o publish vai uma vez, no fim, e só com review antes:\n got %v\nwant %v", got, want)
		}
		if cfg.Default != "" {
			t.Errorf("o padrão apontava para a pipeline que ficou vazia: %q", cfg.Default)
		}
		if len(cfg.Warnings) != 2 {
			t.Errorf("as duas pipelines vazias deviam avisar: %q", cfg.Warnings)
		}
	})

	t.Run("revisor com a skill do post_agent fica", func(t *testing.T) {
		cfg := loadFrom(t, `repos: [acme/api]
post_agent:
  name: frota-publica
  task: /review-fleet {{review_file}} --post
agents:
  - name: review-fleet
    task: /review-fleet {{number}}
  - name: lazy
    task: /lazy {{number}}
  - name: frota-copia
    task: "/review-fleet   {{number}} --post"
    posts: true
pipelines:
  - name: dupla
    steps: [review-fleet, lazy]
  - name: copia
    steps: [review-fleet, frota-copia]
`)
		var nomes []string
		for _, a := range cfg.Agents {
			nomes = append(nomes, a.Name)
		}
		if got := strings.Join(nomes, ","); got != "review-fleet,lazy" {
			t.Errorf("só sai quem tem a task do post_agent, não quem divide a skill: %s", got)
		}
		want := map[string]string{"dupla": "review-fleet,lazy", "copia": "review-fleet,publish"}
		if got := passosPorNome(cfg); !sameMap(got, want) {
			t.Errorf("\n got %v\nwant %v", got, want)
		}
	})

	t.Run("agente da skill embarcada sai mesmo sem posts", func(t *testing.T) {
		cfg := loadFrom(t, `repos: [acme/api]
post_agent:
  name: frota-publica
  task: /review-fleet {{review_file}} --post
agents:
  - name: review-fleet
    task: /review-fleet {{number}}
  - name: publica-embarcada
    task: /bazel-post-report {{review_file}}
pipelines:
  - name: gravada
    steps: [review-fleet, publica-embarcada]
`)
		if len(cfg.Agents) != 1 || cfg.Agents[0].Name != "review-fleet" {
			t.Errorf("quem chama a skill de publicação embarcada é agente de publicação: %+v", cfg.Agents)
		}
		if got := passosPorNome(cfg)["gravada"]; got != "review-fleet,publish" {
			t.Errorf("o passo devia virar publish: %q", got)
		}
	})

	t.Run("ida e volta", func(t *testing.T) {
		for _, src := range []string{migraPortao, migraForaDoFim} {
			first := loadFrom(t, src)
			if err := first.Save(); err != nil {
				t.Fatalf("Save: %v", err)
			}
			second, err := Load()
			if err != nil {
				t.Fatalf("Load depois do Save: %v", err)
			}
			if a, b := passosPorNome(first), passosPorNome(second); !sameMap(a, b) {
				t.Errorf("a segunda carga mudou as pipelines:\n 1ª %v\n 2ª %v", a, b)
			}
			if first.Default != second.Default || len(first.Agents) != len(second.Agents) {
				t.Errorf("a segunda carga mudou o resto: %q/%q, %d/%d agents",
					first.Default, second.Default, len(first.Agents), len(second.Agents))
			}
			if len(second.Warnings) != 0 {
				t.Errorf("o que já foi migrado não avisa de novo: %q", second.Warnings)
			}
			for _, p := range second.Pipelines {
				if _, err := second.resolveSteps(p.Steps); err != nil {
					t.Errorf("%s gravada num formato que a página recusa (%v): %v", p.Name, p.Steps, err)
				}
				ch, err := second.ChoiceByName(p.Name)
				if err != nil {
					t.Errorf("%s sumiu do seletor: %v", p.Name, err)
					continue
				}
				if got, want := strings.Join(ch.StepNames(), ","), strings.Join(p.Steps, ","); got != want {
					t.Errorf("%s roda %s, mas está gravada como %s", p.Name, got, want)
				}
			}
		}
	})
}

const migraPortao = `repos: [acme/api]
agents:
  - name: review-fleet
    task: /review-fleet {{number}}
  - name: history-pr
    task: /history-pr {{number}}
    publishable: false
  - name: security-audit-post
    task: /security-audit {{number}} --post
    posts: true
pipelines:
  - name: portao
    steps: [review-fleet, pause, security-audit-post]
  - name: longe
    steps: [review-fleet, pause, history-pr, security-audit-post]
  - name: sem-portao-antes
    steps: [history-pr, pause, security-audit-post]
  - name: sem-pausa
    steps: [review-fleet, security-audit-post]
  - name: posta-e-publica
    steps: [review-fleet, security-audit-post, publish]
  - name: posta-antes-da-pausa
    steps: [security-audit-post, review-fleet, pause, publish]
default: sem-portao-antes
`

const migraForaDoFim = `repos: [acme/api]
agents:
  - name: review-fleet
    task: /review-fleet {{number}}
  - name: history-pr
    task: /history-pr {{number}}
    publishable: false
  - name: bazel-post-report
    task: /bazel-post-report {{number}}
pipelines:
  - name: meio
    steps: [review-fleet, bazel-post-report, history-pr]
  - name: primeiro
    steps: [bazel-post-report, review-fleet]
  - name: publish-solto
    steps: [review-fleet, publish, history-pr]
  - name: so-historia
    steps: [history-pr, bazel-post-report]
  - name: historia-publish
    steps: [history-pr, publish]
  - name: vazia
    steps: [bazel-post-report, pause]
  - name: so-publish
    steps: [publish]
default: vazia
`

func passosPorNome(cfg *Config) map[string]string {
	out := make(map[string]string, len(cfg.Pipelines))
	for _, p := range cfg.Pipelines {
		out[p.Name] = strings.Join(p.Steps, ",")
	}
	return out
}

func sameMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
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

// Not every skill produces a review. The one that does not — history-pr tells a
// PR's story — cannot have a road to GitHub: publishing it would tell the team
// the code was reviewed when nobody reviewed anything.
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
		// And the tick comes back.
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

// The publishing skill now travels in the binary. Whoever has the old default
// untouched — which called the /post-report from ~/.claude/skills and only
// worked for whoever had installed it — gets the embedded one; whoever edited
// theirs keeps what they wrote.
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

// The pipeline is built on the page from the agents that already exist. A step
// that is not an agent, a repeated name and an agent twice in the same sequence
// are refused at creation — finding that out in the middle of a review is too
// late.
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

// The default is who runs when nobody chooses, and a pipeline can be it too: in
// the selector the agents come first, so it would never be first by position.
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
	// And the list is still whole, just reordered.
	if len(cfg.Choices()) != 3 {
		t.Errorf("reordenar não podia perder escolha: %d", len(cfg.Choices()))
	}

	if cfg.SetDefaultAgent("fantasma") {
		t.Error("escolha que não existe não vira padrão")
	}

	// With the pipeline removed, the default cannot keep pointing to it.
	cfg.RemovePipeline("dupla")
	if cfg.Default != "" {
		t.Errorf("o padrão devia ter sido esquecido junto: %q", cfg.Default)
	}
	if got := cfg.DefaultChoice().Name; got != "history-pr" {
		t.Errorf("sem o padrão, volta a primeira da lista: %q", got)
	}
}

// `publish` is a step Bazel runs, not an agent. The rules exist because each of
// them produces, at runtime, something that looks like a bug.
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
		// With a publishable agent in front, it passes.
		if _, err := cfg.AddPipeline("y", "", []string{"history-pr", "review-fleet", "publish"}); err != nil {
			t.Errorf("com um review no meio devia passar: %v", err)
		}
	})

	t.Run("quem posta sozinho não vai antes do publish", func(t *testing.T) {
		cfg := novo(t)
		cfg.Agents = append(cfg.Agents, AgentDef{Name: "frota-post", Task: "/review-fleet {{number}} --post", Posts: true})
		for i, steps := range [][]string{
			{"frota-post", "publish"},
			{"review-fleet", "frota-post", "publish"},
		} {
			_, err := cfg.AddPipeline(fmt.Sprintf("x%d", i), "", steps)
			if err == nil {
				t.Errorf("%v: o agente já postou quando o publish pararia para você escolher", steps)
				continue
			}
			if !strings.Contains(err.Error(), "frota-post") {
				t.Errorf("%v: a mensagem devia dizer quem posta sozinho: %v", steps, err)
			}
		}
		// Without publish, an agent that posts on its own can still go in.
		if _, err := cfg.AddPipeline("y", "", []string{"review-fleet", "frota-post"}); err != nil {
			t.Errorf("sem publish devia passar: %v", err)
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
		// Without review-fleet, publish is left with nothing to publish.
		cfg.RemoveAgent("review-fleet")
		// Only publish is left, with nothing to publish: the whole pipeline
		// leaves the selector.
		if ch, err := cfg.ChoiceByName("x"); err == nil {
			t.Errorf("a pipeline só com o publish órfão devia sair do seletor, veio %v", ch.StepNames())
		}
		if got := cfg.Choices(); len(got) != 1 || got[0].Name != "history-pr" {
			t.Errorf("só o history-pr devia continuar no seletor: %v", got)
		}
	})
}

// An `agent` template without {{task}} gets the task prefixed — and if it
// already has a handwritten command, every agent starts running two: the chosen
// one and the template's. The agent seems to work, and only the token bill
// gives it away.
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

// A price in config.yaml replaces only that model: the others keep the
// default, and the package default is not touched by the user's yaml.
func TestPricingDoUsuarioSobrescreveSemMexerNoDefault(t *testing.T) {
	cfg := loadFrom(t, "agent:\n  pricing:\n    Claude-Opus-5-5:\n      input: 99\n    meu-modelo:\n      input: 1\n")
	prices := cfg.Agent.Prices()
	// A key in a different case replaces the price instead of competing with
	// the default.
	for range 50 {
		if r, _ := prices.Rate("claude-opus-5-5"); r.Input != 99 {
			t.Fatalf("preço do usuário não entrou: %.2f", r.Input)
		}
	}
	if _, ok := prices.Rate("claude-haiku-4-5"); !ok {
		t.Error("os outros modelos do default sumiram")
	}
	if got := pricing.Default["claude-opus-5-5"].Input; got != 4 {
		t.Errorf("o yaml do usuário escreveu no pricing.Default: %.2f", got)
	}
	if _, ok := pricing.Default["meu-modelo"]; ok {
		t.Error("modelo do usuário vazou para o pricing.Default")
	}
}

// Earlier versions wrote the whole default table into config.yaml, with wrong
// prices. Those lines cannot win over the corrected table, and Save no longer
// writes the default — only what the user wrote.
func TestPricingDefaultAntigoNaoVence(t *testing.T) {
	cfg := loadFrom(t, "agent:\n  pricing:\n"+
		"    claude-opus-5: {input: 15, output: 75, cache_write: 1.5, cache_read: 0.75}\n"+
		"    claude-sonnet-4: {input: 3, output: 15, cache_write: 0.3, cache_read: 0.15}\n"+
		"    claude-haiku-4-5: {input: 2, output: 5, cache_write: 1.25, cache_read: 0.1}\n")
	prices := cfg.Agent.Prices()
	if r, _ := prices.Rate("claude-opus-5-20260101"); r.Input != 5 || r.CacheWrite != 6.25 {
		t.Errorf("linha antiga do opus-5 venceu: %+v", r)
	}
	if r, _ := prices.Rate("claude-sonnet-4-5"); r.CacheWrite != 3.75 {
		t.Errorf("linha antiga do sonnet-4 venceu: %+v", r)
	}
	// A line the user changed is not a leftover of the default: it stays.
	if r, _ := prices.Rate("claude-haiku-4-5"); r.Input != 2 {
		t.Errorf("preço editado pelo usuário sumiu: %+v", r)
	}

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path, _ := Path()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("lendo config: %v", err)
	}
	if strings.Contains(string(data), "claude-opus-5-5") || strings.Contains(string(data), "claude-opus-5:") {
		t.Errorf("o Save gravou o default no config.yaml:%s\n%s", "", data)
	}
	if !strings.Contains(string(data), "claude-haiku-4-5") {
		t.Errorf("o Save perdeu o preço do usuário:\n%s", data)
	}
}

// A watched repo is exactly owner/name (BUG-QA-03). A GitHub repository URL is
// trimmed down to that; a PR URL, a missing half or a deeper path is refused.
func TestNormalizeRepo(t *testing.T) {
	for in, want := range map[string]string{
		"acme/web-app":                        "acme/web-app",
		"  acme/web-app  ":                    "acme/web-app",
		"https://github.com/acme/web-app":     "acme/web-app",
		"https://github.com/acme/web-app/":    "acme/web-app",
		"https://github.com/acme/web-app.git": "acme/web-app",
		"github.com/Beroni/bazel":             "Beroni/bazel",
		"acme/my.repo_v2":                     "acme/my.repo_v2",
		"HTTPS://GitHub.com/acme/web-app":     "acme/web-app",
		"https://www.github.com/acme/web-app": "acme/web-app",
		"git@github.com:acme/web-app.git":     "acme/web-app",
		"a-b/c":                               "a-b/c",
		"acme/.github":                        "acme/.github",
	} {
		if got, err := NormalizeRepo(in); err != nil || got != want {
			t.Errorf("NormalizeRepo(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "acme", "acme/", "/web-app", "acme/web-app/tree/main", "acme/web app",
		"-acme/web-app", "acme/..", "https://gitlab.com/acme/web-app",
		"a/-x", "a/...", "a--b/c", "a-/c", "https://github.com/github.com/a/b",
		"git@gitlab.com:acme/web-app.git", strings.Repeat("a", 40) + "/b",
	} {
		if got, err := NormalizeRepo(in); err == nil {
			t.Errorf("NormalizeRepo(%q) = %q, want an error", in, got)
		}
	}
	_, err := NormalizeRepo("https://github.com/acme/web-app/pull/7")
	if err == nil || !strings.Contains(err.Error(), "pull request") || !strings.Contains(err.Error(), "acme/web-app") {
		t.Errorf("a PR URL must be refused naming its repository, got %v", err)
	}
}

// The History tab's agent: /history-pr by default, never publishable, with a
// clone; history_agent in the config replaces the task, and an untouched one
// is not written to config.yaml.
func TestHistoryChoice(t *testing.T) {
	c := Default()
	ch := c.HistoryChoice()
	if len(ch.Steps) != 1 || ch.Steps[0].Task != defaultHistoryTask || ch.Publishable || ch.Posts || !ch.Steps[0].Checkout {
		t.Errorf("default history choice: %+v", ch)
	}
	if PublishesBy := ch.PublishesBy(); PublishesBy != "" {
		t.Errorf("the history agent must never publish: %s", PublishesBy)
	}
	c.HistoryAgent = AgentDef{Name: "story", Task: "/bazel-history-pr {{number}}"}
	if ch := c.HistoryChoice(); ch.Name != "story" || ch.Steps[0].Task != "/bazel-history-pr {{number}}" {
		t.Errorf("configured history choice: %+v", ch)
	}
	c.HistoryAgent = AgentDef{}
	data, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "history_agent") {
		t.Errorf("an untouched history_agent should not be written:\n%s", data)
	}
}
