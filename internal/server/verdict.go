package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/beroni/bazel/internal/gh"
	"github.com/beroni/bazel/internal/store"
)

// A decision is the person's, taken after reading a review: approve, approve
// with comments, or request changes. It lives in the review index and nowhere
// else — no gh call, no review on the PR. The summary turns the queue's
// decisions into the lines pasted into Slack.

// verdictView is a decision as the page shows it. Moved says the PR is at
// another head than the one decided on, as far as Bazel knows: the decision
// stays, and the page says it was taken before.
type verdictView struct {
	Verdict string    `json:"verdict"`
	Head    string    `json:"head,omitempty"`
	At      time.Time `json:"at"`
	Moved   bool      `json:"moved,omitempty"`
}

// verdictViews are the index's decisions, by lower-case PR key.
func (s *Server) verdictViews() map[string]verdictView {
	heads := s.knownHeads()
	out := map[string]verdictView{}
	for key, v := range store.LoadMarks(s.reviewsDir).Verdicts() {
		out[key] = s.verdictView(v, heads[key])
	}
	return out
}

func (s *Server) verdictView(v store.Verdict, head string) verdictView {
	return verdictView{
		Verdict: v.Verdict,
		Head:    v.Head,
		At:      v.At,
		Moved:   v.Head != "" && head != "" && !strings.EqualFold(v.Head, head),
	}
}

// knownHeads is the newest head Bazel knows for each PR, by lower-case key: the
// list read from GitHub, and for a PR outside it, the newest job's.
func (s *Server) knownHeads() map[string]string {
	heads := map[string]string{}
	for _, j := range s.jobs.Snapshot() { // newest first: the first one wins
		k := strings.ToLower(j.PR.Key)
		if _, ok := heads[k]; !ok && j.PR.Head != "" {
			heads[k] = j.PR.Head
		}
	}
	s.prMu.RLock()
	for _, pr := range s.prs {
		if pr.HeadRefOid != "" {
			heads[strings.ToLower(pr.Key())] = pr.HeadRefOid
		}
	}
	s.prMu.RUnlock()
	return heads
}

func (s *Server) handleVerdicts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"verdicts": s.verdictViews()})
}

// handleJobVerdict decides on the PR of a job whose review is on screen, on the
// head that review read.
func (s *Server) handleJobVerdict(w http.ResponseWriter, r *http.Request) {
	verdict, err := readVerdict(w, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	job, ok := s.jobs.View(r.PathValue("id"), false)
	switch {
	case !ok:
		writeErr(w, http.StatusNotFound, errors.New("review not found"))
		return
	case job.History || !job.HasBody:
		writeErr(w, http.StatusConflict, errors.New("there is no review to decide on yet"))
		return
	}
	s.decide(w, job.PR.Key, job.PR.Head, verdict)
}

// handleSavedVerdict decides on the PR of a saved review, on the head its file
// says it read.
func (s *Server) handleSavedVerdict(w http.ResponseWriter, r *http.Request) {
	verdict, err := readVerdict(w, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	file, err := store.Read(s.reviewsDir, r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	repo, number := store.PRFromTitle(store.Heading(file))
	if repo == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("could not tell which PR %q belongs to", r.PathValue("name")))
		return
	}
	s.decide(w, gh.PR{Repo: repo, Number: number}.Key(), store.HeadOf(file), verdict)
}

func (s *Server) decide(w http.ResponseWriter, key, head, verdict string) {
	v, err := store.SetVerdict(s.reviewsDir, key, verdict, head)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("could not record the decision: %w", err))
		return
	}
	all := s.verdictViews()
	// Every tab draws the same badge: the decision goes out to all of them.
	s.hub.Broadcast("verdicts", all)
	out := map[string]any{"key": key, "verdicts": all}
	if v != nil {
		out["verdict"] = all[strings.ToLower(key)]
	}
	writeJSON(w, http.StatusOK, out)
}

// readVerdict reads {"verdict": "approve" | "approve_with_comments" |
// "request_changes" | ""}; the empty one clears.
func readVerdict(w http.ResponseWriter, r *http.Request) (string, error) {
	var req struct {
		Verdict *string `json:"verdict"`
	}
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("invalid body: %w", err)
	}
	if req.Verdict == nil {
		return "", errors.New(`missing "verdict"`)
	}
	if !store.ValidVerdict(*req.Verdict) {
		return "", fmt.Errorf("unknown decision %q", *req.Verdict)
	}
	return *req.Verdict, nil
}

// summaryPR is a PR of the queue in the summary.
type summaryPR struct {
	Key     string `json:"key"`
	Number  int    `json:"number"`
	URL     string `json:"url"`
	Verdict string `json:"verdict,omitempty"`
	Moved   bool   `json:"moved,omitempty"`
}

// summary is what goes to Slack: one line per PR of the queue with a decision,
// in the order of the cards, each PR once; Undecided are the queue's PRs with
// none, which the text leaves out.
type summary struct {
	Text      string      `json:"text"`
	Decided   []summaryPR `json:"decided"`
	Undecided []summaryPR `json:"undecided"`
}

func (s *Server) summary() summary {
	verdicts := s.verdictViews()
	out := summary{Decided: []summaryPR{}, Undecided: []summaryPR{}}
	seen := map[string]bool{}
	var lines []string
	// Snapshot is the rail's order, newest card first. A history is not a
	// review: nobody decides on it.
	for _, j := range s.jobs.Snapshot() {
		k := strings.ToLower(j.PR.Key)
		if j.History || seen[k] {
			continue
		}
		seen[k] = true
		p := summaryPR{Key: j.PR.Key, Number: j.PR.Number, URL: prURL(j.PR)}
		v, ok := verdicts[k]
		if !ok {
			out.Undecided = append(out.Undecided, p)
			continue
		}
		p.Verdict, p.Moved = v.Verdict, v.Moved
		out.Decided = append(out.Decided, p)
		lines = append(lines, store.SlackLine(p.URL, v.Verdict))
	}
	out.Text = strings.Join(lines, "\n")
	return out
}

// prURL is the PR's page, from gh or, without one, from its key.
func prURL(pr prView) string {
	if pr.URL != "" {
		return pr.URL
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", pr.Repo, pr.Number)
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.summary())
}
