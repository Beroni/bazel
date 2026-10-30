// Package config loads and persists the Bazel configuration
// (~/.bazel/config.yaml).
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/beroni/bazel/internal/pricing"
	"github.com/beroni/bazel/internal/skills"
	"gopkg.in/yaml.v3"
)

// Config is the user's configuration file.
type Config struct {
	// Watched repos, in the "owner/repo" format.
	Repos []string `yaml:"repos"`
	// Authors filters PRs by author (GitHub logins). Empty = everyone.
	Authors []string `yaml:"authors"`
	// IncludeDrafts includes draft PRs in the listing.
	IncludeDrafts bool `yaml:"include_drafts"`
	// ReviewsDir is where the markdown reviews are saved.
	// Empty = <BAZEL_HOME>/reviews.
	ReviewsDir string `yaml:"reviews_dir"`
	// MaxDiffBytes truncates huge diffs before sending them to the agent.
	MaxDiffBytes int `yaml:"max_diff_bytes"`
	// SkillsDir is where the agent's skills live. Empty = ~/.claude/skills.
	SkillsDir string `yaml:"skills_dir"`

	// Agent is the base: command, args and prompt template used by every agent
	// that does not override those fields.
	Agent Agent `yaml:"agent"`
	// Agents are the lenses that can run over a PR. The first one is the
	// default.
	Agents []AgentDef `yaml:"agents"`
	// Pipelines chain agents over the same clone, in the given order.
	Pipelines []Pipeline `yaml:"pipelines"`
	// Default is the name of the choice that runs when nobody chooses. Empty =
	// the first in the list. It exists because a pipeline can also be the
	// default, and it has no way to be first: in the selector the agents come
	// before it.
	Default string `yaml:"default,omitempty"`
	// PostAgent is who publishes a review you have already read. It is the
	// other road to the PR: instead of choosing an agent that publishes on its
	// own, you run the fleet, read the result and only then order it published.
	PostAgent AgentDef `yaml:"post_agent"`
	// HistoryAgent writes a PR's story for the History tab — what it does,
	// the decisions taken and what is still open — on demand. Empty is the
	// default, /history-pr; point its task at another skill to change it.
	HistoryAgent AgentDef `yaml:"history_agent,omitempty"`
	// MCPAllowPublish opens the path to the PR for whoever arrives via /mcp.
	// When off, the MCP surface does not offer publishing — neither as a tool
	// nor by choosing an agent that publishes on its own. "You are the PR
	// gate": a review no human has read does not go to GitHub by default. It is
	// a surface gate, not a security boundary: whoever reaches /mcp reaches
	// /api/jobs/{id}/publish with no authentication (issue #6, token).
	MCPAllowPublish bool `yaml:"mcp_allow_publish,omitempty"`

	// Warnings are what Load had to drop while migrating an old config, one
	// line each. They are never written back to the file.
	Warnings []string `yaml:"-"`
}

// AgentDef is a named agent that appears in the TUI selector.
type AgentDef struct {
	Name string `yaml:"name"`
	// Description is the line that explains the agent in the selector.
	Description string `yaml:"description,omitempty"`
	// Task is this lens's specific instruction. It goes into the {{task}} of
	// the agent.prompt template — that is all that changes from one agent to
	// another.
	Task string `yaml:"task,omitempty"`
	// Prompt replaces the whole template when filled in. It accepts the same
	// placeholders as agent.prompt.
	Prompt string `yaml:"prompt,omitempty"`
	// Command and Args override agent's when filled in — useful for running a
	// lens on another model or another executable.
	Command string   `yaml:"command,omitempty"`
	Args    []string `yaml:"args,omitempty"`
	// Format describes the agent's output protocol. Empty keeps the legacy
	// detection by args (Claude stream-json, or raw output).
	Format string `yaml:"format,omitempty"`
	// Posts marks the agent that publishes the review to the PR on its own,
	// instead of handing the text back for Bazel to publish later. The
	// interface warns before running one of these: it writes on someone else's
	// PR.
	Posts bool `yaml:"posts,omitempty"`
	// Publishable says whether this agent's output can go to the PR. nil = it
	// can, which is how every agent has always behaved. Turning it off is what
	// separates an agent that produces a review from one that produces
	// something else — a PR's history, for example. Publishing a process
	// narrative would tell the team the code was reviewed when nobody reviewed
	// anything.
	Publishable *bool `yaml:"publishable,omitempty"`
	// Checkout overrides agent.checkout. nil = inherits.
	Checkout *bool `yaml:"checkout,omitempty"`
	// TimeoutSeconds overrides agent.timeout_seconds. 0 = inherits.
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty"`
	// Env is merged into this agent's process environment. Empty inherits
	// agent.env; that empty too means the Bazel process environment only.
	Env map[string]string `yaml:"env,omitempty"`
}

// Pipeline is a sequence of agents run over the same clone of the PR.
type Pipeline struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	// Steps are agent names, in execution order.
	Steps []string `yaml:"steps"`
}

// StepPublish is a pipeline's reserved step. It is not an agent: Bazel runs it
// on its own, and that is why nobody can have an agent with that name. The
// pipeline ends on it showing what goes to the PR — only what is publishable,
// finding by finding — and waits for you to choose and send, with the same
// publishing agent as the button.
const StepPublish = "publish"

