// Package config carrega e persiste a configuração do Bazel (~/.bazel/config.yaml).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/beroni/bazel/internal/pricing"
	"gopkg.in/yaml.v3"
)

// Config é o arquivo de configuração do usuário.
type Config struct {
	// Repos monitorados, no formato "owner/repo".
	Repos []string `yaml:"repos"`
	// Authors filtra os PRs por autor (logins do GitHub). Vazio = todos.
	Authors []string `yaml:"authors"`
	// IncludeDrafts inclui PRs em rascunho na listagem.
	IncludeDrafts bool `yaml:"include_drafts"`
	// ReviewsDir é onde os reviews em markdown são salvos.
	// Vazio = <BAZEL_HOME>/reviews.
	ReviewsDir string `yaml:"reviews_dir"`
	// MaxDiffBytes trunca diffs gigantes antes de mandar pro agent.
	MaxDiffBytes int `yaml:"max_diff_bytes"`
	// SkillsDir é onde estão as skills do agente. Vazio = ~/.claude/skills.
	SkillsDir string `yaml:"skills_dir"`

	// Agent é a base: comando, args e o molde do prompt usados por todo
	// agente que não sobrescrever esses campos.
	Agent Agent `yaml:"agent"`
	// Agents são as lentes que podem rodar sobre um PR. A primeira é a padrão.
	Agents []AgentDef `yaml:"agents"`
	// Pipelines encadeiam agentes sobre o mesmo clone, na ordem dada.
	Pipelines []Pipeline `yaml:"pipelines"`
	// Default é o nome da escolha que roda quando ninguém escolhe. Vazio = a
	// primeira da lista. Existe porque uma pipeline também pode ser o padrão,
	// e ela não tem como ser a primeira: no seletor os agents vêm antes.
	Default string `yaml:"default,omitempty"`
	// PostAgent é quem publica um review que você já leu. É o outro caminho
	// para o PR: em vez de escolher um agente que publica sozinho, você roda
	// a frota, lê o resultado e só então manda publicar.
	PostAgent AgentDef `yaml:"post_agent"`
}

// AgentDef é um agente nomeado que aparece no seletor da TUI.
type AgentDef struct {
	Name string `yaml:"name"`
	// Description é a linha que explica o agente no seletor.
	Description string `yaml:"description,omitempty"`
	// Task é a instrução específica desta lente. Entra no {{task}} do molde
	// de agent.prompt — é só isso que muda entre um agente e outro.
	Task string `yaml:"task,omitempty"`
	// Prompt substitui o molde inteiro quando preenchido. Aceita os mesmos
	// placeholders de agent.prompt.
	Prompt string `yaml:"prompt,omitempty"`
	// Command e Args sobrescrevem os de agent quando preenchidos — serve para
	// rodar uma lente em outro modelo ou em outro executável.
	Command string   `yaml:"command,omitempty"`
	Args    []string `yaml:"args,omitempty"`
	// Format descreve o protocolo de saída do agente. Vazio preserva a
	// detecção legada pelos args (Claude stream-json, ou saída crua).
	Format string `yaml:"format,omitempty"`
	// Posts marca o agente que publica o review no PR sozinho, em vez de
	// devolver o texto para o Bazel publicar depois. A interface avisa antes
	// de rodar um desses: é escrita em PR de outra pessoa.
	Posts bool `yaml:"posts,omitempty"`
	// Publishable diz se a saída deste agente pode ir para o PR. nil = pode,
	// que é como todo agente sempre se comportou. Desligar é o que separa um
	// agente que produz review de um que produz outra coisa — a história de um
	// PR, por exemplo. Publicar uma narrativa de processo diria ao time que o
	// código foi revisado quando ninguém revisou nada.
	Publishable *bool `yaml:"publishable,omitempty"`
	// Checkout sobrescreve agent.checkout. nil = herda.
	Checkout *bool `yaml:"checkout,omitempty"`
	// TimeoutSeconds sobrescreve agent.timeout_seconds. 0 = herda.
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty"`
	// Env is merged into this agent's process environment. Empty inherits
	// agent.env; that empty too means the Bazel process environment only.
	Env map[string]string `yaml:"env,omitempty"`
}

// Pipeline é uma sequência de agentes rodada sobre o mesmo clone do PR.
type Pipeline struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	// Steps são nomes de agents, na ordem de execução.
	Steps []string `yaml:"steps"`
}

// Passos reservados de uma pipeline. Não são agentes: o Bazel os executa por
// conta própria, e é por isso que ninguém pode ter um agente com esses nomes.
//
//	pause   — para a sequência e espera você ler o que saiu até ali. O clone
//	          fica de pé, e a fila não: o worker sai e outro review roda.
//	publish — leva ao PR o relatório produzido até aqui, com o mesmo agente de
//	          publicação do botão. Vindo depois de um pause, é "li e aprovei"
//	          declarado na pipeline em vez de clicado.
const (
	StepPause   = "pause"
	StepPublish = "publish"
)

