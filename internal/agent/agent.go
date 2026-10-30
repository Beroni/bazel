// Package agent runs the configured AI agents over a pull request.
package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// EventKind is the kind of notice the Runner emits during a review.
type EventKind string

const (
	// EventClone is fired before cloning the PR's repository.
	EventClone EventKind = "clone"
	// EventStep marks the start of a step.
	EventStep EventKind = "step"
	// EventStepDone marks the end of a step, with or without an error.
	EventStepDone EventKind = "step_done"
	// EventLog is a line the agent wrote while it was running.
	EventLog EventKind = "log"
	// EventUsage is the partial spend of the review, updated while the agent
	// works. Only agents that speak stream-json emit this.
	EventUsage EventKind = "usage"
	// EventLimits is Claude's quota, which rides along in the stream of whoever
	// is running.
	EventLimits EventKind = "limits"
)

// Event is the progress of a review. It's what lets the TUI and the web
// interface show which agent is running right now instead of a blind spinner.
type Event struct {
	Kind  EventKind
	Index int // step position, starting at 0
	Total int
	Name  string
	// Duration is only filled in on EventStepDone.
	Duration time.Duration
	Err      error
	// Text and Stream ("stdout" or "stderr") only come on EventLog. Agent is who
	// wrote the line: the step's agent or a sub-agent it spawned.
	Text   string
	Stream string
	Agent  string
	// Usage only comes on EventUsage: the spend of the whole review so far,
	// previous steps included.
	Usage Usage
	// Limits only comes on EventLimits.
	Limits Limits
}

// StepResult is the output of one review step.
type StepResult struct {
	Name     string
	Body     string
	Duration time.Duration
	// Usage is how much model this step spent.
	Usage Usage
	Err   error
}

// Result is the output of a review.
type Result struct {
	PR gh.PR
	// Agent is the name of the agent or pipeline that ran.
	Agent string
	// Posts says whether this agent publishes to the PR on its own — whoever is
	// going to publish again on top of it needs to know.
	Posts bool
	// Body is the report: the single step's output, or the steps concatenated.
	Body  string
	Steps []StepResult
	// Usage is the spend of the whole review: the sum of the steps.
	Usage     Usage
	Duration  time.Duration
	Truncated bool
	// Workdir is the temporary clone where the agents ran. Empty when checkout
	// is turned off; it only outlives the review with KeepWorkspace.
	Workdir string
	// StoppedAt is the index of the `publish` step where execution stopped. -1
	// when the selection ran to the end. The Runner doesn't publish: publishing
	// waits for you to choose what goes to the PR, and that isn't its job.
	StoppedAt int
}

// Runner runs the configured agents.
type Runner struct {
	cfg *config.Config
	// KeepWorkspace keeps the temporary clone after the review.
	KeepWorkspace bool
	// PricingTable is the price table used to compute the cost in USD.
	// If nil, the cost comes from the total_cost_usd field of the stream-json
	// (Claude Code).
	PricingTable *pricing.Table
}

// New creates a Runner.
func New(cfg *config.Config) *Runner { return &Runner{cfg: cfg} }

// Review prepares the PR material and runs the user's selection — a single
// agent or a whole pipeline, always on the same clone.
//
// With checkout on, the repository is cloned into a temporary folder with the
// PR checked out and the agents run inside it — that's what allows using an
// agent that navigates the code (the review-fleet skill, for example) instead
// of just reading a diff pasted into the prompt.
//
// onEvent, if not nil, receives the step-by-step progress. It's called from
// the review's goroutine, so the listener must not block in it.
func (r *Runner) Review(ctx context.Context, pr gh.PR, choice config.Choice, onEvent func(Event)) (Result, error) {
	return r.run(ctx, pr, choice, nil, onEvent)
}

