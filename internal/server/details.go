package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/beroni/bazel/internal/gh"
	"github.com/beroni/bazel/internal/store"
)

// detailsEntry is one PR's page as served, and the head sha it was read at.
type detailsEntry struct {
	sha  string
	at   time.Time
	view detailsView
}

// detailsTTL bounds the cache of a PR the list doesn't know (its head can't be
// compared), so it is read again after a while instead of never.
const detailsTTL = 2 * time.Minute

// detailsView is what the Overview draws: the PR's description, its timeline
// of commits, the files it touches, who reviews it, labels and checks.
type detailsView struct {
	Sha       string         `json:"sha"`
	CreatedAt time.Time      `json:"created_at"`
	BodyHTML  string         `json:"body_html,omitempty"`
	Commits   []commitView   `json:"commits"`
	Files     []fileView     `json:"files"`
	Reviewers []reviewerView `json:"reviewers"`
	Labels    []string       `json:"labels"`
	Checks    []checkView    `json:"checks"`
}

type commitView struct {
	Sha      string    `json:"sha"`
	Short    string    `json:"short"`
	Headline string    `json:"headline"`
	Author   string    `json:"author"`
	Date     time.Time `json:"date"`
}

type fileView struct {
	Path      string `json:"path"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// reviewerView is someone asked to review, or who did: state is "requested",
// "approved", "changes requested" or "commented".
type reviewerView struct {
	Login string `json:"login"`
	State string `json:"state"`
}

// checkView is one CI check: state is passed, failed, pending or skipped.
type checkView struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

func (s *Server) handlePRDetails(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("owner") + "/" + r.PathValue("name")
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number <= 0 {
		writeErr(w, http.StatusBadRequest, errors.New("invalid PR number"))
		return
	}
	if !s.watched(repo) {
		writeErr(w, http.StatusNotFound, errors.New("not a watched repository"))
		return
	}
	key := strings.ToLower(repo) + "#" + strconv.Itoa(number)
	head := s.knownHead(repo, number)

	s.detMu.Lock()
	e, ok := s.details[key]
	s.detMu.Unlock()
	if ok && (head != "" && e.sha == head || head == "" && time.Since(e.at) < detailsTTL) {
		writeJSON(w, http.StatusOK, e.view)
		return
	}
	d, err := s.fetchDetails(r.Context(), repo, number)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	v := detailsViewOf(d)
	s.detMu.Lock()
	s.details[key] = detailsEntry{sha: d.HeadRefOid, at: time.Now(), view: v}
	s.detMu.Unlock()
	writeJSON(w, http.StatusOK, v)
}

// watched says whether repo is one of the configured repositories: the page
// only asks about those, and nothing else should drive a gh call.
func (s *Server) watched(repo string) bool {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	for _, r := range s.cfg.Repos {
		if strings.EqualFold(r, repo) {
			return true
		}
	}
	return false
}

// knownHead is the head sha the PR list has for this PR, or "".
func (s *Server) knownHead(repo string, number int) string {
	s.prMu.RLock()
	defer s.prMu.RUnlock()
	for _, pr := range s.prs {
		if pr.Number == number && strings.EqualFold(pr.Repo, repo) {
			return pr.HeadRefOid
		}
	}
	return ""
}

func detailsViewOf(d gh.Details) detailsView {
	v := detailsView{Sha: d.HeadRefOid, CreatedAt: d.CreatedAt, Commits: []commitView{}, Files: []fileView{},
		Reviewers: []reviewerView{}, Labels: []string{}, Checks: []checkView{}}
	if strings.TrimSpace(d.Body) != "" {
		v.BodyHTML = renderMarkdown(d.Body)
	}
	for _, c := range d.Commits {
		cv := commitView{Sha: c.Oid, Short: c.Oid, Headline: c.MessageHeadline, Date: c.CommittedDate}
		if len(cv.Short) > 7 {
			cv.Short = cv.Short[:7]
		}
		if len(c.Authors) > 0 {
			cv.Author = c.Authors[0].Login
			if cv.Author == "" {
				cv.Author = c.Authors[0].Name
			}
		}
		v.Commits = append(v.Commits, cv)
	}
	for _, f := range d.Files {
		v.Files = append(v.Files, fileView{Path: f.Path, Additions: f.Additions, Deletions: f.Deletions})
	}
	seen := map[string]bool{}
	for _, rv := range d.LatestReviews {
		if rv.Author.Login == "" {
			continue
		}
		seen[rv.Author.Login] = true
		v.Reviewers = append(v.Reviewers, reviewerView{Login: rv.Author.Login, State: reviewState(rv.State)})
	}
	for _, rq := range d.ReviewRequests {
		who := rq.Login
		if who == "" {
			who = rq.Slug
		}
		if who == "" {
			who = rq.Name
		}
		if who != "" && !seen[who] {
			v.Reviewers = append(v.Reviewers, reviewerView{Login: who, State: "requested"})
		}
	}
	for _, l := range d.Labels {
		v.Labels = append(v.Labels, l.Name)
	}
	for _, c := range d.StatusCheckRollup {
		name := c.Name
		if name == "" {
			name = c.Context
		}
		v.Checks = append(v.Checks, checkView{Name: name, State: checkState(c.Status, c.Conclusion, c.State)})
	}
	return v
}

func reviewState(s string) string {
	switch strings.ToUpper(s) {
	case "APPROVED":
		return "approved"
	case "CHANGES_REQUESTED":
		return "changes requested"
	case "DISMISSED":
		return "dismissed"
	}
	return "commented"
}

// checkState folds a CheckRun (status + conclusion) or a StatusContext
// (state) into passed, failed, pending or skipped.
func checkState(status, conclusion, state string) string {
	switch strings.ToUpper(conclusion + state) {
	case "SUCCESS":
		return "passed"
	case "FAILURE", "ERROR", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE":
		return "failed"
	case "SKIPPED", "NEUTRAL", "STALE":
		return "skipped"
	}
	if strings.ToUpper(status) == "COMPLETED" && conclusion == "" && state == "" {
		return "passed"
	}
	return "pending"
}

// --- the History tab ---

// historyView is a PR's history as the History tab draws it, with the
// markdown parts already rendered and sanitized.
type historyView struct {
	Found       bool           `json:"found"`
	Stale       bool           `json:"stale,omitempty"`
	CommitsRead int            `json:"commits_read,omitempty"`
	Agent       string         `json:"agent"`
	WhatHTML    string         `json:"what_html,omitempty"`
	Decisions   []decisionView `json:"decisions"`
	StillOpen   []string       `json:"still_open_html"`
	RestHTML    string         `json:"rest_html,omitempty"`
	Job         *jobView       `json:"job,omitempty"`
}

type decisionView struct {
	N        int      `json:"n"`
	Title    string   `json:"title"`
	Sha      string   `json:"sha,omitempty"`
	Source   string   `json:"source,omitempty"`
	BodyHTML string   `json:"body_html,omitempty"`
	Rejected []string `json:"rejected"`
}

// prOf finds a watched PR by repo and number: from the list when it is
// there, from gh otherwise.
func (s *Server) prOf(r *http.Request) (gh.PR, int, error) {
	repo := r.PathValue("owner") + "/" + r.PathValue("name")
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number <= 0 {
		return gh.PR{}, http.StatusBadRequest, errors.New("invalid PR number")
	}
	if !s.watched(repo) {
		return gh.PR{}, http.StatusNotFound, errors.New("not a watched repository")
	}
	s.prMu.RLock()
	for _, pr := range s.prs {
		if pr.Number == number && strings.EqualFold(pr.Repo, repo) {
			s.prMu.RUnlock()
			return pr, 0, nil
		}
	}
	s.prMu.RUnlock()
	pr, err := gh.Get(r.Context(), repo, number)
	if err != nil {
		return gh.PR{}, http.StatusBadGateway, err
	}
	return pr, 0, nil
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	pr, code, err := s.prOf(r)
	if err != nil {
		writeErr(w, code, err)
		return
	}
	v := historyView{Agent: s.historyAgentName(), Decisions: []decisionView{}, StillOpen: []string{}}
	if j, ok := s.jobs.HistoryJob(pr.Key()); ok {
		v.Job = &j
	}
	md, stale, found := store.LoadHistory(s.reviewsDir, pr.Repo, pr.Number, pr.HeadRefOid)
	if found {
		h := store.ParseHistory(md)
		v.Found, v.Stale, v.CommitsRead = true, stale, h.CommitsRead
		if h.WhatItDoes != "" {
			v.WhatHTML = renderMarkdown(h.WhatItDoes)
		}
		for _, d := range h.Decisions {
			dv := decisionView{N: d.N, Title: d.Title, Sha: d.Sha, Source: d.Source, Rejected: d.Rejected}
			if d.Body != "" {
				dv.BodyHTML = renderMarkdown(d.Body)
			}
			v.Decisions = append(v.Decisions, dv)
		}
		for _, q := range h.StillOpen {
			v.StillOpen = append(v.StillOpen, renderMarkdown(q))
		}
		if h.Rest != "" {
			v.RestHTML = renderMarkdown(h.Rest)
		}
	}
	writeJSON(w, http.StatusOK, v)
}

// handleHistoryGenerate runs the history agent over the PR, as a job like
// any other: it shows in the queue with its live log, and its result lands in
// the History tab.
func (s *Server) handleHistoryGenerate(w http.ResponseWriter, r *http.Request) {
	pr, code, err := s.prOf(r)
	if err != nil {
		writeErr(w, code, err)
		return
	}
	view, err := s.jobs.EnqueueHistory(pr, strings.EqualFold(pr.Author.Login, s.me))
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, view)
}

func (s *Server) historyAgentName() string {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	ch := s.cfg.HistoryChoice()
	if len(ch.Steps) > 0 && ch.Steps[0].Task != "" {
		return strings.Fields(ch.Steps[0].Task)[0]
	}
	return ch.Name
}