// ReservedStep diz se um nome de passo é executado pelo Bazel em vez de ser um
// agente da lista.
func ReservedStep(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case StepPause, StepPublish:
		return true
	}
	return false
}

// ResolvedAgent é um AgentDef com os campos herdados de agent já preenchidos.
type ResolvedAgent struct {
	Name           string
	Description    string
	Command        string
	Args           []string
	Format         string
	Prompt         string
	Task           string
	Posts          bool
	Publishable    bool
	Checkout       bool
	TimeoutSeconds int
	Env            map[string]string
	// Reserved é o nome do passo reservado quando este não é um agente de
	// verdade — `pause` ou `publish`. Vazio no resto.
	Reserved string
}

// Choice é o que o usuário escolhe antes do review: um agente sozinho ou uma
// pipeline inteira. Nos dois casos o que roda é a lista de Steps.
type Choice struct {
	Name        string
	Description string
	// Pipeline diz se essa escolha encadeia mais de um agente.
	Pipeline bool
	// Posts diz se algum passo publica no PR por conta própria.
	Posts bool
	// Publishable diz se há alguma coisa aqui que possa ir ao PR: pelo menos
	// um passo publicável. Numa pipeline, só o corpo desses passos é que sobe
	// — o resto você lê na tela e fica por aí.
	Publishable bool
	Steps       []ResolvedAgent
}

// Agent descreve como invocar o agente de IA que faz o review.
type Agent struct {
	// Command é o executável (ex.: "claude").
	Command string `yaml:"command"`
	// Args são os argumentos fixos. O prompt vai pelo stdin.
	Args []string `yaml:"args"`
	// Format identifica o protocolo de saída estruturada do agente. Vazio
	// preserva a detecção legada pelos args.
	Format string `yaml:"format,omitempty"`
	// Checkout clona o repositório numa pasta temporária e faz o checkout do
	// PR antes de rodar o agente, que roda com essa pasta como diretório de
	// trabalho. Necessário para agentes que leem o código, e não só o diff.
	Checkout bool `yaml:"checkout"`
	// Prompt é o template do review. Placeholders: {{repo}}, {{number}},
	// {{title}}, {{author}}, {{url}}, {{branch}}, {{base}}, {{body}},
	// {{workdir}}, {{diff}}. O diff só é baixado se o template pedir.
	Prompt string `yaml:"prompt"`
	// TimeoutSeconds limita a duração de um review. 0 = sem limite.
	TimeoutSeconds int `yaml:"timeout_seconds"`
	// Env is merged into every agent process that does not set its own env.
	Env map[string]string `yaml:"env,omitempty"`
	// Pricing é a tabela de preços por modelo, em USD por 1M de tokens.
	// Vazia = usar o default shipped com o código. Os preços mudam, e o
	// binário não pode precisar de rebuild.
	Pricing PricingTable `yaml:"pricing,omitempty"`
}

// Rate é o custo por 1M de tokens, em USD, para uma categoria de uso.
type Rate = pricing.Rate

// PricingTable é a tabela de preços por modelo, em USD por 1M de tokens.
// Chave: nome do modelo (case-insensitive). Valor: rate por categoria.
type PricingTable = pricing.Table

const defaultPrompt = `{{task}}

The repository {{repo}} is cloned in this directory ({{workdir}}), with PR
#{{number}} already checked out on branch ` + "`{{branch}}`" + ` over base ` + "`{{base}}`" + `.
This is a throwaway clone: read freely, but do not edit files, do not commit,
and do not publish anything to GitHub.

PR: {{title}} — @{{author}}
{{url}}

PR description:
{{body}}

Write only the final report to stdout, in Markdown.`

// postPrompt é o molde do agente que publica sozinho. O molde padrão proíbe
// escrever no GitHub — este troca essa linha pela permissão explícita, que é
// justamente o que a skill de post precisa.
const postPrompt = `{{task}}

The repository {{repo}} is cloned in this directory ({{workdir}}), with PR
#{{number}} already checked out on branch ` + "`{{branch}}`" + ` over base ` + "`{{base}}`" + `.
This is a throwaway clone: read freely, but do not edit files and do not commit.

You **are** authorized to publish the result on PR #{{number}} of
{{repo}} — that is why this review was started. Publish a single review,
with inline comments on the right lines, and do not open another if you
already have one on the PR.

PR: {{title}} — @{{author}}
{{url}}

PR description:
{{body}}

Write the final report to stdout in Markdown, and say at the end what was
published on the PR.`

// publishPrompt é o molde de quem publica um review já escrito. A instrução
// que importa é a última: não refazer o review, publicar o que está no arquivo
// — é ele que você leu na tela antes de mandar.
const publishPrompt = `{{task}}

The repository {{repo}} is cloned in this directory ({{workdir}}), with PR
#{{number}} already checked out on branch ` + "`{{branch}}`" + ` over base ` + "`{{base}}`" + `.
This is a throwaway clone: read freely, but do not edit files and do not commit.

The review is already done and has been read — it is in {{review_file}}. Publish
it on PR #{{number}} of {{repo}}: a single review, with inline comments on the
right lines, and do not open another if you already have one on the PR.

**Do not redo the review and do not invent new findings**: publish what is in
the file. If a finding does not fit on a diff line, leave it in the review body.

PR: {{title}} — @{{author}}
{{url}}

Write to stdout what was published, in Markdown.`