// stepPause is the step that existed before publish waited for you. It only
// survives to get out of the pipelines saved with it.
const stepPause = "pause"

// ReservedStep says whether a step name is run by Bazel instead of being an
// agent from the list.
func ReservedStep(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), StepPublish)
}

// ResolvedAgent is an AgentDef with the fields inherited from agent already
// filled in.
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
	// Reserved is the name of the reserved step when this is not a real agent —
	// `publish`. Empty otherwise.
	Reserved string
}

// Choice is what the user picks before the review: a single agent or a whole
// pipeline. In both cases what runs is the list of Steps.
type Choice struct {
	Name        string
	Description string
	// Pipeline says whether this choice chains more than one agent.
	Pipeline bool
	// Posts says whether any step publishes to the PR on its own.
	Posts bool
	// Publishable says whether there is anything here that can go to the PR: at
	// least one publishable step. In a pipeline, only those steps' body goes up
	// — the rest you read on screen and it stays there.
	Publishable bool
	// Bare says this is the single choice of the `agent` block, with no list
	// of agents: the prompt is entirely the user's, and Bazel cannot know
	// whether it publishes.
	Bare  bool
	Steps []ResolvedAgent
}

// PublishesBy says how this choice can write to the PR without going through
// the screen, or "" when no path is in sight. It goes beyond Posts, which is
// only what was declared: a `publish` step, a task with --post, a prompt that
// authorizes publishing and the `agent` block, whose prompt Bazel did not
// write. Whoever decides to refuse (the /mcp) refuses when in doubt.
func (c Choice) PublishesBy() string {
	// The `agent` block alone runs a prompt Bazel did not write, and it never
	// declares posts. It only passes if it keeps the default template's
	// prohibition.
	if c.Bare && len(c.Steps) > 0 && !promptForbidsPosting(c.Steps[0].Prompt) {
		return fmt.Sprintf("is the bare `agent` block, and its prompt does not say %q", noPublishLine)
	}
	for _, st := range c.Steps {
		switch {
		case st.Posts:
			return fmt.Sprintf("has %q, which publishes to the PR on its own", st.Name)
		case st.Reserved == StepPublish:
			return "has a publish step"
		case taskPosts(st.Task):
			return fmt.Sprintf("has %q, whose task passes --post", st.Name)
		case promptAuthorizesPosting(st.Prompt):
			return fmt.Sprintf("has %q, whose prompt authorizes publishing", st.Name)
		}
	}
	return ""
}

// taskPosts says whether the task passes --post to the skill, the flag with
// which the review skills publish what they found.
func taskPosts(task string) bool {
	for _, f := range strings.Fields(task) {
		if f == "--post" || strings.HasPrefix(f, "--post=") {
			return true
		}
	}
	return false
}

// noPublishLine is the prohibition on writing to GitHub that the default
// template carries.
const noPublishLine = "do not publish anything to GitHub"

// promptForbidsPosting says whether the prompt carries the default template's
// prohibition, ignoring line breaks and case.
func promptForbidsPosting(prompt string) bool {
	return strings.Contains(flatten(prompt), strings.ToLower(noPublishLine))
}

// flatten is text as it reads: lower case, on one line, without the quotes and
// emphasis that would split a phrase or a flag.
func flatten(text string) string {
	bare := strings.NewReplacer("`", "", "*", "", "_", "", "'", "", `"`, "").Replace(text)
	return strings.ToLower(strings.Join(strings.Fields(bare), " "))
}

// promptAuthorizesPosting recognizes the templates that allow writing to the
// PR — the postPrompt, the publishPrompt and any prompt that says the same in
// its own words. "Not authorized to publish" says the opposite.
func promptAuthorizesPosting(prompt string) bool {
	flat := flatten(prompt)
	if flat == flatten(postPrompt) || flat == flatten(publishPrompt) {
		return true
	}
	return strings.Contains(strings.ReplaceAll(flat, "not authorized to publish", ""), "authorized to publish")
}

// Agent describes how to invoke the AI agent that does the review.
type Agent struct {
	// Command is the executable (e.g. "claude").
	Command string `yaml:"command"`
	// Args are the fixed arguments. The prompt goes through stdin.
	Args []string `yaml:"args"`
	// Format identifies the agent's structured output protocol. Empty keeps the
	// legacy detection by args.
	Format string `yaml:"format,omitempty"`
	// Checkout clones the repository into a temporary folder and checks out the
	// PR before running the agent, which runs with that folder as its working
	// directory. Needed for agents that read the code, and not just the diff.
	Checkout bool `yaml:"checkout"`
	// Prompt is the review template. Placeholders: {{repo}}, {{number}},
	// {{title}}, {{author}}, {{url}}, {{branch}}, {{base}}, {{body}},
	// {{workdir}}, {{diff}}. The diff is only downloaded if the template asks.
	Prompt string `yaml:"prompt"`
	// TimeoutSeconds caps how long a review takes. 0 = no limit.
	TimeoutSeconds int `yaml:"timeout_seconds"`
	// Env is merged into every agent process that does not set its own env.
	Env map[string]string `yaml:"env,omitempty"`
	// Pricing holds the prices the user overrides, per model, in USD per 1M
	// tokens. It keeps only what the user wrote: the default lives in the
	// binary and enters through Prices — saving it here would freeze the prices
	// of the saving version into config.yaml, and no price fix would ever
	// arrive again.
	Pricing PricingTable `yaml:"pricing,omitempty"`
}

