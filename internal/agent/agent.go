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

	// An embedded skill only exists inside the binary; Claude Code finds it on
	// disk, in the working directory's .claude/skills. Its place is the PR's
	// clone: it's born and dies with it, and the machine of whoever runs Bazel
	// doesn't get anything in ~/.claude that nobody asked for.
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

	steps := make([]StepResult, 0, total)
	// fechado is the spend of the steps that have already finished: the partial
	// of a step in progress is added to it, so the number on screen is the whole
	// review's.
	var fechado Usage
	stopped := -1
	for i := 0; i < len(choice.Steps); i++ {
		step := choice.Steps[i]
		// `publish` belongs to Bazel, not to the Runner: it returns what came out
		// and the caller shows you what goes to the PR before sending it.
		if step.Reserved != "" {
			stopped = i
			break
		}
		emit(Event{Kind: EventStep, Index: i, Total: total, Name: step.Name})

		dir := workdir
		if !step.Checkout {
			dir = "" // inherits the current directory
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

// usaBuiltin says whether any step calls a skill that shipped in the binary.
func usaBuiltin(choice config.Choice) bool {
	for _, s := range choice.Steps {
		if skills.IsBuiltin(skills.TaskSkill(s.Task)) {
			return true
		}
	}
	return false
}

// builtinSemClone returns the step that calls an embedded skill without a clone
// to write it into — without the PR's working directory it has nowhere to
// exist, and writing it into the folder of whoever ran Bazel would be littering
// someone else's project.
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
	cmd.Dir = workdir // empty = inherits the current directory
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