// defaultArgs são os args do `claude`. O --output-format stream-json é o que
// faz o agente narrar o que está fazendo enquanto trabalha, em vez de ficar
// mudo até o relatório sair — é dele que vem o log ao vivo da interface web.
// Sem --allowedTools o `claude -p` nega toda permissão em silêncio.
func defaultArgs() []string {
	return []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--allowedTools", "Read,Grep,Glob,Bash,Agent",
	}
}

// legacyArgs são os args que o `bazel init` gravava antes do log ao vivo.
func legacyArgs() []string {
	return []string{"-p", "--allowedTools", "Read,Grep,Glob,Bash,Agent"}
}

// defaultPostAgent é quem publica um review já lido. A skill que ele chama vem
// dentro do binário: publicar não pode depender de uma skill instalada na
// máquina de quem escreveu o Bazel. Quem tem a sua própria troca o task aqui,
// ou monta outro agente de publicação na página.
func defaultPostAgent() AgentDef {
	return AgentDef{
		Name:        "bazel-post-report",
		Description: "publishes the review you have just read, with inline comments",
		Task:        "/bazel-post-report {{review_file}}",
		Prompt:      publishPrompt,
		Posts:       true,
	}
}

// legacyPostAgent é o agente de publicação que o Bazel gravava antes da skill
// embarcada — ele chamava a `/post-report` de ~/.claude/skills, que só existia
// em quem a tivesse instalado.
func legacyPostAgent() AgentDef {
	return AgentDef{
		Name:        "post-report",
		Description: "publishes the review you have just read, with inline comments",
		Task:        "/post-report {{review_file}}",
		Prompt:      publishPrompt,
		Posts:       true,
	}
}

// migratePostAgent troca a skill do agente de publicação padrão pela que vem
// no binário. Só nome e task mudam: prompt, comando, checkout e timeout são
// escolhas de quem editou o arquivo e sobrevivem. O que não pode sobreviver é
// a task apontando para uma skill que esta máquina talvez nunca tenha tido —
// era isso que fazia o botão de publicar disparar um agente sem instrução.
//
// Quem apontou o post_agent para outra coisa não é tocado, inclusive quem
// aponta para a própria /post-report de propósito: só casa o padrão antigo,
// nome e task exatos.
func migratePostAgent(def AgentDef) AgentDef {
	legacy := legacyPostAgent()
	if strings.TrimSpace(def.Name) != legacy.Name || strings.TrimSpace(def.Task) != legacy.Task {
		return def
	}
	novo := defaultPostAgent()
	def.Name, def.Task = novo.Name, novo.Task
	if d := strings.TrimSpace(def.Description); d == "" || d == legacy.Description {
		def.Description = novo.Description
	}
	return def
}

// Default devolve a configuração inicial usada pelo "bazel init".
func Default() *Config {
	return &Config{
		Repos:         []string{},
		Authors:       []string{},
		IncludeDrafts: false,
		ReviewsDir:    "",
		MaxDiffBytes:  400_000,
		Agent: Agent{
			Command:        "claude",
			Args:           defaultArgs(),
			Checkout:       true,
			Prompt:         defaultPrompt,
			TimeoutSeconds: 1800,
			Pricing:        defaultPricing(),
		},
		// Agents e Pipelines nascem vazios de propósito: quem monta a lista é
		// você, na página, a partir das skills que estão instaladas na sua
		// máquina. Uma lista de fábrica só acertaria por coincidência — ela
		// apontaria para skills que este computador pode nunca ter tido.
		PostAgent: defaultPostAgent(),
	}
}

// defaultPricing é a tabela de preços shipped com o código. Os preços mudam
// — um modelo novo sai, uma tarifa muda — e o binário não pode precisar de
// rebuild. O usuário sobrescreve em config.yaml.
func defaultPricing() PricingTable {
	return pricing.Default
}

