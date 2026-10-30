package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/beroni/bazel/internal/agent"
	"github.com/beroni/bazel/internal/config"
	"github.com/beroni/bazel/internal/gh"
	"github.com/beroni/bazel/internal/store"
)

// State is the lifecycle of a review in the queue.
type State string

const (
	StateQueued   State = "queued"
	StateRunning  State = "running"
	StateDone     State = "done"
	StateFailed   State = "failed"
	StateCanceled State = "canceled"
)

var (
	// errPublishBusy refuses a second publish while one is still in flight.
	errPublishBusy = errors.New("that review is being published right now")
	// errAlreadyPosted refuses publishing a review that already went to the PR.
	errAlreadyPosted = errors.New("that review was already published")
)

// maxJobs caps the in-memory history. The reviews that matter are already
// saved to disk; this is just the session's queue.
const maxJobs = 200

// A review's log is a window, not a file: a chatty agent writes thousands of
// lines and what matters on screen is the end. These two limits are the memory
// ceiling per job.
const (
	maxLogLines     = 500
	maxLogLineRunes = 2000
)

// logLine is a line an agent wrote. Seq is monotonic per job: it is how the
// browser asks only for what it has not seen yet.
type logLine struct {
	Seq  int `json:"seq"`
	Step int `json:"step"`
	// Agent is who wrote it: the step's agent or one of its sub-agents. It is
	// by this that the page separates the lenses running in parallel.
	Agent  string `json:"agent"`
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

// jobStep is a review step — an agent of the choice — as the page sees it.
type jobStep struct {
	Name      string
	State     State
	StartedAt time.Time
	Duration  time.Duration
	Err       string
}

// Job is a review requested through the interface: queued, running or
// finished.
//
// A review takes minutes — the default `timeout_seconds` is 1800 — and no HTTP
// request holds that long. So the handler only enqueues and returns the id;
// the result reaches the browser through the SSE of /api/events.
type Job struct {
	ID   string
	PR   gh.PR
	Mine bool

	// Choice is the agent or pipeline chosen for this PR; Steps is the
	// progress of each of its steps, which is what the page animates.
	Choice  config.Choice
	Steps   []jobStep
	Cloning bool

	// publish, when set, says this job is carrying a review to the PR instead
	// of producing one: the agent takes the already-read review and puts it
	// there. It happens in two ways — in the review's own card, after the user
	// has read it (pubChoice is the post agent and stepBase says where its
	// steps go in Steps), or in a card of its own, when the review came from
	// disk and there is no origin job at all.
	publish   *publishInput
	pubChoice config.Choice
	stepBase  int

	// logs is the window of what the agents wrote; logSeq is the next line
	// number and dropped counts what has already fallen off the front of the
	// window.
	logs    []logLine
	logSeq  int
	dropped int

	State      State
	QueuedAt   time.Time
	StartedAt  time.Time
	FinishedAt time.Time

	Result agent.Result
	// Live is the partial spend of a review in progress: the closed spend of
	// the steps that already finished plus what the current step has consumed
	// so far.
	Live    agent.Usage
	Err     string
	SavedTo string
	Posted  bool
	PostErr string
	// AwaitsPublish is the pipeline that ended in a `publish` step: it does
	// not publish on its own, it waits for you to choose what goes to the PR
	// and send it.
	AwaitsPublish bool
	// posting is set under the lock while Post is inside gh.Comment, which
	// runs without the lock: a second click in that window is refused.
	posting bool
	// gen counts how many times the job went into the queue, bumped under the
	// lock on every send. A publication canceled while queued and sent again
	// leaves two entries for the same job; only the one carrying the current
	// gen may run it.
	gen int

	cancel context.CancelFunc
}

func (j *Job) duration() time.Duration {
	switch {
	case j.StartedAt.IsZero():
		return 0
	case j.FinishedAt.IsZero():
		return time.Since(j.StartedAt)
	default:
		return j.FinishedAt.Sub(j.StartedAt)
	}
}

// publishInput is the ready review a publish job carries to the PR.
type publishInput struct {
	Path string
	Body string
	// Source is the saved review this input came from. Path is a copy in
	// publish/ whenever findings were dropped, so dedup compares Source.
	Source string
}

// discard removes the copy written in publish/ for this publication. The post
// agent is the only reader of Path, so once it has stopped (or never started)
// the file is dead weight; the saved review at Source is never touched.
func (in *publishInput) discard() {
	if in != nil && in.Path != "" && in.Path != in.Source {
		_ = os.Remove(in.Path)
	}
}

// publishInputFor builds what the post agent receives. With no finding
// unticked and the whole report publishable it is the review as is; if some
// finding was dropped or the pipeline had a step that does not go to the PR,
// the agent gets a copy of the file already trimmed in publish/, because the
// post skill reads the file, not the prompt.
//
// trimmed says the body received is no longer what is in path — that is what
// forces the copy even with no finding unticked.
func (m *Manager) publishInputFor(path, body string, skip []int, trimmed bool) (*publishInput, error) {
	if len(skip) == 0 && !trimmed {
		return &publishInput{Path: path, Body: body, Source: path}, nil
	}
	if len(skip) > 0 {
		body = store.DropFindings(body, skip)
	}
	copyPath, err := store.SavePublishCopy(m.reviewsDir, path, body)
	if err != nil {
		return nil, fmt.Errorf("could not write the review without the unticked findings: %w", err)
	}
	return &publishInput{Path: copyPath, Body: body, Source: path}, nil
}

// inPlacePublish says the publication is running inside the card of the review
// that started it: the post agent's steps come after the review's, and what it
// writes does not replace the review on screen.
func (j *Job) inPlacePublish() bool { return j.publish != nil && j.stepBase > 0 }

// queued is one entry in the queue: the job and the gen it was sent with.
type queued struct {
	job *Job
	gen int
}

// enqueueLocked bumps the job's gen and returns the entry to send. Call with
// the Manager lock held, in the same critical section that sets StateQueued.
func (j *Job) enqueueLocked() queued {
	j.gen++
	return queued{job: j, gen: j.gen}
}

// Manager is the review queue and the pool that consumes it.
type Manager struct {
	cfg        *config.Config
	runner     *agent.Runner
	reviewsDir string
	hub        *Hub
	ctx        context.Context

	queue chan queued

	// running counts the jobs inside run, for Wait.
	running atomic.Int32

	mu    sync.Mutex
	jobs  map[string]*Job
	order []string
	seq   int

	// limits is the latest reading of Claude's quota. It belongs to no job: it
	// is the machine's, and holds for the whole page.
	limitsMu sync.RWMutex
	limits   agent.Limits
}

// NewManager starts the worker pool. concurrency caps how many agents run at
// once: each review clones a repository and starts a process, so this is what
// separates "reviewing two PRs" from "bringing down the laptop".
func NewManager(ctx context.Context, cfg *config.Config, reviewsDir string, concurrency int, keep bool, hub *Hub) *Manager {
	if concurrency < 1 {
		concurrency = 1
	}
	runner := agent.New(cfg)
	runner.KeepWorkspace = keep
	prices := cfg.Agent.Prices()
	runner.PricingTable = &prices

	m := &Manager{
		cfg:        cfg,
		runner:     runner,
		reviewsDir: reviewsDir,
		hub:        hub,
		ctx:        ctx,
		queue:      make(chan queued, 256),
		jobs:       map[string]*Job{},
	}
	for range concurrency {
		go m.worker()
	}
	return m
}

// Enqueue puts a PR in the queue with the chosen agent. The same PR with the
// same agent already waiting or running returns the existing job instead of
// cloning the repository twice — with another agent it is another review, and
// it goes into the queue.
func (m *Manager) Enqueue(pr gh.PR, mine bool, choice config.Choice) (jobView, error) {
	if len(choice.Steps) == 0 {
		choice = m.cfg.DefaultChoice()
	}
	// With no agent at all there is no review to enqueue: the list starts
	// empty and it is the page that fills it with the installed skills.
	if len(choice.Steps) == 0 {
		return jobView{}, config.ErrNoAgents
	}
	m.mu.Lock()
	for _, id := range m.order {
		j := m.jobs[id]
		if j.PR.Key() == pr.Key() && j.Choice.Name == choice.Name &&
			(j.State == StateQueued || j.State == StateRunning) {
			v := j.view()
			m.mu.Unlock()
			return v, nil
		}
	}
	m.seq++
	steps := make([]jobStep, 0, len(choice.Steps))
	for _, st := range choice.Steps {
		// The `publish` step does not get its own row: what shows on screen is
		// the publishing agent, which joins the end of the list when you send
		// — and it is the same path as the publish button.
		if st.Reserved == config.StepPublish {
			continue
		}
		steps = append(steps, jobStep{Name: st.Name, State: StateQueued})
	}
	job := &Job{
		ID:       fmt.Sprintf("j%d", m.seq),
		PR:       pr,
		Mine:     mine,
		Choice:   choice,
		Steps:    steps,
		State:    StateQueued,
		QueuedAt: time.Now(),
	}
	m.jobs[job.ID] = job
	m.order = append(m.order, job.ID)
	m.trimLocked()
	err := m.sendLocked(job)
	m.mu.Unlock()
	m.publish(job)
	return m.mustView(job.ID), err
}

// sendLocked sends a freshly registered job to the queue without blocking.
// Call it holding the Manager lock: the send and the failure on a full queue
// happen in the same critical section that registered the job as queued, so
// nobody sees a queued job that never entered the queue — neither a
// concurrent Enqueue of the same PR, nor a Cancel or Remove that finish would
// later trample. The select has a default, so holding the lock here does not
// stall the Manager when the queue is full; the worker that receives the job
// only waits for the lock to start.
func (m *Manager) sendLocked(job *Job) error {
	select {
	case m.queue <- job.enqueueLocked():
		return nil
	default:
		m.finishLocked(job, StateFailed, "queue is full — wait for the reviews in flight")
		return errors.New("queue is full")
	}
}

// trimLocked drops the oldest finished jobs. Call with the lock held.
func (m *Manager) trimLocked() {
	for len(m.order) > maxJobs {
		for i, id := range m.order {
			j := m.jobs[id]
			// A card waiting on you is the only place that review goes to the
			// PR from with the choice made on screen: it stays like a live one.
			if j.State == StateQueued || j.State == StateRunning || j.AwaitsPublish {
				continue
			}
			delete(m.jobs, id)
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
		// Nothing to drop: everything in the queue is active.
		if len(m.order) > maxJobs {
			return
		}
	}
}

// Wait gives the running jobs up to d to return after the context is cancelled.
// An agent runs in a process group of its own, so a Ctrl-C at the terminal does
// not reach it: it dies when its job sees the cancel, and Bazel has to still be
// here for that.
func (m *Manager) Wait(d time.Duration) {
	for deadline := time.Now().Add(d); m.running.Load() > 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
}

func (m *Manager) worker() {
	for q := range m.queue {
		m.run(q)
	}
}

func (m *Manager) run(q queued) {
	m.running.Add(1)
	defer m.running.Add(-1)
	job := q.job
	ctx, cancel := context.WithCancel(m.ctx)
	defer cancel()

	m.mu.Lock()
	// An entry from an earlier send of this job: the job was canceled and
	// sent again, and the current entry is the one that runs it. Whatever the
	// job holds now belongs to that entry, so this one touches nothing.
	if q.gen != job.gen {
		m.mu.Unlock()
		return
	}
	// Canceled while it waited in the queue.
	// Whoever canceled or removed it already discarded its copy.
	if job.State != StateQueued {
		m.mu.Unlock()
		return
	}
	job.State = StateRunning
	if job.StartedAt.IsZero() {
		job.StartedAt = time.Now()
	}
	job.cancel = cancel
	// The publication this run takes, captured under the lock.
	in := job.publish
	m.mu.Unlock()
	m.publish(job)

	onEvent := func(e agent.Event) { m.applyEvent(job, e) }
	var (
		res agent.Result
		err error
	)
	switch {
	case in != nil:
		res, err = m.runner.Publish(ctx, job.PR, job.pubChoice, in.Path, in.Body, onEvent)
		// The post agent has stopped, whatever the outcome: nothing reads the
		// copy again, and one per click would pile up in publish/.
		in.discard()
	default:
		res, err = m.runner.Review(ctx, job.PR, job.Choice, onEvent)
	}
	if err != nil {
		// Publishing inside the review's card, what failed was the
		// publication: the review is still read, saved and publishable again,
		// and wiping it from the screen would cost exactly what the user
		// wants.
		if job.inPlacePublish() {
			msg := err.Error()
			if ctx.Err() != nil {
				msg = "publishing canceled"
			}
			m.failPublish(job, msg)
			return
		}
		state := StateFailed
		if ctx.Err() != nil {
			state = StateCanceled
		}
		m.finish(job, state, err.Error())
		return
	}

	// A report pasted inside a ```markdown block comes out of the block before
	// anything else: inside it the headings are text, and the screen, the disk
	// and the PR would show the raw markdown instead of the review.
	//
	// Step by step, and not only on the final body, because each step's body
	// is reused later: it is where what goes to the PR comes from when only
	// some of the steps are publishable.
	desembrulha(&res)

	// fresh is a review that just ran, as opposed to a publish. Captured
	// before the in-place branch below clears job.publish.
	fresh := job.publish == nil

	// A publish report does not become a file: the review is already saved, and
	// this job only records what went to the PR.
	var (
		path    string
		saveErr error
	)
	if fresh {
		path, saveErr = store.Save(m.reviewsDir, res, m.runner.PricingTable)
	}

	// The PR enters the index: it is the ✓ in the list, and the commit
	// recorded here is what later flags a change to the PR after the review.
	var markErr error
	if fresh {
		markErr = store.MarkReviewed(m.reviewsDir, res, path)
	} else {
		markErr = store.MarkPosted(m.reviewsDir, res.PR.Key())
	}

	// The publish step only waits when there is something to choose: a
	// saved review, a choice whose output may go to the PR, no agent that
	// already posted on its own, and at least one publishable step that
	// produced a body. job.Choice does not change after Enqueue.
	gated := fresh && res.StoppedAt >= 0
	awaits := false
	if gated && saveErr == nil && job.Choice.Publishable && !job.Choice.Posts {
		_, _, bodyErr := agent.PublishableBody(res, job.Choice)
		awaits = bodyErr == nil
	}

	m.mu.Lock()
	if job.inPlacePublish() {
		// The post agent's report does not take the review's place: the card
		// keeps showing what the user read, now marked as published and with
		// the spend of both rounds added up.
		job.Posted = true
		job.PostErr = ""
		job.AwaitsPublish = false
		job.Result.Usage = job.Result.Usage.Plus(res.Usage)
		job.publish = nil
		job.Live = agent.Usage{}
	} else {
		job.Result = res
		job.SavedTo = path
	}
	job.State = StateDone
	switch {
	case saveErr != nil:
		job.Err = "review done, but I could not save it to disk: " + saveErr.Error()
		// A pipeline with a publish step must not fall back to the done card
		// and its one-click publish buttons: that would bypass the gate exactly
		// when something already went wrong. Failed has no publish path.
		if gated {
			job.State = StateFailed
		}
	case markErr != nil:
		// Without the index the PR shows up again as unreviewed, and the ⟳ for a
		// change after the review stops working — better to warn.
		job.Err = "done, but I could not update the reviewed index: " + markErr.Error()
	}
	job.FinishedAt = time.Now()
	job.Cloning = false
	job.cancel = nil
	// `publish` step: the pipeline does not publish by itself. The card halts
	// there, showing what goes to the PR finding by finding, and you are the
	// one who sends it — through the same button as always.
	if awaits {
		job.AwaitsPublish = true
	}
	m.mu.Unlock()
	m.publish(job)
}

// desembrulha takes the report out of a ```markdown block — the body and each
// step's. The agent sometimes returns the whole report pasted inside a code
// block, and then none of it renders: the page shows the raw markdown.
func desembrulha(res *agent.Result) {
	res.Body = store.Unwrap(res.Body)
	for i := range res.Steps {
		res.Steps[i].Body = store.Unwrap(res.Steps[i].Body)
	}
}

// failPublish hands the card back the review it already had: the publication
// failed or was cancelled, but the review is still on screen, saved on disk
// and publishable again. The post agent's steps stay as they are — they are
// where you see where the publication broke.
func (m *Manager) failPublish(job *Job, msg string) {
	m.mu.Lock()
	m.failPublishLocked(job, msg)
	m.mu.Unlock()
	m.publish(job)
}

// failPublishLocked is failPublish for callers already holding the lock.
func (m *Manager) failPublishLocked(job *Job, msg string) {
	for i := job.stepBase; i < len(job.Steps); i++ {
		if st := &job.Steps[i]; st.State == StateQueued || st.State == StateRunning {
			st.State = StateFailed
		}
	}
	job.PostErr = msg
	job.publish.discard()
	job.publish = nil
	job.Live = agent.Usage{}
	job.State = StateDone
	job.FinishedAt = time.Now()
	job.Cloning = false
	job.cancel = nil
}

// applyEvent records a step's progress and notifies the browsers. It is called
// from the runner's goroutine, so it touches the job under the lock and only
// publishes after releasing it.
func (m *Manager) applyEvent(job *Job, e agent.Event) {
	// A log line does not change the job's state and does not go to the SSE: a
	// chatty agent would generate thousands of events and each one would wake
	// every open browser. The browser fetches the log incrementally at
	// /api/jobs/{id}/log.
	if e.Kind == agent.EventLog {
		m.mu.Lock()
		job.appendLog(e.Index+job.stepBase, e.Agent, e.Stream, e.Text)
		m.mu.Unlock()
		return
	}

	// The Claude quota is not the job's: it arrives through its stream, but
	// holds for the whole machine and goes to the page through a notice of its
	// own.
	if e.Kind == agent.EventLimits {
		m.limitsMu.Lock()
		m.limits = e.Limits
		m.limitsMu.Unlock()
		m.hub.Broadcast("limits", e.Limits)
		return
	}

	// The partial spend moves on its own: it touches no step, only the number
	// the page shows while the agent works.
	if e.Kind == agent.EventUsage {
		m.mu.Lock()
		job.Live = e.Usage
		m.mu.Unlock()
		m.publish(job)
		return
	}

	m.mu.Lock()
	// The runner counts the steps of its round from zero; publishing inside
	// the review's card, that round starts after the steps already there —
	// stepBase is what puts the event on the right row.
	i := e.Index + job.stepBase
	switch {
	case e.Kind == agent.EventClone:
		job.Cloning = true
	case e.Index < 0 || i >= len(job.Steps):
		// Event for a step that does not exist: nothing to show.
	case e.Kind == agent.EventStep:
		job.Cloning = false
		job.Steps[i].State = StateRunning
		job.Steps[i].StartedAt = time.Now()
	case e.Kind == agent.EventStepDone:
		st := &job.Steps[i]
		st.Duration = e.Duration
		st.State = StateDone
		if e.Err != nil {
			st.State = StateFailed
			st.Err = e.Err.Error()
		}
	}
	m.mu.Unlock()
	m.publish(job)
}

func (m *Manager) finish(job *Job, state State, errMsg string) {
	m.mu.Lock()
	m.finishLocked(job, state, errMsg)
	m.mu.Unlock()
	m.publish(job)
}

// finishLocked is finish for callers that already hold the Manager lock.
func (m *Manager) finishLocked(job *Job, state State, errMsg string) {
	job.State = state
	job.Err = errMsg
	job.FinishedAt = time.Now()
	job.Cloning = false
	job.cancel = nil
}

// appendLog stores a line, throwing away the oldest when the window fills.
// Call with the Manager lock held.
func (j *Job) appendLog(step int, who, stream, text string) {
	if r := []rune(text); len(r) > maxLogLineRunes {
		text = string(r[:maxLogLineRunes]) + "…"
	}
	j.logs = append(j.logs, logLine{Seq: j.logSeq, Step: step, Agent: who, Stream: stream, Text: text})
	j.logSeq++
	if len(j.logs) > maxLogLines {
		j.dropped += len(j.logs) - maxLogLines
		j.logs = j.logs[len(j.logs)-maxLogLines:]
	}
}

// logView is the piece of the log the browser does not have yet.
type logView struct {
	Lines []logLine `json:"lines"`
	// Next is the seq to ask for next time.
	Next int `json:"next"`
	// Dropped counts what the window discarded — the browser warns about the
	// gap.
	Dropped int `json:"dropped"`
	// Live says whether more is still coming; when false, the browser stops
	// asking.
	Live bool `json:"live"`
}

// Log returns the lines with seq >= from.
func (m *Manager) Log(id string, from int) (logView, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return logView{}, false
	}
	out := logView{
		Lines:   []logLine{},
		Next:    job.logSeq,
		Dropped: job.dropped,
		Live:    job.State == StateQueued || job.State == StateRunning,
	}
	for _, l := range job.logs {
		if l.Seq >= from {
			out.Lines = append(out.Lines, l)
		}
	}
	return out, true
}

// Remove takes a job out of the queue. A review still alive is cancelled
// first: the card disappears from the screen, and leaving the process running
// with nothing to show it would be a ghost agent burning tokens.
//
// It only leaves this session's memory — the saved markdown and the index of
// reviewed PRs stay where they are.
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("job %q does not exist", id)
	}
	cancel := job.cancel
	vivo := job.State == StateQueued || job.State == StateRunning
	if vivo {
		// Still queued, the post agent never started: nothing else will read
		// the copy. A running one is discarded by its worker when it stops.
		if job.State == StateQueued {
			job.publish.discard()
		}
		// The worker only finds out about the cancellation later; marking it here
		// keeps it from republishing a job the page already forgot.
		job.State = StateCanceled
		job.FinishedAt = time.Now()
	}
	delete(m.jobs, id)
	for i, other := range m.order {
		if other == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	m.mu.Unlock()

	if vivo && cancel != nil {
		cancel()
	}
	m.hub.Broadcast("job_gone", map[string]string{"id": id})
	return nil
}

