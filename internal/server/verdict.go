package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
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
	// Source is "review" or "manual"; Current is the head Bazel knows the PR
	// at now, which a decision taken by hand goes on.
	Source  string `json:"source,omitempty"`
	Current string `json:"current,omitempty"`
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
		Source:  v.Source,
		Current: head,
	}
}

// knownHeads is the newest head Bazel knows for each PR, by lower-case key and
// by time: the list read from GitHub, from when that read began, against the
// newest review, from when its checkout was done. A list older than the
// clone knows less than the clone.
func (s *Server) knownHeads() map[string]string {
	type seen struct {
		head string
		at   time.Time
	}
	newest := map[string]seen{}
	for k, r := range s.jobs.reviewedHeads() {
		newest[k] = seen{r.head, r.at}
	}
	s.prMu.RLock()
	for _, pr := range s.prs {
		k := strings.ToLower(pr.Key())
		at := s.prsReadAt[k]
		if r, ok := newest[k]; pr.HeadRefOid != "" && (!ok || !at.Before(r.at)) {
			newest[k] = seen{pr.HeadRefOid, at}
		}
	}
	s.prMu.RUnlock()
	heads := make(map[string]string, len(newest))
	for k, v := range newest {
		heads[k] = v.head
	}
	return heads
}

type reviewedHead struct {
	head string
	at   time.Time
}

// reviewedHeads is, for each PR, the head its newest review read and when:
// the start of its first step, which comes right after the checkout, or the
// end of the job when no step ran.
func (m *Manager) reviewedHeads() map[string]reviewedHead {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]reviewedHead{}
	for _, j := range m.jobs {
		head := j.Result.PR.HeadRefOid
		if j.History || head == "" || !strings.EqualFold(j.Result.PR.Key(), j.PR.Key()) {
			continue
		}
		at := j.FinishedAt
		if len(j.Steps) > 0 && !j.Steps[0].StartedAt.IsZero() {
			at = j.Steps[0].StartedAt
		}
		k := strings.ToLower(j.PR.Key())
		if r, ok := out[k]; !ok || at.After(r.at) {
			out[k] = reviewedHead{head, at}
		}
	}
	return out
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
	s.decide(w, job.PR.Key, job.PR.Head, verdict, store.SourceReview)
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
	s.decide(w, gh.PR{Repo: repo, Number: number}.Key(), store.HeadOf(file), verdict, store.SourceReview)
}

// handlePRVerdict decides by hand on a PR of a watched repository, on the head
// the list read from GitHub says it is at: after a push, with no new review,
// or with no review in Bazel at all. The head is the server's, never the
// page's; without one in the list, there is nothing safe to decide on.
func (s *Server) handlePRVerdict(w http.ResponseWriter, r *http.Request) {
	verdict, err := readVerdict(w, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	repo, number, code, err := s.watchedRef(r)
	if err != nil {
		writeErr(w, code, err)
		return
	}
	// The newest head known, by time: the list's, unless a review read a newer
	// one after that list was read.
	key := gh.PR{Repo: repo, Number: number}.Key()
	head := s.knownHeads()[strings.ToLower(key)]
	if head == "" {
		writeErr(w, http.StatusConflict, errors.New("refresh the PR list first: Bazel does not know the head this PR is at"))
		return
	}
	if pr, ok := s.listedPR(repo, number); ok {
		key = pr.Key()
	}
	s.decide(w, key, head, verdict, store.SourceManual)
}

func (s *Server) decide(w http.ResponseWriter, key, head, verdict, source string) {
	v, err := store.SetVerdict(s.reviewsDir, key, verdict, head, source)
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
	Key       string   `json:"key"`
	Repo      string   `json:"repo"`
	Number    int      `json:"number"`
	Author    string   `json:"author,omitempty"`
	URL       string   `json:"url"`
	Verdict   string   `json:"verdict,omitempty"`
	Moved     bool     `json:"moved,omitempty"`
	Reviewers []string `json:"reviewers"`
}

// summary is what goes to Slack: one line per PR of the queue with a decision,
// each PR once, with who reviews it, grouped under its author. Undecided are
// the queue's PRs with none, and Moved the decided ones that moved past the
// head of the decision: the text leaves both out, since a line there would
// vouch for commits nobody read. ReviewersError says the names could not be
// read: the lines go without them.
type summary struct {
	Text           string      `json:"text"`
	Decided        []summaryPR `json:"decided"`
	Undecided      []summaryPR `json:"undecided"`
	Moved          []summaryPR `json:"moved"`
	ReviewersError string      `json:"reviewers_error,omitempty"`
}

func (s *Server) summary(ctx context.Context) summary {
	verdicts := s.verdictViews()
	out := summary{Decided: []summaryPR{}, Undecided: []summaryPR{}, Moved: []summaryPR{}}
	seen := map[string]bool{}
	var keys []string
	// Snapshot is the rail's order, newest card first. A history is not a
	// review: nobody decides on it.
	for _, j := range s.jobs.Snapshot() {
		k := strings.ToLower(j.PR.Key)
		if j.History || seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, j.PR.Key)
		p := summaryPR{Key: j.PR.Key, Repo: j.PR.Repo, Number: j.PR.Number, Author: j.PR.Author, URL: prURL(j.PR), Reviewers: []string{}}
		if v, ok := verdicts[k]; ok {
			p.Verdict, p.Moved = v.Verdict, v.Moved
			out.Decided = append(out.Decided, p)
		} else {
			out.Undecided = append(out.Undecided, p)
		}
	}
	names, err := s.reviewersOf(ctx, keys)
	if err != nil {
		out.ReviewersError = "reviewers could not be read: " + err.Error()
	}
	lines := make([]string, 0, len(out.Decided))
	for _, list := range [][]summaryPR{out.Decided, out.Undecided} {
		for i := range list {
			if n := names[strings.ToLower(list[i].Key)]; n != nil {
				list[i].Reviewers = n
			}
		}
	}
	// The text goes by the PR's author, A to Z with "unknown" last, and within
	// an author by repository and number; the lists apart by repository and
	// number. The API answers in the same order.
	sortPRs(out.Decided, true)
	sortPRs(out.Undecided, false)
	var groups []string
	for _, p := range out.Decided {
		if p.Moved {
			out.Moved = append(out.Moved, p)
			continue
		}
		line := store.SlackLine(p.URL, p.Verdict)
		// Logins with no @: pasted on GitHub, the text mentions nobody.
		if len(p.Reviewers) > 0 {
			line += " — " + strings.Join(p.Reviewers, ", ")
		}
		// A login is one person whatever its case: one group, headed by the
		// spelling of its first PR.
		if author := authorOf(p); len(groups) == 0 || !strings.EqualFold(groups[len(groups)-1], author) {
			if len(lines) > 0 {
				lines = append(lines, "")
			}
			groups = append(groups, author)
			lines = append(lines, author)
		}
		lines = append(lines, line)
	}
	sortPRs(out.Moved, false)
	out.Text = strings.Join(lines, "\n")
	return out
}