// Dir é o diretório de configuração do Bazel.
func Dir() (string, error) {
	if d := os.Getenv("BAZEL_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".bazel"), nil
}

// Path é o caminho do config.yaml.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// Load lê a configuração do disco, preenchendo os campos ausentes com o padrão.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no configuration found at %s", path)
		}
		return nil, err
	}

	cfg := Default()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("invalid config (%s): %w", path, err)
	}
	if cfg.Agent.Command == "" {
		cfg.Agent.Command = Default().Agent.Command
	}
	if strings.TrimSpace(cfg.Agent.Prompt) == "" {
		cfg.Agent.Prompt = defaultPrompt
	}
	// Mesma ideia para os args: quem está com os de fábrica antigos ganha o
	// --output-format stream-json, que é o que acende o log ao vivo. Args
	// customizados ficam como estão — sem stream-json o log mostra a saída
	// crua do agente, que é o que ele de fato escreve.
	if sameArgs(cfg.Agent.Args, legacyArgs()) {
		cfg.Agent.Args = defaultArgs()
	}
	if strings.TrimSpace(cfg.PostAgent.Name) == "" {
		cfg.PostAgent = defaultPostAgent()
	}
	// Mesma ideia dos args: quem está com o padrão antigo ganha a skill
	// embarcada, que funciona sem nada instalado.
	cfg.PostAgent = migratePostAgent(cfg.PostAgent)
	if cfg.MaxDiffBytes <= 0 {
		cfg.MaxDiffBytes = Default().MaxDiffBytes
	}
	if err := cfg.validateFormats(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadOrInit lê a configuração e, se ela ainda não existir, escreve a padrão
// e devolve essa. O Bazel é uma interface web: não há shell para rodar um
// comando de inicialização antes de abrir a página.
func LoadOrInit() (*Config, bool, error) {
	path, err := Path()
	if err != nil {
		return nil, false, err
	}
	if _, err := os.Stat(path); err == nil {
		cfg, err := Load()
		return cfg, false, err
	} else if !os.IsNotExist(err) {
		return nil, false, err
	}

	cfg := Default()
	if err := cfg.Save(); err != nil {
		return nil, false, fmt.Errorf("criando %s: %w", path, err)
	}
	return cfg, true, nil
}

// Save grava a configuração, criando o diretório se necessário.
func (c *Config) Save() error {
	if err := c.validateFormats(); err != nil {
		return err
	}
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// ResolvedReviewsDir devolve o diretório de reviews absoluto. Se reviews_dir
// estiver vazio, cai em <BAZEL_HOME>/reviews — assim BAZEL_HOME isola tudo.
func (c *Config) ResolvedReviewsDir() (string, error) {
	if strings.TrimSpace(c.ReviewsDir) == "" {
		dir, err := Dir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, "reviews"), nil
	}
	return expandHome(c.ReviewsDir)
}

func expandHome(p string) (string, error) {
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}

// AddRepo adiciona um repositório, ignorando duplicatas.
func (c *Config) AddRepo(repo string) bool {
	repo = strings.TrimSpace(strings.TrimSuffix(repo, "/"))
	repo = strings.TrimPrefix(repo, "https://github.com/")
	for _, r := range c.Repos {
		if strings.EqualFold(r, repo) {
			return false
		}
	}
	c.Repos = append(c.Repos, repo)
	return true
}

// RemoveRepo remove um repositório da lista.
func (c *Config) RemoveRepo(repo string) bool {
	for i, r := range c.Repos {
		if strings.EqualFold(r, repo) {
			c.Repos = append(c.Repos[:i], c.Repos[i+1:]...)
			return true
		}
	}
	return false
}

// --- agents e pipelines ---

// ErrNoAgents é o que sai quando ainda não há agente nenhum na configuração.
// Um config novo começa assim: a lista é montada na página, a partir das
// skills que o Claude Code tem instaladas nesta máquina.
var ErrNoAgents = errors.New("no agents configured — open the configuration and add one out of your skills")

// AddAgentFromSkill acrescenta um agente que roda uma skill do Claude Code
// sobre o PR. É por aqui que a lista vazia de um config novo é preenchida: o
// nome do agente é o da skill, e a task é a invocação dela com o número do PR.
//
// Com posts, o agente publica o review sozinho: a task ganha o --post e o
// molde do prompt passa a ser o que autoriza escrever no GitHub.
func (c *Config) AddAgentFromSkill(skill, description string, posts bool) (AgentDef, error) {
	skill = strings.TrimPrefix(strings.TrimSpace(skill), "/")
	if skill == "" {
		return AgentDef{}, errors.New("skill with no name")
	}
	if strings.ContainsAny(skill, " \t\n/") {
		return AgentDef{}, fmt.Errorf("%q is not a skill name", skill)
	}
	// `pause` e `publish` são passos que o Bazel executa dentro de uma
	// pipeline. Um agente com esse nome tornaria ambíguo o que um passo quer
	// dizer, e a pipeline escolheria errado.
	if ReservedStep(skill) {
		return AgentDef{}, fmt.Errorf("%q is a reserved pipeline step — an agent cannot be called that", skill)
	}
	def := AgentDef{
		Name:        skill,
		Description: strings.TrimSpace(description),
		Task:        "/" + skill + " {{number}}",
	}
	if posts {
		// O sufixo é o que deixa os dois conviverem na lista: a mesma skill
		// pode virar um agente que você lê e outro que publica sozinho.
		def.Name = skill + "-post"
		def.Task += " --post"
		def.Prompt = postPrompt
		def.Posts = true
	}
	for _, a := range c.Agents {
		if strings.EqualFold(strings.TrimSpace(a.Name), def.Name) {
			return AgentDef{}, fmt.Errorf("the agent %q is already in the list", def.Name)
		}
	}
	c.Agents = append(c.Agents, def)
	return def, nil
}

// RemoveAgent tira um agente da lista. Pipeline que apontava para ele continua
// valendo pelo resto dos passos — é o mesmo tratamento que ela já dá a um
// passo desconhecido.
func (c *Config) RemoveAgent(name string) bool {
	name = strings.TrimSpace(name)
	for i, a := range c.Agents {
		if strings.EqualFold(strings.TrimSpace(a.Name), name) {
			c.Agents = append(c.Agents[:i], c.Agents[i+1:]...)
			c.clearDefault(name)
			return true
		}
	}
	return false
}

// SetDefaultAgent marca a escolha que roda quando ninguém escolhe. Vale para
// agente e para pipeline: no seletor os agents vêm antes das pipelines, então
// "ser o primeiro" não é algo que uma pipeline consiga por posição.
func (c *Config) SetDefaultAgent(name string) bool {
	name = strings.TrimSpace(name)
	for _, ch := range c.Choices() {
		if strings.EqualFold(ch.Name, name) {
			c.Default = ch.Name
			return true
		}
	}
	return false
}

// --- pipelines ---

// AddPipeline encadeia agentes sobre o mesmo clone, na ordem dada. É o que a
// página monta quando você quer duas lentes em sequência sem clonar duas
// vezes, ou a história do PR antes do review dele.
//
// Todo passo tem de ser um agente que existe: uma pipeline com passo
// desconhecido roda pelo resto e cala sobre o que faltou, e descobrir isso no
// meio de um review é tarde demais.
func (c *Config) AddPipeline(name, description string, steps []string) (Pipeline, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Pipeline{}, errors.New("a pipeline needs a name")
	}
	for _, ch := range c.Choices() {
		if strings.EqualFold(ch.Name, name) {
			kind := "agent"
			if ch.Pipeline {
				kind = "pipeline"
			}
			return Pipeline{}, fmt.Errorf("there is already a %s called %q", kind, ch.Name)
		}
	}

	limpos, err := c.resolveSteps(steps)
	if err != nil {
		return Pipeline{}, err
	}

	p := Pipeline{Name: name, Description: strings.TrimSpace(description), Steps: limpos}
	c.Pipelines = append(c.Pipelines, p)
	return p, nil
}