// Cancel interrupts a review — queued or already running.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("job %q does not exist", id)
	}
	switch job.State {
	case StateQueued:
		// Cancelling a publication waiting in the queue does not cancel the
		// review already in the card: it goes back to being a finished review.
		// The worker sees the changed state and never gets to run the post
		// agent.
		if job.inPlacePublish() {
			m.failPublishLocked(job, "publishing canceled")
			m.mu.Unlock()
			m.publish(job)
			return nil
		}
		// A publication of a saved review that never left the queue: its
		// copy has no reader anymore.
		job.publish.discard()
		job.State = StateCanceled
		job.FinishedAt = time.Now()
		m.mu.Unlock()
		m.publish(job)
		return nil
	case StateRunning:
		cancel := job.cancel
		m.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil
	default:
		m.mu.Unlock()
		return fmt.Errorf("that review has already finished")
	}
}

// PublishWithAgent sends the post agent to carry to the PR the review the user
// just read. It is the other path to GitHub: Post's plain comment is Bazel
// writing; this is the agent publishing a review with inline comments.
//
// It runs in the review's own card: the post agent's steps come after the
// review's and the card goes back to running. Publishing is not a separate job
// — it is the end of the review — and a second card would only spread the same
// PR across two places in the queue.
//
// skip are the findings the user unticked on screen: they drop out of what
// goes to the PR, and the agent gets a copy of the review without them — the
// saved file stays whole.
func (m *Manager) PublishWithAgent(id string, skip []int) (jobView, error) {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return jobView{}, fmt.Errorf("job %q does not exist", id)
	}
	// Already publishing: return the card as is, instead of sending the post
	// agent to the same PR twice.
	if job.publish != nil && (job.State == StateQueued || job.State == StateRunning) {
		v := job.view()
		m.mu.Unlock()
		return v, nil
	}
	if job.State != StateDone {
		m.mu.Unlock()
		return jobView{}, errors.New("that review has not finished")
	}
	if job.posting {
		m.mu.Unlock()
		return jobView{}, errPublishBusy
	}
	if job.Posted {
		m.mu.Unlock()
		return jobView{}, errAlreadyPosted
	}
	if job.SavedTo == "" {
		m.mu.Unlock()
		return jobView{}, errors.New("that review was not saved to disk — there is nothing to publish")
	}
	// An agent whose output is not a review has no path to the PR. The
	// interface already hides the button; this is what stops a POST coming
	// from elsewhere.
	if !job.Choice.Publishable {
		m.mu.Unlock()
		return jobView{}, fmt.Errorf("what %q produces does not go to the PR", job.Choice.Name)
	}
	choice := m.cfg.PostChoice()
	if len(choice.Steps) == 0 {
		m.mu.Unlock()
		return jobView{}, config.ErrNoAgents
	}
	body, trimmed, err := agent.PublishableBody(job.Result, job.Choice)
	if err != nil {
		m.mu.Unlock()
		return jobView{}, err
	}
	in, err := m.publishInputFor(job.SavedTo, body, skip, trimmed)
	if err != nil {
		m.mu.Unlock()
		return jobView{}, err
	}

	// A previous publication that failed left its steps on the card; the new
	// attempt redoes them, it does not stack them.
	if job.stepBase > 0 {
		job.Steps = job.Steps[:job.stepBase]
	} else {
		job.stepBase = len(job.Steps)
	}
	for _, name := range choice.StepNames() {
		job.Steps = append(job.Steps, jobStep{Name: name, State: StateQueued})
	}
	job.publish = in
	job.pubChoice = choice
	job.State = StateQueued
	job.QueuedAt = time.Now()
	job.StartedAt = time.Time{}
	job.FinishedAt = time.Time{}
	job.Err = ""
	job.PostErr = ""
	job.Live = agent.Usage{}
	q := job.enqueueLocked()
	m.mu.Unlock()

	select {
	case m.queue <- q:
	default:
		// Only this send's publication fails: if it was canceled (and maybe
		// sent again) in the meantime, the job is no longer ours to touch.
		m.mu.Lock()
		if job.gen == q.gen && job.State == StateQueued {
			m.failPublishLocked(job, "queue is full — wait for the reviews in flight")
		}
		m.mu.Unlock()
		m.publish(job)
		return m.mustView(job.ID), errors.New("queue is full")
	}
	m.publish(job)
	return m.mustView(job.ID), nil
}

