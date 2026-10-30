package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/beroni/bazel/internal/gh"
	"github.com/beroni/bazel/internal/store"
)

// detailsEntry is one PR's page as served, the head sha it was read at, and
// the error when the read failed.
type detailsEntry struct {
	sha  string
	at   time.Time
	view detailsView
	err  string
}

// The details cache. Checks and reviews change without a new commit, so even
// at the same head an entry is read again after detailsTTL. A failed read is
// not retried before detailsErrTTL, unless someone clicks Retry (retry=1):
// that one goes to GitHub, at most once every detailsRetryGap, so a held-down
// click or a script cannot turn it into a loop. Whatever reaches GitHub from
// here, or from a forced refresh of the list, also counts against
// ghReadsPerMin, because with GitHub failing is when more calls hurt most: past
// it the last good page is served, or a 429. At most detailsMax entries
// are kept, the oldest going first. A PR that is not in the list is only read
// when the page asks for it explicitly (offlist=1), and at most offListPerMin
// times a minute: the list bounds what the page needs, and nothing else
// should be able to spend the machine's GitHub quota.
var (
	detailsTTL    = time.Minute
	detailsErrTTL = 30 * time.Second
	detailsMax    = 256
	offListPerMin = 6
	ghReadsPerMin = 60
	timeNow       = time.Now

	detailsRetryGap    = 5 * time.Second
	detailsReadTimeout = time.Minute
)