// resolveSteps valida a sequência que a página montou e devolve os nomes já
// normalizados. As regras existem porque cada uma delas produz, em runtime, um
// comportamento que parece bug: pausa antes de qualquer agente para sem ter o
// que mostrar, pausa no fim não tem para onde continuar, e duas seguidas pedem
// dois cliques para nada.
func (c *Config) resolveSteps(steps []string) ([]string, error) {
	out := make([]string, 0, len(steps))
	vistos := make(map[string]bool, len(steps))
	agentes, publica := 0, false

	for _, step := range steps {
		step = strings.TrimSpace(step)
		if step == "" {
			continue
		}
		if ReservedStep(step) {
			nome := strings.ToLower(step)
			if agentes == 0 {
				return nil, fmt.Errorf("%q cannot be the first step — there would be nothing to show you yet", nome)
			}
			// Duas pausas seguidas pedem dois cliques para nada. Já
			// `pause` → `publish` é o ponto do recurso: parar, ler, mandar.
			if nome == StepPause && len(out) > 0 && strings.EqualFold(out[len(out)-1], StepPause) {
				return nil, errors.New("two pauses in a row ask for two clicks and run nothing between them")
			}
			if nome == StepPublish {
				if publica {
					return nil, errors.New("a pipeline publishes at most once — a second review on the same PR is noise")
				}
				if !c.temPublicavelAntes(out) {
					return nil, errors.New("nothing before this step produces a review that can go to the PR")
				}
				publica = true
			}
			out = append(out, nome)
			continue
		}

		var achou string
		for _, a := range c.Agents {
			if strings.EqualFold(strings.TrimSpace(a.Name), step) {
				achou = strings.TrimSpace(a.Name)
				break
			}
		}
		if achou == "" {
			return nil, fmt.Errorf("%q is not an agent — a pipeline chains agents from the list, not skills", step)
		}
		// O mesmo agente duas vezes seria o mesmo trabalho duas vezes sobre o
		// mesmo clone, e os dois passos dividiriam o nome no log.
		if vistos[strings.ToLower(achou)] {
			return nil, fmt.Errorf("%q is in this pipeline twice", achou)
		}
		vistos[strings.ToLower(achou)] = true
		agentes++
		out = append(out, achou)
	}

	if agentes == 0 {
		return nil, errors.New("a pipeline needs at least one agent")
	}
	if strings.EqualFold(out[len(out)-1], StepPause) {
		return nil, errors.New("a pause at the end has nothing to continue into — it is the same as stopping there")
	}
	// Publicar é o fim da linha: o relatório que vai ao PR é o que a pipeline
	// produziu, e continuar revisando depois de já ter publicado deixaria em
	// disco um review diferente do que o time leu.
	if publica && !strings.EqualFold(out[len(out)-1], StepPublish) {
		return nil, errors.New("publish has to be the last step — what goes to the PR is what the pipeline produced")
	}
	return out, nil
}