// Publish runs the publishing agent over a review that is already done — what
// you read on screen. The file path and the text go into the prompt through
// the {{review_file}} and {{review}} placeholders; the agent doesn't redo the
// review, it publishes that one.
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

	// Before the clone and before any step: one bad step refuses the whole job,
	// instead of failing midway through a review that can still be published.
	for _, s := range choice.Steps {
		err := checkLauncher(s)
		if err == nil {
			err = checkSettingSources(s.Args)
		}
		if err == nil {
			err = checkPathFlags(s.Args, "")
		}
		if err != nil {
			return Result{}, fmt.Errorf("`%s`: %w", s.Name, err)
		}
	}
	// The publishing skill works over the PR's clone (it checks that the repo it
	// is about to comment on is the PR's): without one it has nothing to check.
	if step := builtinSemClone(choice); step != "" {
		return Result{}, fmt.Errorf("`%s` runs a skill built into Bazel that works over the PR's clone — turn checkout on for it, or point it at a skill of your own", step)
	}

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

	// Now that the clone has a path: a flag that reads a file from it hands the
	// PR's content to the agent as configuration.
	for _, s := range choice.Steps {
		if err := checkPathFlags(s.Args, workdir); err != nil {
			return Result{}, fmt.Errorf("`%s`: %w", s.Name, err)
		}
	}

	// Claude Code gets the embedded skills as a plugin in a folder of Bazel's,
	// never in the clone, and that folder goes with the review — on success,
	// error or cancel alike.
	var plugin string
	if runsClaudeAnywhere(choice) {
		dir, err := newRuntime()
		if err != nil {
			return Result{}, err
		}
		defer os.RemoveAll(dir)
		if err := skills.Materialize(dir); err != nil {
			return Result{}, err
		}
		plugin = dir
	}

	diff, truncated, err := r.material(ctx, pr, choice)
	if err != nil {
		return Result{}, err
	}

	steps := make([]StepResult, 0, total)
	// fechado is the spend of the steps that have already finished: the partial
	// of a step in progress is added to it, so the number on screen is the whole
	// review's.
	var fechado Usage
	stopped := -1
	var neutralized []string
	for i := 0; i < len(choice.Steps); i++ {
		step := choice.Steps[i]
		// `publish` belongs to Bazel, not to the Runner: it returns what came out
		// and the caller shows you what goes to the PR before sending it.
		if step.Reserved != "" {
			stopped = i
			break
		}
		emit(Event{Kind: EventStep, Index: i, Total: total, Name: step.Name})

		// Right before the agent starts, inside exec, with nothing in between:
		// an earlier step can run commands in the clone, bring the files back
		// with a git checkout, or swap a path the args name for a link into the
		// clone. A step that does not pass stops the review: an agent never
		// runs next to the PR's configuration.
		guard := func() (string, error) {
			if workdir == "" {
				return "", nil
			}
			removed, err := workspace.Neutralize(workdir)
			if err != nil {
				return "", fmt.Errorf("%w: neutralizing the PR's agent configuration: %w", errRefused, err)
			}
			if len(removed) > 0 {
				for _, rel := range removed {
					if !slices.Contains(neutralized, rel) {
						neutralized = append(neutralized, rel)
					}
				}
				emit(Event{Kind: EventLog, Index: i, Total: total, Name: step.Name, Stream: "stdout", Agent: step.Name,
					Text: "· removed from the clone, so that no agent runs them: " + strings.Join(removed, ", ")})
			}
			if err := checkPathFlags(step.Args, workdir); err != nil {
				return "", fmt.Errorf("%w: `%s`: %w", errRefused, step.Name, err)
			}
			return neutralizedNote(neutralized), nil
		}

		dir := workdir
		if !step.Checkout {
			dir = "" // inherits the current directory
		}
		prompt := buildPrompt(step.Prompt, step.Task, pr, diff, workdir, extra)
		stepStart := time.Now()
		body, used, err := r.exec(ctx, step, prompt, dir, plugin, guard,
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
		// A refused step ends the job: it must not turn into one failed section
		// of a review that can still be published.
		if errors.Is(err, errRefused) {
			return Result{}, err
		}
		res := StepResult{Name: step.Name, Body: strings.TrimSpace(body), Duration: time.Since(stepStart), Usage: used, Err: err}
		if err == nil && res.Body == "" {
			res.Err = fmt.Errorf("the agent `%s` returned nothing", step.Command)
		}
		fechado.add(used)
		steps = append(steps, res)
		emit(Event{Kind: EventStepDone, Index: i, Total: total, Name: step.Name, Duration: res.Duration, Err: res.Err})

		// Cancellation aborts the whole queue: the clone is gone, and pushing on
		// with the next steps only burns time on a review nobody wants anymore.
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
		StoppedAt: stopped,
	}, nil
}

