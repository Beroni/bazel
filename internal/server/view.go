package server

import (
	"time"

	"github.com/beroni/bazel/internal/agent"
	"github.com/beroni/bazel/internal/config"
	"github.com/beroni/bazel/internal/gh"
	"github.com/beroni/bazel/internal/store"
)

// prView is the PR as the browser needs it.
//
// gh.PR marks Repo as `json:"-"` — it is filled from the repository queried,
// it does not come from gh — so serializing the struct directly would lose
// precisely the field that identifies the PR. Hence this view.
type prView struct {
	Key          string    `json:"key"`
	Repo         string    `json:"repo"`
	Slug         string    `json:"slug"`
	Number       int       `json:"number"`
	Title        string    `json:"title"`
	URL          string    `json:"url"`
	Author       string    `json:"author"`
	Draft        bool      `json:"draft"`
	Additions    int       `json:"additions"`
	Deletions    int       `json:"deletions"`
	ChangedFiles int       `json:"changed_files"`
	UpdatedAt    time.Time `json:"updated_at"`
	Age          string    `json:"age"`
	Branch       string    `json:"branch"`
	Base         string    `json:"base"`
	Decision     string    `json:"review_decision"`
	Mine         bool      `json:"mine"`
	// BodyHTML is the PR description already converted and sanitized. It is
	// only filled in the listing — in job events it would be a pointless
	// render on every state change.
	BodyHTML string `json:"body_html,omitempty"`

	// Reviewed and friends are the history: has this PR been through a review?
	// Has it changed since then? Did the review go to GitHub?
	Reviewed     bool   `json:"reviewed,omitempty"`
	ReviewedAt   string `json:"reviewed_at,omitempty"`
	ReviewAgent  string `json:"review_agent,omitempty"`
	ReviewPosted bool   `json:"review_posted,omitempty"`
	Changed      bool   `json:"changed_since_review,omitempty"`
}

// withStatus stamps onto the PR what the review index knows about it.
func (v prView) withStatus(st store.Status, now time.Time) prView {
	if !st.Reviewed {
		return v
	}
	v.Reviewed = true
	v.ReviewedAt = st.Age(now)
	v.ReviewAgent = st.Agent
	v.ReviewPosted = st.Posted
	v.Changed = st.Changed
	return v
}

func newPRView(pr gh.PR, mine bool, now time.Time) prView {
	return prView{
		Key:          pr.Key(),
		Repo:         pr.Repo,
		Slug:         pr.Slug(),
		Number:       pr.Number,
		Title:        pr.Title,
		URL:          pr.URL,
		Author:       pr.Author.Login,
		Draft:        pr.IsDraft,
		Additions:    pr.Additions,
		Deletions:    pr.Deletions,
		ChangedFiles: pr.ChangedFiles,
		UpdatedAt:    pr.UpdatedAt,
		Age:          pr.Age(now),
		Branch:       pr.HeadRefName,
		Base:         pr.BaseRefName,
		Decision:     pr.ReviewDecision,
		Mine:         mine,
	}
}

// stepView is a serialized review step. started_at goes along so the page can
// count the time of the running step without depending on a new event every
// second.
type stepView struct {
	Name      string     `json:"name"`
	State     State      `json:"state"`
	Seconds   int        `json:"seconds"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	Err       string     `json:"error,omitempty"`
}

// jobView is a serializable job. The review body only goes along when the
// client asks for a specific job — in the list it would just take up
// bandwidth.
type jobView struct {
	ID       string `json:"id"`
	PR       prView `json:"pr"`
	Agent    string `json:"agent,omitempty"`
	Pipeline bool   `json:"pipeline,omitempty"`
	Posts    bool   `json:"posts,omitempty"`
	// Publishable says whether what this job produced can go to the PR. Always
	// sent, no omitempty: the false is exactly what the page needs to read to
	// hide the publish buttons.
	Publishable bool `json:"publishable"`
	// AwaitsPublish marks the pipeline that ended in a `publish` step and
	// waits for you to choose what goes to the PR.
	AwaitsPublish bool `json:"awaits_publish,omitempty"`
	// History is a job writing the PR's history for the History tab.
	History bool `json:"history,omitempty"`
	// Publishing marks the job that is carrying an already-read review to the
	// PR — the review's own card while the post agent runs, or the card of a
	// publication coming from disk.
	Publishing bool       `json:"publishing,omitempty"`
	LogLines   int        `json:"log_lines"`
	Steps      []stepView `json:"steps,omitempty"`
	Cloning    bool       `json:"cloning,omitempty"`
	State      State      `json:"state"`
	QueuedAt   time.Time  `json:"queued_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Seconds    int        `json:"seconds"`
	Err        string     `json:"error,omitempty"`
	SavedTo    string     `json:"saved_to,omitempty"`
	Workdir    string     `json:"workdir,omitempty"`
	Truncated  bool       `json:"truncated,omitempty"`
	// Tokens and Cost are the review's spend. While it runs they are the
	// partial tally, and Partial tells the page so: missing is what the lenses
	// spent as sub-agents, which only comes in at the close. An agent that
	// does not speak stream-json reports no spend at all and stays at zero.
	Tokens  int     `json:"tokens,omitempty"`
	Cost    float64 `json:"cost_usd,omitempty"`
	Partial bool    `json:"tokens_partial,omitempty"`
	Posted  bool    `json:"posted"`
	PostErr string  `json:"post_error,omitempty"`
	HasBody bool    `json:"has_body"`
	Body    string  `json:"body,omitempty"`
	HTML    string  `json:"html,omitempty"`
	// PublishHTML and ContextHTML split the report when only some of the steps
	// go to the PR: the first is exactly what goes up — it is where the
	// finding checkboxes live, in the same numbering the server uses to remove
	// them — and the second is what stays in Bazel. Empty when the whole
	// report is publishable, or none of it is.
	PublishHTML string `json:"publish_html,omitempty"`
	ContextHTML string `json:"context_html,omitempty"`
}