// temPublicavelAntes diz se algum passo já corrido produz relatório que pode ir
// ao PR. Publicar o que não é review é justamente o que o tique de publicação
// existe para impedir.
func (c *Config) temPublicavelAntes(steps []string) bool {
	for _, step := range steps {
		if ReservedStep(step) {
			continue
		}
		for _, a := range c.Agents {
			if !strings.EqualFold(strings.TrimSpace(a.Name), step) {
				continue
			}
			if a.Publishable == nil || *a.Publishable {
				return true
			}
		}
	}
	return false
}

// RemovePipeline tira uma pipeline da lista.
func (c *Config) RemovePipeline(name string) bool {
	name = strings.TrimSpace(name)
	for i, p := range c.Pipelines {
		if !strings.EqualFold(strings.TrimSpace(p.Name), name) {
			continue
		}
		c.Pipelines = append(c.Pipelines[:i], c.Pipelines[i+1:]...)
		c.clearDefault(name)
		return true
	}
	return false
}

// clearDefault esquece o padrão quando ele apontava para o que acabou de sair.
// Sem isso o seletor voltaria para a primeira escolha de qualquer jeito, mas o
// yaml ficaria com um nome que não existe mais.
func (c *Config) clearDefault(name string) {
	if strings.EqualFold(strings.TrimSpace(c.Default), strings.TrimSpace(name)) {
		c.Default = ""
	}
}

// SetAgentPublishable liga ou desliga a ida ao PR da saída de um agente. É o
// tique do painel: history-pr conta a história de um PR, e história não é
// review — publicá-la diria ao time que o código foi revisado quando ninguém
// revisou nada.
//
// Agente que publica sozinho não pode ser marcado como não-publicável: ele
// escreve no PR por conta própria, e desligar o botão aqui não o impediria de
// nada. Nesse caso o jeito é tirá-lo da lista.
func (c *Config) SetAgentPublishable(name string, v bool) error {
	name = strings.TrimSpace(name)
	for i, a := range c.Agents {
		if !strings.EqualFold(strings.TrimSpace(a.Name), name) {
			continue
		}
		if !v && a.Posts {
			return fmt.Errorf("%q publishes to the PR on its own — remove it from the list instead", a.Name)
		}
		c.Agents[i].Publishable = &v
		return nil
	}
	return fmt.Errorf("the agent %q is not in the list", name)
}

// StepPublishes diz se a saída deste passo pode ir ao PR. Um passo que não
// está na escolha não publica nada.
func (c Choice) StepPublishes(name string) bool {
	name = strings.TrimSpace(name)
	for _, s := range c.Steps {
		if strings.EqualFold(strings.TrimSpace(s.Name), name) {
			return s.Reserved == "" && s.Publishable
		}
	}
	return false
}

// Agents são os passos que são agente de verdade — os que rodam um processo.
// É o que separa "quantas lentes vão rodar" de "quantas linhas a pipeline tem".
func (c Choice) Agents() []ResolvedAgent {
	out := make([]ResolvedAgent, 0, len(c.Steps))
	for _, s := range c.Steps {
		if s.Reserved == "" {
			out = append(out, s)
		}
	}
	return out
}

// HasPause diz se a sequência para no meio para você ler o que saiu.
func (c Choice) HasPause() bool {
	for _, s := range c.Steps {
		if s.Reserved == StepPause {
			return true
		}
	}
	return false
}

// sameArgs compara duas listas de argumentos.
func sameArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// StrayCommand devolve o comando que o molde de `agent` dispara por conta
// própria. É o vazio no caso normal.
//
// Um molde sem `{{task}}` ganha a task prefixada na primeira linha, o que é
// conveniente para quem escreveu um molde sem placeholder — e é uma armadilha
// para quem escreveu um molde de antes de existir seletor, com a skill escrita
// à mão nele. Aí todo agente roda dois comandos: o que você escolheu e o que
// está no molde. O agente até parece funcionar, e o segundo comando aparece
// como um gasto que ninguém pediu.
//
// Agente com `prompt` próprio não passa por aqui: só herda quem não escreveu o
// seu, e é o bloco `agent` que vale para esses.
func (c *Config) StrayCommand() string {
	tmpl := c.Agent.Prompt
	if strings.Contains(tmpl, "{{task}}") {
		return ""
	}
	for _, line := range strings.Split(tmpl, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "/") {
			continue
		}
		cmd := line
		if i := strings.IndexAny(cmd, " \t"); i > 0 {
			cmd = cmd[:i]
		}
		// Um caminho absoluto não é comando; uma skill não tem barra no meio.
		if len(cmd) > 1 && !strings.Contains(cmd[1:], "/") {
			return cmd
		}
	}
	return ""
}

// validateFormats rejects only explicit, unsupported output protocols. An
// empty format deliberately remains valid: existing configurations retain
// their argument-based Claude stream detection or raw stdout behavior.
func (c *Config) validateFormats() error {
	if err := validateFormat(c.Agent.Format); err != nil {
		return fmt.Errorf("invalid format for base agent: %w", err)
	}
	for i, def := range c.Agents {
		if err := validateFormat(def.Format); err != nil {
			return fmt.Errorf("invalid format for agents[%d]: %w", i, err)
		}
	}
	if err := validateFormat(c.PostAgent.Format); err != nil {
		return fmt.Errorf("invalid format for post_agent: %w", err)
	}
	return nil
}