func (s *Server) handlePRDetails(w http.ResponseWriter, r *http.Request) {
	repo, number, code, err := s.watchedRef(r)
	if err != nil {
		writeErr(w, code, err)
		return
	}
	view, code, err := s.prDetails(r, repo, number)
	if err != nil {
		writeErr(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// detailsRead is a read of one PR under way: whoever asks for that PR
// meanwhile waits for it instead of starting another gh.
type detailsRead struct {
	done  chan struct{}
	entry detailsEntry
}

// answer is the entry as a handler returns it.
func (e detailsEntry) answer() (detailsView, int, error) {
	if e.err != "" {
		return detailsView{}, http.StatusBadGateway, errors.New(e.err)
	}
	return e.view, http.StatusOK, nil
}

// prDetails is one PR's page, from the cache or from GitHub, under the limits
// described above; on a refusal the int is the status to answer with.
func (s *Server) prDetails(r *http.Request, repo string, number int) (detailsView, int, error) {
	pr, listed := s.listedPR(repo, number)
	if !listed && r.URL.Query().Get("offlist") != "1" {
		return detailsView{}, http.StatusNotFound, errors.New("that PR is not in the list; ask with offlist=1 to read it from GitHub")
	}
	key := strings.ToLower(repo) + "#" + strconv.Itoa(number)
	now := timeNow()

	s.detMu.Lock()
	e, ok := s.details[key]
	retry := r.URL.Query().Get("retry") == "1"
	if ok && (!listed || e.sha == pr.HeadRefOid || e.err != "") {
		failed := e.err != "" && now.Sub(e.at) < detailsErrTTL && !retry
		if failed || e.err == "" && now.Sub(e.at) < detailsTTL {
			s.detMu.Unlock()
			return e.answer()
		}
	}
	if read, ok := s.reading[key]; ok {
		s.detMu.Unlock()
		select {
		case <-read.done:
			return read.entry.answer()
		case <-r.Context().Done():
			return detailsView{}, http.StatusBadGateway, r.Context().Err()
		}
	}
	if wait := detailsRetryGap - now.Sub(s.retried[key]); retry && wait > 0 {
		s.detMu.Unlock()
		return detailsView{}, http.StatusTooManyRequests, fmt.Errorf("just retried — try again in %ds", (wait+time.Second-1)/time.Second)
	}
	if err := s.ghReadLocked(now, !listed); err != nil {
		s.detMu.Unlock()
		if ok && e.err == "" {
			return e.answer()
		}
		return detailsView{}, http.StatusTooManyRequests, err
	}
	if retry {
		s.noteRetryLocked(key, now)
	}
	read := &detailsRead{done: make(chan struct{}), entry: detailsEntry{err: "the read of this PR failed"}}
	s.reading[key] = read
	s.detMu.Unlock()
	defer func() {
		s.detMu.Lock()
		delete(s.reading, key)
		s.detMu.Unlock()
		close(read.done)
	}()

	// Others may be waiting on this read, and a tab that closes does not make
	// its answer any less useful: it runs to its own timeout.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), detailsReadTimeout)
	defer cancel()
	d, err := s.fetchDetails(ctx, repo, number)
	// The sha is the list's, the one this entry is compared with: GitHub's head
	// is newer when the list is behind, and then no read would ever match.
	// The time is the end of the read: a gh that took 30s to fail must not
	// come back already expired.
	read.entry = detailsEntry{sha: pr.HeadRefOid, at: timeNow()}
	if err != nil {
		read.entry.err = err.Error()
	} else {
		read.entry.view = detailsViewOf(d)
	}
	// A canceled read says nothing about the PR.
	if !errors.Is(err, context.Canceled) {
		s.detMu.Lock()
		s.putDetails(key, read.entry)
		s.detMu.Unlock()
	}
	return read.entry.answer()
}

// ghReadLocked takes one call to GitHub from this minute's budget, and one
// from the budget of reads outside the list when it is one, or says which is
// spent. Call with detMu held.
func (s *Server) ghReadLocked(now time.Time, offList bool) error {
	s.ghReads = lastMinute(s.ghReads, now)
	s.offList = lastMinute(s.offList, now)
	if len(s.ghReads) >= ghReadsPerMin {
		return errors.New("too many reads from GitHub; wait a minute")
	}
	if offList {
		if len(s.offList) >= offListPerMin {
			return errors.New("too many reads of PRs outside the list; wait a minute")
		}
		s.offList = append(s.offList, now)
	}
	s.ghReads = append(s.ghReads, now)
	return nil
}

// lastMinute drops the times older than a minute.
func lastMinute(times []time.Time, now time.Time) []time.Time {
	recent := times[:0]
	for _, t := range times {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	return recent
}

// noteRetryLocked marks the Retry of a PR. It lives apart from the cache so
// that evicting the entry does not reset the gap, and only what is within the
// gap is kept. Call with detMu held. The token is taken first: a refused read
// costs no wait.
func (s *Server) noteRetryLocked(key string, now time.Time) {
	for k, t := range s.retried {
		if now.Sub(t) >= detailsRetryGap {
			delete(s.retried, k)
		}
	}
	s.retried[key] = now
}

// putDetails stores an entry, dropping the oldest when the cache is full.
// Call with detMu held.
func (s *Server) putDetails(key string, e detailsEntry) {
	delete(s.details, key)
	for len(s.details) >= detailsMax {
		oldest, at := "", time.Time{}
		for k, v := range s.details {
			if oldest == "" || v.at.Before(at) {
				oldest, at = k, v.at
			}
		}
		delete(s.details, oldest)
	}
	s.details[key] = e
}

// watchedRef reads {owner}/{name}/{number} from the path and returns the
// repository with the config's spelling — so ACME/api and acme/api are one
// PR — or why the request is refused.
func (s *Server) watchedRef(r *http.Request) (string, int, int, error) {
	repo := r.PathValue("owner") + "/" + r.PathValue("name")
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number <= 0 {
		return "", 0, http.StatusBadRequest, errors.New("invalid PR number")
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	for _, w := range s.cfg.Repos {
		if strings.EqualFold(w, repo) {
			return w, number, 0, nil
		}
	}
	return "", 0, http.StatusNotFound, errors.New("not a watched repository")
}

// listedPR is the PR as the current list has it, if it is there.
func (s *Server) listedPR(repo string, number int) (gh.PR, bool) {
	s.prMu.RLock()
	defer s.prMu.RUnlock()
	for _, pr := range s.prs {
		if pr.Number == number && strings.EqualFold(pr.Repo, repo) {
			return pr, true
		}
	}
	return gh.PR{}, false
}

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
	Found       bool   `json:"found"`
	Stale       bool   `json:"stale,omitempty"`
	CommitsRead int    `json:"commits_read,omitempty"`
	Agent       string `json:"agent"`
	// Custom says the agent is a history_agent the user wrote.
	Custom    bool           `json:"custom,omitempty"`
	WhatHTML  string         `json:"what_html,omitempty"`
	Decisions []decisionView `json:"decisions"`
	StillOpen []string       `json:"still_open_html"`
	OpenNote  string         `json:"still_open_note_html,omitempty"`
	RestHTML  string         `json:"rest_html,omitempty"`
	Job       *jobView       `json:"job,omitempty"`
}

type decisionView struct {
	N        int      `json:"n"`
	Title    string   `json:"title"`
	Sha      string   `json:"sha,omitempty"`
	Shas     []string `json:"shas,omitempty"`
	Source   string   `json:"source,omitempty"`
	BodyHTML string   `json:"body_html,omitempty"`
	Rejected []string `json:"rejected"`
}

// prOf finds a watched PR by repo and number, with the config's spelling of
// the repository: from the list when it is there, from gh otherwise. The gh
// call counts against the reads of PRs outside the list.
func (s *Server) prOf(r *http.Request) (gh.PR, int, error) {
	repo, number, code, err := s.watchedRef(r)
	if err != nil {
		return gh.PR{}, code, err
	}
	if pr, ok := s.listedPR(repo, number); ok {
		return pr, 0, nil
	}
	s.detMu.Lock()
	err = s.ghReadLocked(timeNow(), true)
	s.detMu.Unlock()
	if err != nil {
		return gh.PR{}, http.StatusTooManyRequests, err
	}
	pr, err := gh.Get(r.Context(), repo, number)
	if err != nil {
		return gh.PR{}, http.StatusBadGateway, err
	}
	pr.Repo = repo
	return pr, 0, nil
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	repo, number, code, err := s.watchedRef(r)
	if err != nil {
		writeErr(w, code, err)
		return
	}
	pr, listed := s.listedPR(repo, number)
	// Outside the list all this needs from GitHub is the head, and the page's
	// own read brings it, under its cache and its limits.
	if !listed {
		view, code, err := s.prDetails(r, repo, number)
		if err != nil {
			writeErr(w, code, err)
			return
		}
		pr = gh.PR{Repo: repo, Number: number, HeadRefOid: view.Sha}
	}
	s.cfgMu.Lock()
	custom := s.cfg.HistoryCustom()
	s.cfgMu.Unlock()
	v := historyView{Agent: s.historyAgentName(), Custom: custom, Decisions: []decisionView{}, StillOpen: []string{}}
	if j, ok := s.jobs.HistoryJob(pr.Key()); ok {
		v.Job = &j
	}
	md, stale, found := store.LoadHistory(s.reviewsDir, pr.Repo, pr.Number, pr.HeadRefOid)
	if found {
		h := store.ParseHistory(md)
		// The history is an agent's output, as a review is: its raw HTML shows
		// as text rather than being sanitized away out of sight.
		v.Found, v.Stale, v.CommitsRead = true, stale, h.CommitsRead
		if h.WhatItDoes != "" {
			v.WhatHTML = renderReviewMarkdown(h.WhatItDoes)
		}
		for _, d := range h.Decisions {
			dv := decisionView{N: d.N, Title: d.Title, Sha: d.Sha, Shas: d.Shas, Source: d.Source, Rejected: d.Rejected}
			if d.Body != "" {
				dv.BodyHTML = renderReviewMarkdown(d.Body)
			}
			v.Decisions = append(v.Decisions, dv)
		}
		for _, q := range h.StillOpen {
			v.StillOpen = append(v.StillOpen, renderReviewMarkdown(q))
		}
		if h.StillOpenNote != "" {
			v.OpenNote = renderReviewMarkdown(h.StillOpenNote)
		}
		if h.Rest != "" {
			v.RestHTML = renderReviewMarkdown(h.Rest)
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