// view serializes the job, without the body. Call with the Manager lock held.
func (j *Job) view() jobView {
	v := jobView{
		ID:          j.ID,
		PR:          newPRView(j.PR, j.Mine, time.Now()),
		Agent:       j.Choice.Name,
		Pipeline:    j.Choice.Pipeline,
		Posts:       j.Choice.Posts,
		Publishable: j.Choice.Publishable,
		LogLines:    j.logSeq,
		Steps:       j.stepViews(),
		Cloning:     j.Cloning,
		State:       j.State,
		QueuedAt:    j.QueuedAt,
		Seconds:     int(j.duration().Seconds()),
		Err:         j.Err,
		SavedTo:     j.SavedTo,
		Workdir:     j.Result.Workdir,
		Truncated:   j.Result.Truncated,
		Posted:      j.Posted,
		PostErr:     j.PostErr,
		HasBody:     j.Result.Body != "",
	}
	// Once finished, the closed spend of the result rules; before that, the
	// partial the agent keeps reporting. Publishing inside the review's card,
	// both count: the review's closed spend plus what the post agent has
	// already burned.
	switch fechado := j.Result.Usage; {
	case !fechado.Empty() && j.publish != nil && !j.Live.Empty():
		soma := fechado.Plus(j.Live)
		v.Tokens, v.Cost, v.Partial = soma.Total(), soma.CostUSD, true
	case !fechado.Empty():
		v.Tokens, v.Cost = fechado.Total(), fechado.CostUSD
	case !j.Live.Empty():
		v.Tokens, v.Cost, v.Partial = j.Live.Total(), j.Live.CostUSD, true
	}
	if j.publish != nil {
		v.Publishing = true
	}
	v.AwaitsPublish = j.AwaitsPublish
	v.History = j.History
	if !j.StartedAt.IsZero() {
		t := j.StartedAt
		v.StartedAt = &t
	}
	if !j.FinishedAt.IsZero() {
		t := j.FinishedAt
		v.FinishedAt = &t
	}
	return v
}

// bodySource is what renderBody needs from a job, copied under the lock so
// the rendering can run without it.
type bodySource struct {
	res    agent.Result
	choice config.Choice
}

// bodySource copies the result and the choice. Call with the Manager lock held.
func (j *Job) bodySource() bodySource {
	res := j.Result
	res.Steps = append([]agent.StepResult(nil), j.Result.Steps...)
	return bodySource{res: res, choice: j.Choice}
}

// renderBody fills the body fields of v. Runs without the Manager lock.
func (v *jobView) renderBody(src bodySource) {
	if src.res.Body == "" {
		return
	}
	v.Body = src.res.Body
	v.HTML = renderReview(src.res.Body)
	if src.choice.Publishable {
		if pub, trimmed, err := agent.PublishableBody(src.res, src.choice); err == nil && trimmed {
			v.PublishHTML = renderReview(pub)
			v.ContextHTML = renderReview(agent.ContextBody(src.res, src.choice))
		}
	}
}

// stepViews serializes the steps. Call with the Manager lock held.
func (j *Job) stepViews() []stepView {
	if len(j.Steps) == 0 {
		return nil
	}
	out := make([]stepView, 0, len(j.Steps))
	for _, st := range j.Steps {
		v := stepView{Name: st.Name, State: st.State, Err: st.Err}
		switch {
		case st.State == StateRunning && !st.StartedAt.IsZero():
			t := st.StartedAt
			v.StartedAt = &t
			v.Seconds = int(time.Since(st.StartedAt).Seconds())
		default:
			v.Seconds = int(st.Duration.Seconds())
		}
		out = append(out, v)
	}
	return out
}