func validateFormat(format string) error {
	switch format {
	case "", "claude-stream", "codex-json", "grok-stream", "plain":
		return nil
	default:
		return fmt.Errorf("%q (expected claude-stream, codex-json, grok-stream, or plain)", format)
	}
}

// promptNeedsTask diz se o molde é o de fábrica — uma casca em volta do
// {{task}} de um agente. Sozinho ele não pede nada: quem manda no review é o
// agente que preenche esse buraco.
func promptNeedsTask(p string) bool {
	return strings.TrimSpace(p) == strings.TrimSpace(defaultPrompt)
}

// Choices é o que o seletor mostra antes do review: primeiro os agents
// nomeados, depois as pipelines. A primeira da lista é a padrão.
//
// A lista começa vazia num config novo: é a página que a preenche, a partir
// das skills que o Claude Code tem instaladas nesta máquina. Sem `agents:` e
// com um `agent.prompt` seu, sobra uma escolha só — o bloco `agent` puro, que
// é como o Bazel se comportava antes de existir seletor.
func (c *Config) Choices() []Choice {
	if len(c.Agents) == 0 {
		// Lista vazia é o estado inicial: os agents são adicionados na página,
		// a partir das skills instaladas. Quem escreveu o próprio
		// `agent.prompt` continua tendo o que rodar sem eles — o de fábrica,
		// não: sem {{task}} preenchido ele não pede review nenhum.
		if promptNeedsTask(c.Agent.Prompt) {
			return nil
		}
		return []Choice{c.baseChoice()}
	}

	out := make([]Choice, 0, len(c.Agents)+len(c.Pipelines))
	byName := make(map[string]ResolvedAgent, len(c.Agents))
	for _, def := range c.Agents {
		if strings.TrimSpace(def.Name) == "" {
			continue
		}
		ra := c.resolve(def)
		byName[ra.Name] = ra
		out = append(out, Choice{
			Name:        ra.Name,
			Description: ra.Description,
			Posts:       ra.Posts,
			Publishable: ra.Publishable,
			Steps:       []ResolvedAgent{ra},
		})
	}

	for _, p := range c.Pipelines {
		name := strings.TrimSpace(p.Name)
		if name == "" || byName[name].Name != "" {
			// Nome vazio ou colidindo com um agent: o agent ganha.
			continue
		}
		var steps []ResolvedAgent
		for _, step := range p.Steps {
			step = strings.TrimSpace(step)
			if ReservedStep(step) {
				nome := strings.ToLower(step)
				steps = append(steps, ResolvedAgent{Name: nome, Reserved: nome})
				continue
			}
			// Passo apontando para agent que não existe é ignorado — a
			// pipeline continua valendo pelo resto.
			if ra, ok := byName[step]; ok {
				steps = append(steps, ra)
			}
		}
		steps = semReservadoSolto(steps)
		if len(steps) == 0 {
			continue
		}
		posts, publicavel := false, false
		for _, st := range steps {
			// Um passo `publish` escreve no PR sozinho — a página avisa antes
			// de disparar a escolha, como faz com qualquer agente que publica.
			posts = posts || st.Posts || st.Reserved == StepPublish
			publicavel = publicavel || st.Publishable
		}
		out = append(out, Choice{
			Name:        name,
			Description: p.Description,
			Pipeline:    true,
			Posts:       posts,
			Publishable: publicavel,
			Steps:       steps,
		})
	}
	return frente(out, c.Default)
}

// semReservadoSolto tira os passos reservados que sobraram sem agente nenhum
// para acompanhar. Um agente pode ter sido removido da lista depois que a
// pipeline foi montada, e o que resta pode ser um `pause` sem nada para ler
// antes ou um `publish` sem relatório para publicar — parar ou publicar o nada
// é pior do que simplesmente não fazer.
func semReservadoSolto(steps []ResolvedAgent) []ResolvedAgent {
	out := make([]ResolvedAgent, 0, len(steps))
	agentes := 0
	for _, st := range steps {
		if st.Reserved == "" {
			agentes++
			out = append(out, st)
			continue
		}
		// Reservado só vale com algum agente antes dele, e pausa nunca vem
		// logo depois de outra pausa.
		if agentes == 0 {
			continue
		}
		if st.Reserved == StepPause && len(out) > 0 && out[len(out)-1].Reserved == StepPause {
			continue
		}
		out = append(out, st)
	}
	// Pausa no fim não tem para onde continuar; publish que deixou de ser o
	// último (porque um agente depois dele sumiu) também não vale mais.
	for len(out) > 0 && out[len(out)-1].Reserved == StepPause {
		out = out[:len(out)-1]
	}
	for i := 0; i < len(out)-1; i++ {
		if out[i].Reserved == StepPublish {
			out = out[:i]
			break
		}
	}
	if agentes == 0 {
		return nil
	}
	return out
}