// PublishSaved carries to the PR a review that is on disk — one from a
// previous session, opened in the saved tab. It is the same path as
// PublishWithAgent, but starting from the file: a review outlives the server,
// and the chance to publish it has to outlive it too.
func (m *Manager) PublishSaved(pr gh.PR, mine bool, path, body string, skip []int) (jobView, error) {
	if strings.TrimSpace(path) == "" {
		return jobView{}, errors.New("no review file to publish")
	}
	in, err := m.publishInputFor(path, body, skip, false)
	if err != nil {
		return jobView{}, err
	}
	m.mu.Lock()
	// Already publishing this same file: return the job in progress.
	for _, id := range m.order {
		j := m.jobs[id]
		if j.publish != nil && j.publish.Source == path && (j.State == StateQueued || j.State == StateRunning) {
			v := j.view()
			m.mu.Unlock()
			// The copy written for this click is not going anywhere.
			if in.Path != path {
				_ = os.Remove(in.Path)
			}
			return v, nil
		}
	}

	choice := m.cfg.PostChoice()
	m.seq++
	job := &Job{
		ID:        fmt.Sprintf("j%d", m.seq),
		PR:        pr,
		Mine:      mine,
		Choice:    choice,
		Steps:     []jobStep{{Name: choice.Steps[0].Name, State: StateQueued}},
		publish:   in,
		pubChoice: choice,
		State:     StateQueued,
		QueuedAt:  time.Now(),
	}
	m.jobs[job.ID] = job
	m.order = append(m.order, job.ID)
	m.trimLocked()
	err = m.sendLocked(job)
	m.mu.Unlock()
	if err != nil {
		in.discard()
	}
	m.publish(job)
	return m.mustView(job.ID), err
}

