package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// O seletor mostra os agents na ordem do arquivo e depois as pipelines, com a
// primeira escolha valendo como padrão.
func TestChoicesOrderAndDefault(t *testing.T) {
	cfg := Default()
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

// Sem `agents:` no arquivo sobra a escolha única do bloco `agent` — que é como
// o Bazel se comportava antes do seletor.
func TestChoicesFallsBackToBaseAgent(t *testing.T) {
	cfg := Default()
	cfg.Agents = nil
	cfg.Pipelines = nil
	choices := cfg.Choices()
	if len(choices) != 1 || len(choices[0].Steps) != 1 {
		t.Fatalf("esperava uma escolha de um passo, vieram %+v", choices)
	}
	if choices[0].Steps[0].Command != cfg.Agent.Command {
		t.Error("a escolha única devia ser o próprio bloco agent")
	}
}

// Config antigo, escrito quando o /review-fleet vivia colado no prompt, ganha
// as lentes padrão. Prompt customizado fica como está.
func TestLoadBackfillsAgents(t *testing.T) {
	t.Run("prompt padrão antigo", func(t *testing.T) {
		cfg := loadFrom(t, "repos: [acme/api-core]\nagent:\n  command: claude\n  prompt: |-\n"+indent(legacyPrompt))
		if len(cfg.Agents) != len(defaultAgents()) {
			t.Fatalf("esperava as lentes padrão, vieram %d", len(cfg.Agents))
		}
		if cfg.Agent.Prompt != defaultPrompt {
			t.Error("o molde antigo devia virar o novo, com {{task}}")
		}
	})

	t.Run("prompt customizado", func(t *testing.T) {
		cfg := loadFrom(t, "repos: [acme/api-core]\nagent:\n  command: claude\n  prompt: revise o PR {{number}}\n")
		if len(cfg.Agents) != 0 {
			t.Errorf("prompt customizado não devia ganhar agents padrão: %+v", cfg.Agents)
		}
		if len(cfg.Choices()) != 1 {
			t.Error("e o seletor devia mostrar só a escolha única")
		}
	})
}

// O agente que publica sozinho é declarado como tal — a interface precisa
// disso para avisar antes de escrever no PR de outra pessoa.
func TestPostingAgentIsMarked(t *testing.T) {
	cfg := Default()
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
	if strings.Contains(post.Steps[0].Prompt, "não publique nada no GitHub") {
		t.Error("o molde dele não pode proibir publicar")
	}

	plain, _ := cfg.ChoiceByName("review-fleet")
	if plain.Posts {
		t.Error("a frota normal não publica sozinha")
	}
	if !strings.Contains(plain.Steps[0].Prompt, "não publique nada no GitHub") {
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
	if !strings.Contains(post.Steps[0].Prompt, "Não refaça o review") {
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
	if fromDisk.PostChoice().Name != "post-report" {
		t.Errorf("config sem post_agent devia cair no padrão, veio %q", fromDisk.PostChoice().Name)
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

func indent(s string) string {
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}
