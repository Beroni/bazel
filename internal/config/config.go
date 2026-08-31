// Package config carrega e persiste a configuração do Bazel (~/.bazel/config.yaml).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	// Posts marca o agente que publica o review no PR sozinho, em vez de
	// devolver o texto para o Bazel publicar depois. A interface avisa antes
	// de rodar um desses: é escrita em PR de outra pessoa.
	Posts bool `yaml:"posts,omitempty"`
	// Checkout sobrescreve agent.checkout. nil = herda.
	Checkout *bool `yaml:"checkout,omitempty"`
	// TimeoutSeconds sobrescreve agent.timeout_seconds. 0 = herda.
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty"`
}

// Pipeline é uma sequência de agentes rodada sobre o mesmo clone do PR.
type Pipeline struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	// Steps são nomes de agents, na ordem de execução.
	Steps []string `yaml:"steps"`
}

// ResolvedAgent é um AgentDef com os campos herdados de agent já preenchidos.
type ResolvedAgent struct {
	Name           string
	Description    string
	Command        string
	Args           []string
	Prompt         string
	Task           string
	Posts          bool
	Checkout       bool
	TimeoutSeconds int
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
	Steps []ResolvedAgent
}

// Agent descreve como invocar o agente de IA que faz o review.
type Agent struct {
	// Command é o executável (ex.: "claude").
	Command string `yaml:"command"`
	// Args são os argumentos fixos. O prompt vai pelo stdin.
	Args []string `yaml:"args"`
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
}

const defaultPrompt = `{{task}}

O repositório {{repo}} está clonado nesta pasta ({{workdir}}), com o PR
#{{number}} já em checkout na branch ` + "`{{branch}}`" + ` sobre a base ` + "`{{base}}`" + `.
É um clone temporário e descartável: leia à vontade, mas não edite arquivos,
não commite e não publique nada no GitHub.

PR: {{title}} — @{{author}}
{{url}}

Descrição do PR:
{{body}}

Devolva no stdout só o relatório final, em markdown.`

// postPrompt é o molde do agente que publica sozinho. O molde padrão proíbe
// escrever no GitHub — este troca essa linha pela permissão explícita, que é
// justamente o que a skill de post precisa.
const postPrompt = `{{task}}

O repositório {{repo}} está clonado nesta pasta ({{workdir}}), com o PR
#{{number}} já em checkout na branch ` + "`{{branch}}`" + ` sobre a base ` + "`{{base}}`" + `.
É um clone temporário e descartável: leia à vontade, mas não edite arquivos e
não commite nada.

Você **tem** autorização para publicar o resultado no PR #{{number}} de
{{repo}} — é para isso que este review foi disparado. Publique um review só,
com os comentários inline nas linhas certas, e não abra outro se já houver um
seu no PR.

PR: {{title}} — @{{author}}
{{url}}

Descrição do PR:
{{body}}

Devolva no stdout o relatório final em markdown, dizendo no fim o que foi
publicado no PR.`

// publishPrompt é o molde de quem publica um review já escrito. A instrução
// que importa é a última: não refazer o review, publicar o que está no arquivo
// — é ele que você leu na tela antes de mandar.
const publishPrompt = `{{task}}

O repositório {{repo}} está clonado nesta pasta ({{workdir}}), com o PR
#{{number}} já em checkout na branch ` + "`{{branch}}`" + ` sobre a base ` + "`{{base}}`" + `.
É um clone temporário e descartável: leia à vontade, mas não edite arquivos e
não commite nada.

O review já está pronto e já foi lido — está em {{review_file}}. Publique-o no
PR #{{number}} de {{repo}}: um review só, com os comentários inline nas linhas
certas, e não abra outro se já houver um seu no PR.

**Não refaça o review e não invente achado novo**: publique o que está no
arquivo. Se algum achado não couber numa linha do diff, deixe-o no corpo do
review.

PR: {{title}} — @{{author}}
{{url}}

Devolva no stdout o que foi publicado, em markdown.`

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

// defaultPostAgent é quem publica um review já lido.
func defaultPostAgent() AgentDef {
	return AgentDef{
		Name:        "post-report",
		Description: "publishes the review you have just read, with inline comments",
		Task:        "/post-report {{review_file}}",
		Prompt:      publishPrompt,
		Posts:       true,
	}
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
		},
		// Agents e Pipelines nascem vazios de propósito: quem monta a lista é
		// você, na página, a partir das skills que estão instaladas na sua
		// máquina. Uma lista de fábrica só acertaria por coincidência — ela
		// apontaria para skills que este computador pode nunca ter tido.
		PostAgent: defaultPostAgent(),
	}
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
	if cfg.MaxDiffBytes <= 0 {
		cfg.MaxDiffBytes = Default().MaxDiffBytes
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
			return true
		}
	}
	return false
}

// SetDefaultAgent move um agente para o começo da lista, que é o mesmo que
// torná-lo o padrão: é a primeira escolha que roda quando ninguém escolhe.
func (c *Config) SetDefaultAgent(name string) bool {
	name = strings.TrimSpace(name)
	for i, a := range c.Agents {
		if !strings.EqualFold(strings.TrimSpace(a.Name), name) {
			continue
		}
		def := c.Agents[i]
		c.Agents = append(c.Agents[:i], c.Agents[i+1:]...)
		c.Agents = append([]AgentDef{def}, c.Agents...)
		return true
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
			// Passo apontando para agent que não existe é ignorado — a
			// pipeline continua valendo pelo resto.
			if ra, ok := byName[strings.TrimSpace(step)]; ok {
				steps = append(steps, ra)
			}
		}
		if len(steps) == 0 {
			continue
		}
		posts := false
		for _, st := range steps {
			posts = posts || st.Posts
		}
		out = append(out, Choice{
			Name:        name,
			Description: p.Description,
			Pipeline:    true,
			Posts:       posts,
			Steps:       steps,
		})
	}
	return out
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
	return Choice{Name: ra.Name, Description: ra.Description, Posts: true, Steps: []ResolvedAgent{ra}}
}

// baseChoice embrulha o bloco `agent` num Choice de um passo só.
func (c *Config) baseChoice() Choice {
	ra := ResolvedAgent{
		Name:           "default",
		Description:    "the agent configured under `agent`",
		Command:        c.Agent.Command,
		Args:           c.Agent.Args,
		Prompt:         c.Agent.Prompt,
		Checkout:       c.Agent.Checkout,
		TimeoutSeconds: c.Agent.TimeoutSeconds,
	}
	return Choice{Name: ra.Name, Description: ra.Description, Steps: []ResolvedAgent{ra}}
}

// resolve preenche com o bloco `agent` tudo que o AgentDef não sobrescreve.
func (c *Config) resolve(def AgentDef) ResolvedAgent {
	ra := ResolvedAgent{
		Name:           strings.TrimSpace(def.Name),
		Description:    def.Description,
		Command:        def.Command,
		Args:           def.Args,
		Prompt:         def.Prompt,
		Task:           def.Task,
		Posts:          def.Posts,
		Checkout:       c.Agent.Checkout,
		TimeoutSeconds: def.TimeoutSeconds,
	}
	if ra.Command == "" {
		ra.Command = c.Agent.Command
		// Args só herdam junto com o comando: args do claude num codex da
		// vida não querem dizer nada.
		if ra.Args == nil {
			ra.Args = c.Agent.Args
		}
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