// CommentSaved pastes onto the PR a review that is on disk, with no agent at
// all. It is the cheap path from the file to GitHub.
func (m *Manager) CommentSaved(ctx context.Context, pr gh.PR, body string, skip []int) error {
	body = store.DropFindings(body, skip)
	if strings.TrimSpace(body) == "" {
		return errors.New("that review has no body to publish")
	}
	err := gh.Comment(ctx, pr.Repo, pr.Number, store.CommentBody(agent.Result{PR: pr, Body: body}))
	if err == nil {
		_ = store.MarkPosted(m.reviewsDir, pr.Key())
	}
	return err
}

// Post publishes the review as a comment on the PR, without the findings in
// skip.
func (m *Manager) Post(ctx context.Context, id string, skip []int) (jobView, error) {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return jobView{}, fmt.Errorf("job %q does not exist", id)
	}
	if job.State != StateDone {
		m.mu.Unlock()
		return jobView{}, fmt.Errorf("that review has not finished")
	}
	if job.posting {
		m.mu.Unlock()
		return jobView{}, errPublishBusy
	}
	if job.Posted {
		m.mu.Unlock()
		return jobView{}, errAlreadyPosted
	}
	if !job.Choice.Publishable {
		m.mu.Unlock()
		return jobView{}, fmt.Errorf("what %q produces does not go to the PR", job.Choice.Name)
	}
	res := job.Result
	// Pasting as a comment is the same destination through another door: from
	// the pipeline only what is publishable goes, same as the post agent path.
	body, _, err := agent.PublishableBody(res, job.Choice)
	if err != nil {
		m.mu.Unlock()
		return jobView{}, err
	}
	res.Body = store.DropFindings(body, skip)
	// The lock is released around gh.Comment; posting keeps a second click
	// (here or in PublishWithAgent) out until this one is settled.
	job.posting = true
	m.mu.Unlock()

	err = gh.Comment(ctx, res.PR.Repo, res.PR.Number, store.CommentBody(res))

	if err == nil {
		_ = store.MarkPosted(m.reviewsDir, res.PR.Key())
	}

	m.mu.Lock()
	job.posting = false
	if err != nil {
		job.PostErr = err.Error()
	} else {
		job.Posted = true
		job.PostErr = ""
		job.AwaitsPublish = false
	}
	m.mu.Unlock()
	m.publish(job)
	return m.mustView(id), err
}