// Rate is the cost per 1M tokens, in USD, for a usage category.
type Rate = pricing.Rate

// PricingTable is the price table per model, in USD per 1M tokens.
// Key: model name (case-insensitive). Value: rate per category.
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

// postPrompt is the template for the agent that publishes on its own. The
// default template forbids writing to GitHub — this one swaps that line for the
// explicit permission, which is exactly what the post skill needs.
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

// publishPrompt is the template for whoever publishes an already written
// review. The instruction that matters is the last one: do not redo the review,
// publish what is in the file — that is what you read on screen before sending.
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

// defaultArgs are the `claude` args. --output-format stream-json is what makes
// the agent narrate what it is doing while it works, instead of staying silent
// until the report comes out — that is where the web interface's live log comes
// from. Without --allowedTools `claude -p` silently denies every permission.
func defaultArgs() []string {
	return []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--allowedTools", "Read,Grep,Glob,Bash,Agent",
	}
}

// legacyArgs are the args `bazel init` saved before the live log.
func legacyArgs() []string {
	return []string{"-p", "--allowedTools", "Read,Grep,Glob,Bash,Agent"}
}

// defaultPostAgent is who publishes an already read review. The skill it calls
// ships inside the binary: publishing cannot depend on a skill installed on the
// machine of whoever wrote Bazel. Anyone who has their own swaps the task here,
// or builds another publishing agent on the page.
func defaultPostAgent() AgentDef {
	return AgentDef{
		Name:        "bazel-post-report",
		Description: "publishes the review you have just read, with inline comments",
		Task:        "/bazel-post-report {{review_file}}",
		Prompt:      publishPrompt,
		Posts:       true,
	}
}

// legacyPostAgent is the publishing agent Bazel saved before the embedded skill
// — it called the `/post-report` from ~/.claude/skills, which only existed for
// whoever had installed it.
func legacyPostAgent() AgentDef {
	return AgentDef{
		Name:        "post-report",
		Description: "publishes the review you have just read, with inline comments",
		Task:        "/post-report {{review_file}}",
		Prompt:      publishPrompt,
		Posts:       true,
	}
}

// migratePostAgent swaps the default publishing agent's skill for the one that
// ships in the binary. Only name and task change: prompt, command, checkout and
// timeout are choices of whoever edited the file and survive. What cannot
// survive is the task pointing to a skill this machine may never have had —
// that is what made the publish button fire an agent with no instruction.
//
// Whoever pointed post_agent at something else is not touched, including
// whoever points at their own /post-report on purpose: only the old default
// matches, exact name and task.
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

// Default returns the initial configuration used by "bazel init".
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
		// Agents and Pipelines start empty on purpose: the one who builds the
		// list is you, on the page, from the skills installed on your machine.
		// A factory list would only be right by coincidence — it would point to
		// skills this computer may never have had.
		PostAgent: defaultPostAgent(),
	}
}

// Prices is the price table in effect: the binary's, with the config.yaml
// entries on top. A user entry replaces the price of that model and the others
// keep the default.
func (a Agent) Prices() PricingTable {
	return pricing.Default.With(a.Pricing)
}

// legacyPricing is the table that earlier versions wrote whole into
// config.yaml on every Save — with wrong prices. An entry identical to one of
// these lines was not written by the user: it is a leftover of the old
// default, and letting it stand would override the corrected table.
var legacyPricing = PricingTable{
	"claude-opus-5":              {Input: 15, Output: 75, CacheWrite: 1.50, CacheRead: 0.75},
	"claude-haiku-4-5":           {Input: 1, Output: 5, CacheWrite: 1.25, CacheRead: 0.10},
	"claude-sonnet-4":            {Input: 3, Output: 15, CacheWrite: 0.30, CacheRead: 0.15},
	"claude-opus-4":              {Input: 15, Output: 75, CacheWrite: 1.875, CacheRead: 0.9375},
	"claude-sonnet-3-5-20241022": {Input: 3, Output: 15, CacheWrite: 0.30, CacheRead: 0.15},
}

// dropLegacyPricing removes the old default's lines from what came from
// config.yaml, leaving only what the user actually wrote.
func dropLegacyPricing(t PricingTable) PricingTable {
	for k, v := range t {
		if old, ok := legacyPricing[k]; ok && old == v {
			delete(t, k)
		}
	}
	if len(t) == 0 {
		return nil
	}
	return t
}

// Dir is Bazel's configuration directory.
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