// gitGuard is the git configuration of every agent with a clone. A folder the PR
// versions can be a repository of its own, with HEAD and a config: a `git status`
// inside it runs the core.fsmonitor that config names. These settings come in
// through the environment, so no config of the PR's overrides them.
var gitGuard = [][2]string{
	{"safe.bareRepository", "explicit"},
	{"core.fsmonitor", "false"},
	{"core.hooksPath", "/dev/null"},
}

// gitGuardEnv returns the variables that add gitGuard to env, after any entries
// GIT_CONFIG_COUNT already has there: the user's stay, and the last one wins.
func gitGuardEnv(env []string) []string {
	base := 0
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GIT_CONFIG_COUNT="); ok {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				base = n
			}
		}
	}
	out := make([]string, 0, 2*len(gitGuard)+1)
	for i, kv := range gitGuard {
		out = append(out, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", base+i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", base+i, kv[1]))
	}
	return append(out, fmt.Sprintf("GIT_CONFIG_COUNT=%d", base+len(gitGuard)))
}

// errRefused marks a step Bazel would not start: the whole job stops, instead of
// that step failing inside a review.
var errRefused = errors.New("refused to start the agent")

// neutralizedNote tells the agent why those paths are missing from the working
// tree. The review still covers them: they are the PR's change like any other,
// and the ones a reviewer most wants to read.
func neutralizedNote(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return "\n\nBazel removed these from the working tree before you ran, because agent CLIs execute them: " +
		strings.Join(paths, ", ") + ". They are still in git and still part of the review: the diff shows them, and `git show HEAD:<path>` reads them."
}

// runsClaude says whether a step runs Claude Code: its command is `claude`, or
// resolves to it, or it speaks Claude's stream-json. Only the command and the
// format count, never the args: a `--model claude` on another CLI is not Claude
// Code, and flags injected there break it. A command that shows neither (a
// wrapper called `cc`) counts as another CLI, and so does a shell, whose
// `sh -c '…'` takes no flags for what it calls: workspace.Neutralize is what
// protects those runs.
func runsClaude(step config.ResolvedAgent) bool {
	if isShell(step.Command) || isLauncher(step.Command) {
		return false
	}
	return isClaudeCommand(step.Command) || outputFormat(step.Format, step.Args) == "claude-stream"
}