// Dismiss is the "keep it here" answer to the publish step: the card stops
// waiting on you and nothing goes to the PR.
func (m *Manager) Dismiss(id string) (jobView, error) {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return jobView{}, fmt.Errorf("job %q does not exist", id)
	}
	if job.posting {
		m.mu.Unlock()
		return jobView{}, errPublishBusy
	}
	if job.State != StateDone || !job.AwaitsPublish {
		m.mu.Unlock()
		return jobView{}, errors.New("that review is not waiting to be published")
	}
	job.AwaitsPublish = false
	m.mu.Unlock()
	m.publish(job)
	return m.mustView(id), nil
}

// View serializes a job. Workers touch jobs all the time, so no *Job leaves
// here — whoever reads gets a copy made under the lock.
//
// The body is rendered after the lock is released: markdown rendering and
// sanitizing a large report would otherwise stall every job operation.
func (m *Manager) View(id string, withBody bool) (jobView, bool) {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return jobView{}, false
	}
	v := job.view()
	var src bodySource
	if withBody {
		src = job.bodySource()
	}
	m.mu.Unlock()
	if withBody {
		v.renderBody(src)
	}
	return v, true
}

func (m *Manager) mustView(id string) jobView {
	v, _ := m.View(id, false)
	return v
}

// Snapshot returns the jobs from newest to oldest.
func (m *Manager) Snapshot() []jobView {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]jobView, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- {
		out = append(out, m.jobs[m.order[i]].view())
	}
	return out
}

// Limits is the last reading of the Claude quota, empty until the first review
// of this session — it only arrives in the stream of a running agent.
func (m *Manager) Limits() agent.Limits {
	m.limitsMu.RLock()
	defer m.limitsMu.RUnlock()
	return m.limits
}

// Active counts the reviews waiting or running.
func (m *Manager) Active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, id := range m.order {
		if s := m.jobs[id].State; s == StateQueued || s == StateRunning {
			n++
		}
	}
	return n
}

func (m *Manager) publish(job *Job) {
	m.mu.Lock()
	view := job.view()
	m.mu.Unlock()
	m.hub.Broadcast("job", view)
}