// Path is the path of config.yaml.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// Load reads the configuration from disk, filling missing fields with the
// default.
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
	cfg.Agent.Pricing = dropLegacyPricing(cfg.Agent.Pricing)
	if strings.TrimSpace(cfg.Agent.Prompt) == "" {
		cfg.Agent.Prompt = defaultPrompt
	}
	// Same idea for the args: whoever has the old factory ones gets
	// --output-format stream-json, which is what lights up the live log. Custom
	// args stay as they are — without stream-json the log shows the agent's raw
	// output, which is what it actually writes.
	if sameArgs(cfg.Agent.Args, legacyArgs()) {
		cfg.Agent.Args = defaultArgs()
	}
	if strings.TrimSpace(cfg.PostAgent.Name) == "" {
		cfg.PostAgent = defaultPostAgent()
	}
	// Same idea as the args: whoever has the old default gets the embedded
	// skill, which works with nothing installed.
	cfg.PostAgent = migratePostAgent(cfg.PostAgent)
	cfg.migratePipelines()
	for _, w := range cfg.Warnings {
		fmt.Fprintln(warnOut, "bazel: "+w)
	}
	if cfg.MaxDiffBytes <= 0 {
		cfg.MaxDiffBytes = Default().MaxDiffBytes
	}
	if err := cfg.validateFormats(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// warnOut is where Load reports what it dropped while migrating. The server
// runs in a terminal, so stderr is where you see it.
var warnOut io.Writer = os.Stderr

// LoadOrInit reads the configuration and, if it does not exist yet, writes the
// default and returns that. Bazel is a web interface: there is no shell to run
// an init command before the page opens.
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

// Save writes the configuration, creating the directory if needed.
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

// ResolvedReviewsDir returns the absolute reviews directory. If reviews_dir is
// empty, it falls back to <BAZEL_HOME>/reviews — that way BAZEL_HOME isolates
// everything.
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

// AddRepo adds a repository, ignoring duplicates.
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

var (
	// An owner is letters, digits and single hyphens, never at either end
	// (39 characters at most); a repository name is letters, digits, ".", "_"
	// and "-", up to 100, not starting with "-" and not only dots.
	repoOwnerRe = regexp.MustCompile(`^[A-Za-z0-9]+(?:-[A-Za-z0-9]+)*$`)
	repoNameRe  = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]{0,99}$`)
)

// NormalizeRepo turns what was typed into the owner/name the config keeps, or
// says why it can't. Besides owner/name it takes what is usually pasted for a
// repository — https://github.com/owner/name (any case in the scheme and host,
// with or without www., .git or a trailing slash) and git@github.com:owner/name
// — and trims it down. A PR or any deeper URL is refused, since it names one
// pull request and not the repository to watch, and so is another host or a
// name GitHub would not accept.
func NormalizeRepo(in string) (string, error) {
	repo := strings.TrimSpace(in)
	cut := func(prefix string) bool {
		if strings.HasPrefix(strings.ToLower(repo), prefix) {
			repo = repo[len(prefix):]
			return true
		}
		return false
	}
	if !cut("git@github.com:") {
		url := cut("https://") || cut("http://")
		url = cut("www.") || url
		if !cut("github.com/") && url {
			return "", fmt.Errorf("%q is not a GitHub repository (use owner/name)", in)
		}
	}
	repo = strings.TrimSuffix(strings.TrimSuffix(repo, "/"), ".git")
	parts := strings.Split(repo, "/")
	if len(parts) > 2 && (parts[2] == "pull" || parts[2] == "pulls") {
		return "", fmt.Errorf("%q is a pull request — add its repository instead: %s/%s", in, parts[0], parts[1])
	}
	if len(parts) != 2 || len(parts[0]) > 39 || !repoOwnerRe.MatchString(parts[0]) ||
		!repoNameRe.MatchString(parts[1]) || strings.Trim(parts[1], ".") == "" {
		return "", fmt.Errorf("invalid repository: %q (use owner/name)", in)
	}
	return parts[0] + "/" + parts[1], nil
}

// RemoveRepo removes a repository from the list.
func (c *Config) RemoveRepo(repo string) bool {
	for i, r := range c.Repos {
		if strings.EqualFold(r, repo) {
			c.Repos = append(c.Repos[:i], c.Repos[i+1:]...)
			return true
		}
	}
	return false
}

// --- agents and pipelines ---

// ErrNoAgents is what comes out when there is no agent at all in the
// configuration yet. A new config starts like this: the list is built on the
// page, from the skills Claude Code has installed on this machine.
var ErrNoAgents = errors.New("no agents configured — open the configuration and add one out of your skills")

// AddAgentFromSkill adds an agent that runs a Claude Code skill over the PR.
// This is how the empty list of a new config gets filled: the agent's name is
// the skill's, and the task is its invocation with the PR number.
//
// With posts, the agent publishes the review on its own: the task gets --post
// and the prompt template becomes the one that authorizes writing to GitHub.
func (c *Config) AddAgentFromSkill(skill, description string, posts bool) (AgentDef, error) {
	skill = strings.TrimPrefix(strings.TrimSpace(skill), "/")
	if skill == "" {
		return AgentDef{}, errors.New("skill with no name")
	}
	if strings.ContainsAny(skill, " \t\n/") {
		return AgentDef{}, fmt.Errorf("%q is not a skill name", skill)
	}
	// `publish` is a step Bazel runs inside a pipeline. An agent with that name
	// would make what the step means ambiguous, and the pipeline would pick
	// wrong.
	if ReservedStep(skill) {
		return AgentDef{}, fmt.Errorf("%q is a reserved pipeline step — an agent cannot be called that", skill)
	}
	if post := c.PostSkill(); post != "" && strings.EqualFold(skill, post) {
		return AgentDef{}, fmt.Errorf("%q is the publishing skill — in a pipeline it is the publish step, not an agent", skill)
	}
	def := AgentDef{
		Name:        skill,
		Description: strings.TrimSpace(description),
		Task:        "/" + skill + " {{number}}",
	}
	if posts {
		// The suffix is what lets the two live together in the list: the same
		// skill can become an agent you read and another that publishes on its
		// own.
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

// RemoveAgent takes an agent out of the list. A pipeline that pointed to it
// still holds for the rest of its steps — it is the same treatment it already
// gives an unknown step.
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

// SetDefaultAgent marks the choice that runs when nobody chooses. It works for
// agent and for pipeline: in the selector the agents come before the pipelines,
// so "being first" is not something a pipeline can get by position.
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

// AddPipeline chains agents over the same clone, in the given order. It is what
// the page builds when you want two lenses in sequence without cloning twice,
// or the PR's history before its review.
//
// Every step has to be an agent that exists: a pipeline with an unknown step
// runs through the rest and says nothing about what was missing, and finding
// that out in the middle of a review is too late.
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

// resolveSteps validates the sequence the page built and returns the names
// already normalized. The rules exist because each of them produces, at
// runtime, a behavior that looks like a bug: publishing before any agent, or
// twice, or in the middle of the sequence.
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
			if publica {
				return nil, errors.New("a pipeline publishes at most once — a second review on the same PR is noise")
			}
			if !c.temPublicavelAntes(out) {
				return nil, errors.New("nothing before this step produces a review that can go to the PR")
			}
			// An agent that posts on its own has already written to the PR by
			// the time publish stops to let you choose what goes.
			if quem, ok := c.postaAntes(out); ok {
				return nil, fmt.Errorf("%q posts to the PR on its own, so it cannot come before %q — that step waits for you to choose what goes, and by then %q has already posted", quem, nome, quem)
			}
			publica = true
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
		// The same agent twice would be the same work twice over the same
		// clone, and the two steps would share the name in the log.
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
	// Publishing is the end of the line: the report that goes to the PR is what
	// the pipeline produced, and to keep reviewing after having published would
	// leave on disk a review different from the one the team read.
	if publica && !strings.EqualFold(out[len(out)-1], StepPublish) {
		return nil, errors.New("publish has to be the last step — what goes to the PR is what the pipeline produced")
	}
	return out, nil
}

// temPublicavelAntes says whether any step already run produces a report that
// can go to the PR. Publishing what is not a review is exactly what the publish
// tick exists to prevent.
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

// postaAntes returns the first agent among steps that posts to the PR on its
// own.
func (c *Config) postaAntes(steps []string) (string, bool) {
	for _, step := range steps {
		if ReservedStep(step) {
			continue
		}
		if a, ok := c.agentNamed(step); ok && a.Posts {
			return strings.TrimSpace(a.Name), true
		}
	}
	return "", false
}

// RemovePipeline removes a pipeline from the list.
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

// clearDefault forgets the default when it pointed to what just left. Without
// it the selector would fall back to the first choice anyway, but the yaml
// would keep a name that no longer exists.
func (c *Config) clearDefault(name string) {
	if strings.EqualFold(strings.TrimSpace(c.Default), strings.TrimSpace(name)) {
		c.Default = ""
	}
}

// SetAgentPublishable turns on or off whether an agent's output goes to the PR.
// It is the panel's tick: history-pr tells a PR's story, and a story is not a
// review — publishing it would tell the team the code was reviewed when nobody
// reviewed anything.
//
// An agent that publishes on its own cannot be marked as not publishable: it
// writes on the PR by itself, and turning the button off here would not stop it
// from anything. In that case the way out is to take it off the list.
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

// StepPublishes says whether this step's output can go to the PR. A step that
// is not in the choice publishes nothing.
func (c Choice) StepPublishes(name string) bool {
	name = strings.TrimSpace(name)
	for _, s := range c.Steps {
		if strings.EqualFold(strings.TrimSpace(s.Name), name) {
			return s.Reserved == "" && s.Publishable
		}
	}
	return false
}

// Agents are the steps that are real agents — the ones that run a process. It
// is what separates "how many lenses will run" from "how many lines the
// pipeline has".
func (c Choice) Agents() []ResolvedAgent {
	out := make([]ResolvedAgent, 0, len(c.Steps))
	for _, s := range c.Steps {
		if s.Reserved == "" {
			out = append(out, s)
		}
	}
	return out
}

// sameArgs compares two argument lists.
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

// StrayCommand returns the command that the `agent` template fires on its own.
// It is empty in the normal case.
//
// A template without `{{task}}` gets the task prefixed on the first line, which
// is convenient for whoever wrote a template without a placeholder — and it is
// a trap for whoever wrote a template from before the selector existed, with
// the skill handwritten in it. Then every agent runs two commands: the one you
// chose and the one in the template. The agent even seems to work, and the
// second command shows up as an expense nobody asked for.
//
// An agent with its own `prompt` does not go through here: only those who did
// not write theirs inherit, and for those it is the `agent` block that counts.
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
		// An absolute path is not a command; a skill has no slash in the
		// middle.
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
	if err := validateFormat(c.HistoryAgent.Format); err != nil {
		return fmt.Errorf("invalid format for history_agent: %w", err)
	}
	// Only what is declared stops the boot. The patterns in ValidateHistoryAgent
	// can be wrong, and a false positive must cost one History, not the server.
	if c.HistoryAgent.Posts {
		return errors.New("history_agent has posts: true — the History tab never publishes; drop it")
	}
	return nil
}

// ValidateHistoryAgent refuses a history_agent that could write to the PR.
// It looks only at what the user wrote there: with no history_agent the task
// and the prompt are Bazel's own, and the `agent` block is not inherited. It
// runs when a history is asked for, and a refusal stops that history only.
//
// A list of words can show that an agent publishes, never that it does not, so
// a prompt of its own has to carry the default template's prohibition, the
// same rule the bare `agent` block lives by.
func (c *Config) ValidateHistoryAgent() error {
	def := c.HistoryAgent
	refuse := func(why string) error {
		return fmt.Errorf("history_agent %s — the History tab never publishes", why)
	}
	named := def
	if strings.TrimSpace(named.Name) == "" {
		named.Name = "history"
	}
	switch {
	case def.Posts:
		return refuse("has posts: true")
	case c.isPostAgent(named):
		return refuse("is the post agent")
	case c.mentionsPublishing(def.Task):
		return refuse(fmt.Sprintf("has a task that publishes: %q", def.Task))
	case c.mentionsPublishing(def.Command + " " + strings.Join(def.Args, " ")):
		return refuse("has a command or args that publish")
	case strings.TrimSpace(def.Prompt) == "":
		return nil
	case promptAuthorizesPosting(def.Prompt):
		return refuse("has a prompt that authorizes publishing")
	case c.mentionsPublishing(def.Prompt):
		return refuse("has a prompt that tells it to publish")
	case !promptForbidsPosting(def.Prompt):
		return refuse(fmt.Sprintf("has a prompt that does not say %q", noPublishLine))
	}
	return nil
}

// HistoryCustom says the user wrote a history_agent: then what it does is
// theirs to answer for, and ValidateHistoryAgent can only catch the obvious.
func (c *Config) HistoryCustom() bool {
	d := c.HistoryAgent
	return strings.TrimSpace(d.Task+d.Prompt+d.Command) != "" || len(d.Args) > 0
}

// publishCommandRe matches, in flattened text, the flag and the gh commands
// that write to a PR. --post=false is the flag turned off.
var publishCommandRe = regexp.MustCompile(`(^|\s)--post(=true)?(\s|$)|\bgh (pr (comment|review|merge|edit|close)|issue comment)\b`)

// mentionsPublishing says whether the text calls the built-in post skill, which
// only publishes, passes --post or runs a gh command that writes to the PR.
func (c *Config) mentionsPublishing(text string) bool {
	flat := flatten(text)
	if publishCommandRe.MatchString(flat) {
		return true
	}
	return strings.Contains(flat, "/"+flatten(skills.TaskSkill(defaultPostAgent().Task)))
}

func validateFormat(format string) error {
	switch format {
	case "", "claude-stream", "codex-json", "grok-stream", "plain":
		return nil
	default:
		return fmt.Errorf("%q (expected claude-stream, codex-json, grok-stream, or plain)", format)
	}
}

// promptNeedsTask says whether the template is the factory one — a shell around
// an agent's {{task}}. On its own it asks for nothing: what drives the review
// is the agent that fills that hole.
func promptNeedsTask(p string) bool {
	return strings.TrimSpace(p) == strings.TrimSpace(defaultPrompt)
}

// Choices is what the selector shows before the review: first the named agents,
// then the pipelines. The first in the list is the default.
//
// The list starts empty in a new config: it is the page that fills it, from the
// skills Claude Code has installed on this machine. Without `agents:` and with
// an `agent.prompt` of your own, a single choice is left — the plain `agent`
// block, which is how Bazel behaved before the selector existed.
func (c *Config) Choices() []Choice {
	if len(c.Agents) == 0 {
		// An empty list is the initial state: the agents are added on the page,
		// from the installed skills. Whoever wrote their own `agent.prompt`
		// still has something to run without them — the factory one does not:
		// without {{task}} filled in it asks for no review at all.
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
			// Empty name or colliding with an agent: the agent wins.
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
			// A step pointing to an agent that does not exist is ignored — the
			// pipeline still holds for the rest.
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
			// The `publish` step does not count: it waits for you to send. Only
			// an agent that publishes on its own writes on the PR without
			// asking.
			posts = posts || st.Posts
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

// semReservadoSolto removes the reserved steps left over with no agent at all
// to go with them. An agent may have been removed from the list after the
// pipeline was built, and what is left may be a `publish` with no report to
// publish — publishing nothing is worse than simply not doing it.
func semReservadoSolto(steps []ResolvedAgent) []ResolvedAgent {
	out := make([]ResolvedAgent, 0, len(steps))
	agentes := 0
	for _, st := range steps {
		if st.Reserved == "" {
			agentes++
			out = append(out, st)
			continue
		}
		// A reserved step only counts with some agent before it.
		if agentes == 0 {
			continue
		}
		out = append(out, st)
	}
	// A publish that stopped being the last (because an agent after it
	// vanished) no longer counts either.
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

// frente brings the default choice to the start of the list. The first is the
// one that runs when nobody chooses, and that is how the page marks it — so
// reordering here is enough for the rest of the house to agree. An empty name
// or one that no longer exists leaves the order as it is.
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

// DefaultChoice is the one that runs when nobody chooses: the first in the
// list. With no agent configured it comes back empty, and whoever goes to run
// it complains.
func (c *Config) DefaultChoice() Choice {
	choices := c.Choices()
	if len(choices) == 0 {
		return Choice{}
	}
	return choices[0]
}

// ChoiceByName finds an agent or pipeline by name, case-insensitively.
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

// PostSkill is the skill the publishing agent calls. It does not become an
// agent in the list: alone, in an ordinary step, it runs without the review or
// the publish prompt and posts nothing. In a pipeline the one that publishes is
// the `publish` step.
func (c *Config) PostSkill() string {
	post := c.PostAgent
	if strings.TrimSpace(post.Name) == "" {
		post = defaultPostAgent()
	}
	return skills.TaskSkill(post.Task)
}

// isPostAgent reports whether an agent in the list is the post agent in
// another hat.
//
// It matches by the post_agent's exact name, or by its whole task once
// placeholders and spacing are set aside. Sharing the skill is not enough: a
// post_agent built on a reviewing skill's --post mode
// (`/review-fleet {{review_file}} --post`) would otherwise take the
// `review-fleet` reviewer out of the list. The one skill that is enough is the
// built-in post skill (bazel-post-report): it only publishes, so an agent that
// calls it is a post agent whatever post_agent says, `posts: true` or not.
func (c *Config) isPostAgent(a AgentDef) bool {
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return false
	}
	post := c.PostAgent
	if strings.TrimSpace(post.Name) == "" {
		post = defaultPostAgent()
	}
	if strings.EqualFold(strings.TrimSpace(post.Name), name) {
		return true
	}
	if skill := skills.TaskSkill(a.Task); skill != "" && strings.EqualFold(skill, skills.TaskSkill(defaultPostAgent().Task)) {
		return true
	}
	shape := taskShape(post.Task)
	return shape != "" && strings.EqualFold(taskShape(a.Task), shape)
}

// placeholderRe matches one {{placeholder}} in a task.
var placeholderRe = regexp.MustCompile(`\{\{[^{}]*\}\}`)

// taskShape is a task with every placeholder blanked and spacing collapsed:
// `/x {{review_file}}` and `/x  {{number}}` have the same shape.
func taskShape(task string) string {
	return strings.Join(strings.Fields(placeholderRe.ReplaceAllString(task, "{{}}")), " ")
}

// migratePipelines fixes what the page used to let be saved:
//
//   - the post agent listed as a regular agent leaves the list, and its steps
//     in the pipelines become a single `publish` at the end, which is what they
//     meant;
//   - the `pause` leaves the pipelines. What stops for you to read now is
//     `publish` itself, and `pause → publish` becomes just `publish`.
//
// A pipeline that cannot be carried over without losing the human gate, or
// that is left with no agent, is dropped with a line in Warnings, and
// `default:` is cleared if it named it. Pipelines with nothing to migrate are
// left exactly as written.
func (c *Config) migratePipelines() {
	removidos := map[string]bool{}
	agents := c.Agents[:0]
	for _, a := range c.Agents {
		if c.isPostAgent(a) {
			removidos[strings.ToLower(strings.TrimSpace(a.Name))] = true
			c.clearDefault(a.Name)
			continue
		}
		agents = append(agents, a)
	}
	c.Agents = agents

	pipes := c.Pipelines[:0]
	for _, p := range c.Pipelines {
		steps, motivo := c.migrateSteps(p.Name, p.Steps, removidos)
		if motivo != "" {
			c.Warnings = append(c.Warnings, fmt.Sprintf("pipeline %q was removed while upgrading the config: %s", p.Name, motivo))
			c.clearDefault(p.Name)
			continue
		}
		p.Steps = steps
		pipes = append(pipes, p)
	}
	c.Pipelines = pipes
}

// migrateSteps rewrites one pipeline's steps. A non-empty reason means the
// pipeline has to go.
//
// Every post agent step and every `publish` comes out of where it was, and a
// single `publish` goes at the end when something before it produces a review
// (the rules resolveSteps enforces), unless an agent that posts on its own is
// left before it. A `pause` used to stop before every later step, so an agent
// that posts on its own after one becomes that `publish` too: keeping it would
// post what nobody read.
//
// When that final publish is left out because an agent already posts before it,
// a line goes to Warnings: the step the user wrote (or the post agent after a
// pause) is gone, and nothing else says so. So does an agent that posted after
// a pause and became the publish step: it no longer runs.
func (c *Config) migrateSteps(pipeline string, steps []string, removidos map[string]bool) ([]string, string) {
	precisa := false
	for _, step := range steps {
		nome := strings.ToLower(strings.TrimSpace(step))
		if nome == stepPause || removidos[nome] || ReservedStep(nome) {
			precisa = true
			break
		}
	}
	if !precisa {
		return steps, ""
	}

	out := make([]string, 0, len(steps))
	publica, pausou := false, false
	for _, step := range steps {
		step = strings.TrimSpace(step)
		nome := strings.ToLower(step)
		switch {
		case nome == "":
			continue
		case nome == stepPause:
			pausou = true
			continue
		case removidos[nome] || ReservedStep(nome):
			publica = true
			continue
		}
		if a, ok := c.agentNamed(step); ok && a.Posts && pausou {
			if !c.temPublicavelAntes(out) {
				return nil, fmt.Sprintf("%q posted to the PR after a pause, and nothing before it produces a review the publish step could show you", a.Name)
			}
			// The agent the user put there stops running, and the pipeline
			// looks like any other with a publish step: say so.
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"pipeline %q: %q (after a pause) became the publish step and no longer runs",
				pipeline, strings.TrimSpace(a.Name)))
			publica = true
			continue
		}
		out = append(out, step)
	}

	agentes := 0
	for _, step := range out {
		if _, ok := c.agentNamed(step); ok {
			agentes++
		}
	}
	if agentes == 0 {
		return nil, "no agent is left once the publishing and pause steps are taken out"
	}
	// An agent that posts on its own and stays before the final publish was
	// already posting with no gate before the upgrade; a publish after it would
	// be a second post, and resolveSteps refuses that shape. (One that came
	// after a pause has already turned into the publish above.)
	posta, postaSozinho := c.postaAntes(out)
	if publica && postaSozinho {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"pipeline %q lost its publish step while upgrading the config: %q already posts to the PR on its own, and a publish after it would be a second post",
			pipeline, posta))
	} else if publica && c.temPublicavelAntes(out) {
		out = append(out, StepPublish)
	}
	return out, ""
}

// agentNamed finds an agent in the list by name, ignoring case.
func (c *Config) agentNamed(name string) (AgentDef, bool) {
	name = strings.TrimSpace(name)
	for _, a := range c.Agents {
		if strings.EqualFold(strings.TrimSpace(a.Name), name) {
			return a, true
		}
	}
	return AgentDef{}, false
}

// defaultHistoryTask is the skill the History tab runs unless history_agent
// names another one.
const defaultHistoryTask = "/history-pr {{number}}"

// HistoryChoice is the agent that writes a PR's history, ready to run. It
// reads the clone (git log, the diff), and what it writes never goes to the
// PR: it is the History tab's, not a review.
func (c *Config) HistoryChoice() Choice {
	def := c.HistoryAgent
	if strings.TrimSpace(def.Name) == "" {
		def.Name = "history"
	}
	if strings.TrimSpace(def.Task) == "" {
		def.Task = defaultHistoryTask
	}
	if def.Description == "" {
		def.Description = "the PR's story: what it does, the decisions taken, what is still open"
	}
	// Not the `agent` block's: that one may be a publishing template, and this
	// agent must read and report, whatever the reviews do.
	if strings.TrimSpace(def.Prompt) == "" {
		def.Prompt = defaultPrompt
	}
	ra := c.resolve(def)
	if def.Checkout == nil {
		ra.Checkout = true
	}
	// Not a review: nothing of it is offered for publishing. Whether the agent
	// could post on its own is not masked here: ValidateHistoryAgent refuses it.
	ra.Publishable = false
	return Choice{Name: ra.Name, Description: ra.Description, Posts: ra.Posts, Steps: []ResolvedAgent{ra}}
}

// PostChoice is the publishing agent, ready to run.
func (c *Config) PostChoice() Choice {
	def := c.PostAgent
	if strings.TrimSpace(def.Name) == "" {
		def = defaultPostAgent()
	}
	ra := c.resolve(def)
	// Unless told otherwise, publishing clones: an inline comment needs the
	// diff to find the line. Whoever wrote `checkout: false` in post_agent
	// knows what they are doing — an agent that only calls the GitHub API does
	// not need it.
	if def.Checkout == nil {
		ra.Checkout = true
	}
	ra.Posts = true
	ra.Publishable = true
	return Choice{Name: ra.Name, Description: ra.Description, Posts: true, Publishable: true, Steps: []ResolvedAgent{ra}}
}

// baseChoice wraps the `agent` block in a single-step Choice.
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
	return Choice{Name: ra.Name, Description: ra.Description, Publishable: true, Bare: true, Steps: []ResolvedAgent{ra}}
}

// resolve fills in from the `agent` block everything the AgentDef does not
// override.
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
		// nil is publishable: every agent written before this field existed
		// keeps going to the PR as it always has.
		Publishable:    def.Publishable == nil || *def.Publishable,
		Checkout:       c.Agent.Checkout,
		TimeoutSeconds: def.TimeoutSeconds,
		Env:            def.Env,
	}
	if ra.Command == "" {
		ra.Command = c.Agent.Command
		// Args only inherit along with the command: claude's args on some codex
		// or other mean nothing.
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

// NeedsCheckout says whether any step of the choice needs the PR clone.
func (c Choice) NeedsCheckout() bool {
	for _, s := range c.Steps {
		if s.Checkout {
			return true
		}
	}
	return false
}

// StepNames are the step names, in execution order.
func (c Choice) StepNames() []string {
	out := make([]string, 0, len(c.Steps))
	for _, s := range c.Steps {
		out = append(out, s.Name)
	}
	return out
}