// programName is a command's base name as a program: lower case, because the
// macOS and Windows filesystems ignore case, and with no Windows extension.
func programName(command string) string {
	name := strings.ToLower(filepath.Base(strings.ReplaceAll(command, `\`, "/")))
	for _, ext := range []string{".exe", ".cmd", ".bat"} {
		name = strings.TrimSuffix(name, ext)
	}
	return name
}

// isClaudeCommand says whether command is Claude Code, by name or through PATH
// and symlinks: an npm install ends in its package, the native one in
// ~/.local/share/claude/versions/<version>, where the file is the version.
func isClaudeCommand(command string) bool {
	if programName(command) == "claude" {
		return true
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return false
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	slashed := filepath.ToSlash(real)
	return programName(real) == "claude" || strings.Contains(slashed, "/@anthropic-ai/claude-code/") ||
		strings.Contains(slashed, "/claude/versions/")
}

// isShell says whether command is a shell.
func isShell(command string) bool {
	switch programName(command) {
	case "sh", "bash", "zsh", "dash", "ksh", "fish":
		return true
	}
	return false
}

// isLauncher says whether command picks what it runs at run time. With the
// clone as working directory, that choice is the PR's: npx runs the
// node_modules the PR versions, an .npmrc points the registry at its server,
// and a .yarnrc runs a file of its own — all before any flag of Bazel's counts.
func isLauncher(command string) bool {
	switch programName(command) {
	case "npx", "npm", "pnpx", "pnpm", "yarn", "bun", "bunx", "uv", "uvx", "pipx", "env":
		return true
	}
	return false
}

// checkLauncher refuses a command that would decide inside the clone what to
// run: a launcher, or a relative path, which resolves in the clone too.
func checkLauncher(step config.ResolvedAgent) error {
	if !step.Checkout {
		return nil
	}
	if isLauncher(step.Command) {
		return fmt.Errorf("`%s` is not supported with checkout on: it resolves what it runs from the working directory (package.json, node_modules, .npmrc, .yarnrc), and that is the PR's clone — point command at an installed binary, and set environment variables with env:", step.Command)
	}
	if strings.ContainsAny(step.Command, `/\`) && !filepath.IsAbs(step.Command) {
		return fmt.Errorf("command %q is a relative path, which resolves inside the PR's clone with checkout on: use an installed binary or an absolute path", step.Command)
	}
	return nil
}

// runsClaudeAnywhere says whether any step of the choice runs Claude Code.
func runsClaudeAnywhere(choice config.Choice) bool {
	for _, s := range choice.Steps {
		if s.Reserved == "" && runsClaude(s) {
			return true
		}
	}
	return false
}

// newRuntime creates the folder one review's embedded skills are written to,
// under <BAZEL_HOME>/runtime: Bazel's own, outside the PR's clone, and only
// readable by the user.
func newRuntime() (string, error) {
	home, err := config.Dir()
	if err != nil {
		return "", err
	}
	// Absolute: the agent runs with the clone as its working directory, and a
	// relative BAZEL_HOME would point --plugin-dir inside it.
	home, err = filepath.Abs(home)
	if err != nil {
		return "", err
	}
	parent := filepath.Join(home, "runtime")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(parent, "")
}

// confineClaude adds the flags that keep the PR's own Claude Code
// configuration from running on this machine, whatever args the user set:
//
//   - --setting-sources user: the clone's .claude/settings.json is never read,
//     so its hooks (which run shell commands, with the user's gh login at hand),
//     permissions and env do not apply;
//   - --strict-mcp-config: MCP servers come only from --mcp-config, never from
//     the clone's .mcp.json, which a PR can also switch on from its settings;
//   - --plugin-dir: the embedded skills, which --setting-sources user would
//     otherwise hide along with the clone's .claude/skills.
//
// The flags go first and never at the end: after a `--`, anything is plain text
// to the CLI, and an appended flag would look added and do nothing. For the same
// reason a flag found after a `--` does not count as already there.
// checkSettingSources has already refused any --setting-sources but user.
func confineClaude(args []string, plugin string) []string {
	own := args
	if end := slices.Index(args, "--"); end >= 0 {
		own = args[:end]
	}
	var flags []string
	if !slices.ContainsFunc(own, func(a string) bool {
		return a == "--setting-sources" || strings.HasPrefix(a, "--setting-sources=")
	}) {
		flags = append(flags, "--setting-sources", "user")
	}
	if !slices.Contains(own, "--strict-mcp-config") {
		flags = append(flags, "--strict-mcp-config")
	}
	if plugin != "" {
		flags = append(flags, "--plugin-dir", plugin)
	}
	return slices.Concat(flags, args)
}

// checkSettingSources refuses a --setting-sources other than user, wherever it
// is in the args: there is no safe way to honor one that reads the project.
func checkSettingSources(args []string) error {
	for i, a := range args {
		value, found := "", false
		switch {
		case a == "--setting-sources":
			found = true
			if i+1 < len(args) {
				value = args[i+1]
			}
		case strings.HasPrefix(a, "--setting-sources="):
			found, value = true, strings.TrimPrefix(a, "--setting-sources=")
		}
		if found && strings.TrimSpace(value) != "user" {
			return fmt.Errorf("the agent's args set --setting-sources %q, but Bazel runs Claude Code with --setting-sources user only: any other source reads the PR's own .claude/settings.json, whose hooks run commands on this machine — remove the flag from the args", value)
		}
	}
	return nil
}

// pathFlags are the CLI flags whose value is read as configuration: settings
// with hooks, MCP servers that start commands, plugins, folders the agent gets
// access to, system prompts. In a file the PR controls, each one is the PR
// configuring the agent.
var pathFlags = []string{
	"--settings", "--mcp-config", "--plugin-dir", "--add-dir",
	"--append-system-prompt-file", "--system-prompt-file",
}

// checkPathFlags refuses a pathFlag in the args of any step unless its value
// is an absolute path outside the clone. A relative path resolves inside the
// clone, where the agent runs; so does anything that is not a path at all,
// like the inline JSON --settings and --mcp-config also take, which a config
// has no reason to need here and which cannot be told apart from a mistake.
// Refusing is what keeps a misconfigured step from running at all: --mcp-config
// and --add-dir take several values, and all of them are checked.
func checkPathFlags(args []string, clone string) error {
	for i, a := range args {
		for _, flag := range pathFlags {
			var values []string
			switch {
			case a == flag:
				for _, v := range args[i+1:] {
					if strings.HasPrefix(v, "-") {
						break
					}
					values = append(values, v)
				}
				if len(values) == 0 {
					return fmt.Errorf("%s has no value: it must be an absolute path outside the PR's clone", flag)
				}
			case strings.HasPrefix(a, flag+"="):
				values = []string{strings.TrimPrefix(a, flag+"=")}
			}
			for _, v := range values {
				if !filepath.IsAbs(v) {
					return fmt.Errorf("%s %q: it must be an absolute path outside the PR's clone — a relative one resolves inside it, where the PR controls the file", flag, v)
				}
				if clone != "" && insideDir(v, clone) {
					return fmt.Errorf("%s %q is inside the PR's clone, where the PR controls the file: point it outside", flag, v)
				}
				if info, err := os.Lstat(v); err == nil && info.Mode()&os.ModeSymlink != 0 {
					if _, err := filepath.EvalSymlinks(v); err != nil {
						return fmt.Errorf("%s %q is a link that leads nowhere, so it cannot be checked: point it at a real file outside the PR's clone", flag, v)
					}
				}
			}
		}
	}
	return nil
}

// insideDir says whether path is dir or below it, comparing the paths as
// written and with symlinks resolved: on macOS the temp folder is itself a
// link, and a path outside can link back in, in any of its folders.
func insideDir(path, dir string) bool {
	within := func(p, d string) bool {
		rel, err := filepath.Rel(d, p)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	return within(filepath.Clean(path), filepath.Clean(dir)) || within(resolveExisting(path), resolveExisting(dir))
}

// resolveExisting resolves symlinks in the longest part of path that exists:
// a file not there yet, under a folder that links into the clone, is in the
// clone all the same.
func resolveExisting(path string) string {
	path = filepath.Clean(path)
	for prefix, rest := path, ""; ; {
		if real, err := filepath.EvalSymlinks(prefix); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(prefix)
		if parent == prefix {
			return path
		}
		prefix, rest = parent, filepath.Join(filepath.Base(prefix), rest)
	}
}

// builtinSemClone returns the step that calls an embedded skill with checkout
// off: the publishing skill checks the PR's clone, so it needs one.
func builtinSemClone(choice config.Choice) string {
	for _, s := range choice.Steps {
		if !s.Checkout && skills.IsBuiltin(skills.TaskSkill(s.Task)) {
			return s.Name
		}
	}
	return ""
}

// material downloads what the review needs before running any step. The diff
// only goes into the prompt if some template asks for it: with the clone at
// hand, the agent reads what it needs, and sending hundreds of KB along only
// burns context.
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

// PublishableBody is what goes to the PR: the body of only the steps whose
// output is publishable. In a `history-pr → review-fleet` pipeline the screen
// shows both — that's why you chained them — but the PR only gets the review.
// The history is context for whoever is going to review, not a comment for the
// author.
//
// The bool says whether anything was left out: when it's false the whole report
// is what gets published, and the file on disk already does the job.
func PublishableBody(res Result, choice config.Choice) (string, bool, error) {
	kept := stepsWhere(res, choice, true)
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

// ContextBody is the flip side of PublishableBody: the body of the steps that
// stay in Bazel. It's what the screen shows separately, to make clear it
// doesn't go to the PR.
func ContextBody(res Result, choice config.Choice) string {
	kept := stepsWhere(res, choice, false)
	if len(kept) == 0 {
		return ""
	}
	body, err := joinSteps(kept)
	if err != nil {
		// Every step that stays in Bazel failed. Without this note the section
		// would vanish from the screen, and the failure would only show up in the
		// steps box.
		notes := make([]string, 0, len(kept))
		for _, s := range kept {
			msg := "no output"
			if s.Err != nil {
				msg = firstLine(s.Err.Error())
			}
			notes = append(notes, fmt.Sprintf("> ✗ %s failed: %s", s.Name, msg))
		}
		return strings.Join(notes, "\n\n") + "\n"
	}
	return body
}

// stepsWhere splits the steps that go to the PR (publishes=true) from those
// that stay in Bazel (publishes=false).
func stepsWhere(res Result, choice config.Choice, publishes bool) []StepResult {
	kept := make([]StepResult, 0, len(res.Steps))
	for _, s := range res.Steps {
		if choice.StepPublishes(s.Name) == publishes {
			kept = append(kept, s)
		}
	}
	return kept
}

// joinSteps builds the final report. A single step comes out raw, as it always
// has; several become sections. All of them failing is the whole review
// failing.
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

// exec runs an agent and passes along what it writes, line by line, to onLog.
// That's what feeds the live log: with stdout in a buffer nobody would see
// anything until the process died.
//
// In stream-json mode stdout is JSONL of events — the log gets the readable
// version and the report comes from the final event. For any other agent
// stdout is the report itself, and goes raw to both.
// The model spend comes out along with the report — whoever reads the output
// is who knows how to count it — and flows out along the way through onUsage,
// which is what makes the count move on screen instead of only showing at the
// end.
func (r *Runner) exec(ctx context.Context, step config.ResolvedAgent, prompt, workdir, plugin string, guard func() (string, error), onLog func(stream, agent, text string), onUsage func(Usage), onLimits func(Limits)) (string, Usage, error) {
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

	args := step.Args
	if runsClaude(step) {
		args = confineClaude(step.Args, plugin)
	}
	cmd := exec.CommandContext(ctx, step.Command, args...)
	cmd.Dir = workdir // empty = inherits the current directory
	if len(step.Env) > 0 || workdir != "" {
		cmd.Env = os.Environ()
		for k, v := range step.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	if workdir != "" {
		cmd.Env = append(cmd.Env, gitGuardEnv(cmd.Env)...)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", Usage{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", Usage{}, err
	}
	// The last thing before the process: the smaller the gap between checking
	// the clone and reading it, the less there is to race.
	note, err := guard()
	if err != nil {
		stdout.Close()
		stderr.Close()
		return "", Usage{}, err
	}
	cmd.Stdin = strings.NewReader(prompt + note)
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
			// The count only goes up when it changes: a tool call doesn't
			// spend any tokens and doesn't need to wake up the browser.
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

	// The pipes have to reach EOF before Wait: it closes them.
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
	// With a pricing table configured, recompute the cost from the tokens and
	// the model — that is what lets the cost change without rebuilding the
	// binary.
	var byModel []Usage
	if m, ok := adapter.(interface{ models() []Usage }); ok {
		byModel = m.models()
	}
	return adapter.report(), priced(adapter.usage(), byModel, r.PricingTable), nil
}

func agentFailure(step config.ResolvedAgent, message string) error {
	if step.Format == "grok-stream" && strings.Contains(strings.ToLower(message), "max turns reached") {
		return fmt.Errorf("Grok reached its configured turn limit before completing the review")
	}
	return fmt.Errorf("agent `%s` failed: %s", step.Command, message)
}

// eachLine calls fn for each line read, with no size limit — a stream-json
// event with a large tool result overflows bufio.Scanner.
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

// lastLines keeps the tail of stderr, which is where the message explaining
// the failure usually is.
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

// buildPrompt replaces the template's placeholders with the PR's content.
// workdir is the temporary clone, or empty if checkout is turned off.
//
// task is the lens's instruction: it goes into the template's {{task}}. A
// template without {{task}} — a custom prompt written before the named agents
// — gets the instruction on the first line, which is where it was when there
// was only one template.
// extra carries the placeholders of an already finished review, which only the
// publishing agent uses.
func buildPrompt(tmpl, task string, pr gh.PR, diff, workdir string, extra map[string]string) string {
	body := strings.TrimSpace(pr.Body)
	if body == "" {
		body = "(no description)"
	}
	if workdir == "" {
		workdir = "(no local clone)"
	}
	// The task goes into the template before the substitution, and not as one
	// more Replacer pair: it has placeholders too ("/review-fleet {{number}}"),
	// and the Replacer doesn't re-expand what it has just inserted.
	task = strings.TrimSpace(task)
	if task != "" && !strings.Contains(tmpl, "{{task}}") {
		tmpl = "{{task}}\n\n" + tmpl
	}
	tmpl = strings.ReplaceAll(tmpl, "{{task}}", task)

	pairs := []string{}
	for k, v := range extra {
		pairs = append(pairs, k, v)
	}
	// The extras go in first: {{review_file}} may be inside the task.
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