// frente traz a escolha padrão para o começo da lista. A primeira é a que roda
// quando ninguém escolhe, e é assim que a página a marca — então basta
// reordenar aqui para o resto da casa concordar. Nome vazio ou que não existe
// mais deixa a ordem como está.
func frente(list []Choice, padrao string) []Choice {
	padrao = strings.TrimSpace(padrao)
	if padrao == "" {
		return list
	}
	for i, ch := range list {
		if !strings.EqualFold(ch.Name, padrao) {
			continue
		}
		if i == 0 {
			return list
		}
		out := make([]Choice, 0, len(list))
		out = append(out, list[i])
		out = append(out, list[:i]...)
		return append(out, list[i+1:]...)
	}
	return list
}

// DefaultChoice é a que roda quando ninguém escolhe: a primeira da lista. Sem
// nenhum agente configurado ela vem vazia, e quem for rodá-la reclama.
func (c *Config) DefaultChoice() Choice {
	choices := c.Choices()
	if len(choices) == 0 {
		return Choice{}
	}
	return choices[0]
}

// ChoiceByName acha um agent ou pipeline pelo nome, sem diferenciar caixa.
func (c *Config) ChoiceByName(name string) (Choice, error) {
	name = strings.TrimSpace(name)
	choices := c.Choices()
	for _, ch := range choices {
		if strings.EqualFold(ch.Name, name) {
			return ch, nil
		}
	}
	if len(choices) == 0 {
		return Choice{}, ErrNoAgents
	}
	names := make([]string, 0, len(choices))
	for _, ch := range choices {
		names = append(names, ch.Name)
	}
	return Choice{}, fmt.Errorf("no such agent %q — available: %s", name, strings.Join(names, ", "))
}

// PostChoice é o agente de publicação, pronto para rodar.
func (c *Config) PostChoice() Choice {
	def := c.PostAgent
	if strings.TrimSpace(def.Name) == "" {
		def = defaultPostAgent()
	}
	ra := c.resolve(def)
	// Sem dizer nada, publicar clona: comentário inline precisa do diff para
	// achar a linha. Quem escreveu `checkout: false` no post_agent sabe o que
	// está fazendo — um agente que só chama a API do GitHub não precisa disso.
	if def.Checkout == nil {
		ra.Checkout = true
	}
	ra.Posts = true
	ra.Publishable = true
	return Choice{Name: ra.Name, Description: ra.Description, Posts: true, Publishable: true, Steps: []ResolvedAgent{ra}}
}

// baseChoice embrulha o bloco `agent` num Choice de um passo só.
func (c *Config) baseChoice() Choice {
	ra := ResolvedAgent{
		Name:           "default",
		Description:    "the agent configured under `agent`",
		Command:        c.Agent.Command,
		Args:           c.Agent.Args,
		Format:         c.Agent.Format,
		Prompt:         c.Agent.Prompt,
		Publishable:    true,
		Checkout:       c.Agent.Checkout,
		TimeoutSeconds: c.Agent.TimeoutSeconds,
		Env:            c.Agent.Env,
	}
	return Choice{Name: ra.Name, Description: ra.Description, Publishable: true, Steps: []ResolvedAgent{ra}}
}

// resolve preenche com o bloco `agent` tudo que o AgentDef não sobrescreve.
func (c *Config) resolve(def AgentDef) ResolvedAgent {
	ra := ResolvedAgent{
		Name:        strings.TrimSpace(def.Name),
		Description: def.Description,
		Command:     def.Command,
		Args:        def.Args,
		Format:      def.Format,
		Prompt:      def.Prompt,
		Task:        def.Task,
		Posts:       def.Posts,
		// nil é publicável: todo agente escrito antes deste campo existir
		// continua indo ao PR como sempre foi.
		Publishable:    def.Publishable == nil || *def.Publishable,
		Checkout:       c.Agent.Checkout,
		TimeoutSeconds: def.TimeoutSeconds,
		Env:            def.Env,
	}
	if ra.Command == "" {
		ra.Command = c.Agent.Command
		// Args só herdam junto com o comando: args do claude num codex da
		// vida não querem dizer nada.
		if ra.Args == nil {
			ra.Args = c.Agent.Args
		}
	}
	if ra.Format == "" {
		ra.Format = c.Agent.Format
	}
	if strings.TrimSpace(ra.Prompt) == "" {
		ra.Prompt = c.Agent.Prompt
	}
	if def.Checkout != nil {
		ra.Checkout = *def.Checkout
	}
	if ra.TimeoutSeconds <= 0 {
		ra.TimeoutSeconds = c.Agent.TimeoutSeconds
	}
	if len(ra.Env) == 0 {
		ra.Env = c.Agent.Env
	}
	return ra
}

// NeedsCheckout diz se algum passo da escolha precisa do clone do PR.
func (c Choice) NeedsCheckout() bool {
	for _, s := range c.Steps {
		if s.Checkout {
			return true
		}
	}
	return false
}

// StepNames são os nomes dos passos, na ordem de execução.
func (c Choice) StepNames() []string {
	out := make([]string, 0, len(c.Steps))
	for _, s := range c.Steps {
		out = append(out, s.Name)
	}
	return out
}