// unknownAuthor heads the group of PRs whose author Bazel does not know.
const unknownAuthor = "unknown"

func authorOf(p summaryPR) string {
	if p.Author == "" {
		return unknownAuthor
	}
	return p.Author
}

// sortPRs orders the summary's PRs by repository, then number; byAuthor puts
// the author first, case aside, with the unknown ones last.
func sortPRs(list []summaryPR, byAuthor bool) {
	slices.SortStableFunc(list, func(a, b summaryPR) int {
		if byAuthor {
			if ua, ub := a.Author == "", b.Author == ""; ua != ub {
				if ua {
					return 1
				}
				return -1
			}
			if c := strings.Compare(strings.ToLower(a.Author), strings.ToLower(b.Author)); c != 0 {
				return c
			}
		}
		if c := strings.Compare(strings.ToLower(a.Repo), strings.ToLower(b.Repo)); c != 0 {
			return c
		}
		return cmp.Compare(a.Number, b.Number)
	})
}

// reviewersTTL is how long a read of who reviews a PR serves, as the Needs
// you page does: the summary is opened, edited, opened again.
const reviewersTTL = time.Minute

type reviewersEntry struct {
	names []string
	at    time.Time
}

// reviewersOf is who reviews each PR, by lower-case key: what was read in the
// last minute, and one gh read for the rest. A failed read gives no names.
func (s *Server) reviewersOf(ctx context.Context, keys []string) (map[string][]string, error) {
	out := map[string][]string{}
	var missing []string
	s.revMu.Lock()
	for _, key := range keys {
		k := strings.ToLower(key)
		if e, ok := s.reviewers[k]; ok && time.Since(e.at) < reviewersTTL {
			out[k] = e.names
		} else {
			missing = append(missing, key)
		}
	}
	s.revMu.Unlock()
	if len(missing) == 0 {
		return out, nil
	}
	got, err := s.fetchReviewers(ctx, missing, s.me)
	if err != nil {
		return map[string][]string{}, err
	}
	now := time.Now()
	s.revMu.Lock()
	for key, names := range got {
		k := strings.ToLower(key)
		s.reviewers[k] = reviewersEntry{names: names, at: now}
		out[k] = names
	}
	// The cache only keeps what a summary may still ask for.
	for k, e := range s.reviewers {
		if time.Since(e.at) >= reviewersTTL {
			delete(s.reviewers, k)
		}
	}
	s.revMu.Unlock()
	return out, nil
}

// prURL is the PR's page, from gh or, without one, from its key.
func prURL(pr prView) string {
	if pr.URL != "" {
		return pr.URL
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", pr.Repo, pr.Number)
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.summary(r.Context()))
}
