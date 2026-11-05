package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/beroni/bazel/internal/gh"
)

// The "Needs you" page: the open PRs where the user has something to do. Each
// PR appears once, under the first of these that applies, with the reason
// written out.
const (
	needsRequested = "requested" // a review is asked of you and you have not given it
	needsReplied   = "replied"   // you asked for changes and the author answered
	needsWaiting   = "waiting"   // you asked for changes and the author has not answered yet
	needsYours     = "yours"     // your own PR has something for you
)

// needsItem is one PR on the page.
type needsItem struct {
	prView
	Category string    `json:"category"`
	Reason   string    `json:"reason"`
	Since    time.Time `json:"since"`
	SinceAge string    `json:"since_age"`
	Watched  bool      `json:"watched"`
}

// classifyNeeds puts each PR under its category, or leaves it out. The rules are
// the conservative ones: only what the PR's author did counts as an answer, a
// bot's comment is no news, and a time the API does not give is not guessed.
func classifyNeeds(me string, p gh.NeedsPR) (category, reason string, since time.Time) {
	is := func(a gh.Actor) bool { return strings.EqualFold(a.Login, me) }
	author := p.Author.Login
	byAuthor := func(a gh.Actor) bool { return strings.EqualFold(a.Login, author) }
	bot := func(a gh.Actor) bool {
		return a.Typename == "Bot" || strings.HasSuffix(strings.ToLower(a.Login), "[bot]")
	}
	pushedAt := p.PushedAt()

	if strings.EqualFold(author, me) {
		return yoursReason(me, p, pushedAt, is, bot)
	}

	// My last review that says something: drafts (PENDING) are not given yet.
	type review struct {
		id, state string
		at        time.Time
	}
	var mine *review
	for _, r := range p.Reviews.Nodes {
		if !is(r.Author) || r.State == "PENDING" || r.SubmittedAt.IsZero() {
			continue
		}
		if mine == nil || r.SubmittedAt.After(mine.at) {
			mine = &review{r.ID, r.State, r.SubmittedAt}
		}
	}

	// 1. Asked to review. The search found it, so a request is pending for me
	// or for a team of mine. A review of mine after the last request answers it.
	if p.Requested {
		var asked time.Time
		for _, r := range p.Requests.Nodes {
			rr := r.RequestedReviewer
			if rr == nil || rr.Typename == "Team" || strings.EqualFold(rr.Login, me) {
				if r.CreatedAt.After(asked) {
					asked = r.CreatedAt
				}
			}
		}
		if mine == nil || asked.IsZero() || mine.at.Before(asked) {
			if asked.IsZero() {
				asked = p.UpdatedAt
			}
			return needsRequested, "review requested from you", asked
		}
	}

	// 2 and 3. I asked for changes: my last review says so, or a thread I
	// started is still open. askedAt is the latest of those.
	var askedAt time.Time
	askReview := ""
	if mine != nil && mine.state == "CHANGES_REQUESTED" {
		askedAt, askReview = mine.at, mine.id
	}
	openThreads := 0
	for _, t := range p.ReviewThreads.Nodes {
		if t.IsResolved || len(t.FirstComment.Nodes) == 0 || !is(t.FirstComment.Nodes[0].Author) {
			continue
		}
		openThreads++
		// When I last spoke in it: my last reply, or the thread itself.
		at := t.FirstComment.Nodes[0].CreatedAt
		for _, c := range t.LastComment.Nodes {
			if is(c.Author) && c.CreatedAt.After(at) {
				at = c.CreatedAt
			}
		}
		if at.After(askedAt) {
			askedAt = at
		}
	}
	if askedAt.IsZero() {
		return "", "", time.Time{}
	}

	// What the author did after that: a push, a comment, a reply in a thread,
	// or resolving a thread of the review that asked.
	var answered time.Time
	what := ""
	note := func(at time.Time, w string) {
		if at.After(askedAt) && at.After(answered) {
			answered, what = at, w
		}
	}
	note(pushedAt, "pushed")
	for _, c := range p.Comments.Nodes {
		if byAuthor(c.Author) && !bot(c.Author) {
			note(c.CreatedAt, "commented")
		}
	}
	for _, t := range p.ReviewThreads.Nodes {
		for _, c := range t.LastComment.Nodes {
			if byAuthor(c.Author) {
				note(c.CreatedAt, "replied in a thread")
			}
		}
		// A resolution has no time of its own: it counts only for a thread of
		// the very review that asked, resolved by the author.
		if t.IsResolved && askReview != "" && t.ResolvedBy != nil && byAuthor(*t.ResolvedBy) && len(t.FirstComment.Nodes) > 0 {
			if pr := t.FirstComment.Nodes[0].PullRequestReview; pr != nil && pr.ID == askReview && answered.IsZero() {
				answered, what = askedAt, "resolved your threads"
			}
		}
	}
	asked := "you asked for changes"
	if askReview == "" {
		asked = fmt.Sprintf("you have %d open thread%s", openThreads, plural(openThreads))
	}
	if !answered.IsZero() {
		return needsReplied, fmt.Sprintf("%s; @%s %s since", asked, author, what), answered
	}
	return needsWaiting, asked + "; waiting for @" + author, askedAt
}

// yoursReason is category 4: the first thing on my own PR that is for me.
func yoursReason(me string, p gh.NeedsPR, pushedAt time.Time, is func(gh.Actor) bool, bot func(gh.Actor) bool) (string, string, time.Time) {
	// The last thing I did on it: a push, a comment, a reply.
	mineAt := pushedAt
	for _, c := range p.Comments.Nodes {
		if is(c.Author) && c.CreatedAt.After(mineAt) {
			mineAt = c.CreatedAt
		}
	}
	for _, t := range p.ReviewThreads.Nodes {
		for _, c := range t.LastComment.Nodes {
			if is(c.Author) && c.CreatedAt.After(mineAt) {
				mineAt = c.CreatedAt
			}
		}
	}
	var changesAt time.Time
	changesBy := ""
	var approvedAt time.Time
	for _, r := range p.Reviews.Nodes {
		switch {
		case is(r.Author):
		case r.State == "CHANGES_REQUESTED" && r.SubmittedAt.After(changesAt):
			changesAt, changesBy = r.SubmittedAt, r.Author.Login
		case r.State == "APPROVED" && r.SubmittedAt.After(approvedAt):
			approvedAt = r.SubmittedAt
		}
	}
	if p.ReviewDecision == "CHANGES_REQUESTED" && changesBy != "" {
		return needsYours, "changes requested by @" + changesBy, changesAt
	}
	waitingThreads, threadAt := 0, time.Time{}
	for _, t := range p.ReviewThreads.Nodes {
		if t.IsResolved || len(t.LastComment.Nodes) == 0 {
			continue
		}
		c := t.LastComment.Nodes[0]
		if !is(c.Author) && !bot(c.Author) {
			waitingThreads++
			if c.CreatedAt.After(threadAt) {
				threadAt = c.CreatedAt
			}
		}
	}
	if waitingThreads > 0 {
		return needsYours, fmt.Sprintf("%d thread%s waiting for your answer", waitingThreads, plural(waitingThreads)), threadAt
	}
	var commentAt time.Time
	commentBy := ""
	for _, c := range p.Comments.Nodes {
		if !is(c.Author) && !bot(c.Author) && c.CreatedAt.After(mineAt) && c.CreatedAt.After(commentAt) {
			commentAt, commentBy = c.CreatedAt, c.Author.Login
		}
	}
	if commentBy != "" {
		return needsYours, "new comment from @" + commentBy, commentAt
	}
	ci := p.Checks()
	if ci == "FAILURE" || ci == "ERROR" {
		return needsYours, "checks failing", pushedAt
	}
	if p.Mergeable == "CONFLICTING" {
		return needsYours, "conflicts with " + p.BaseRefName, p.UpdatedAt
	}
	if p.ReviewDecision == "APPROVED" && p.Mergeable == "MERGEABLE" && !p.IsDraft && (ci == "" || ci == "SUCCESS") {
		at := approvedAt
		if at.IsZero() {
			at = p.UpdatedAt
		}
		return needsYours, "approved, ready to merge", at
	}
	return "", "", time.Time{}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// needsRank orders the categories on the page.
var needsRank = map[string]int{needsRequested: 0, needsReplied: 1, needsWaiting: 2, needsYours: 3}

// needsFetchTTL is how long a read of the page serves before GitHub is asked
// again; the refresh button asks sooner, under the list's budget.
var needsFetchTTL = time.Minute

// needsCache is the last read of the searches.
type needsCache struct {
	at        time.Time
	prs       []gh.NeedsPR
	truncated bool
	err       string
}

func (s *Server) handleNeeds(w http.ResponseWriter, r *http.Request) {
	if s.me == "" {
		writeErr(w, http.StatusServiceUnavailable, errors.New("who you are on GitHub is not known yet"))
		return
	}
	force := r.URL.Query().Get("refresh") == "1"
	all := r.URL.Query().Get("all") == "1"
	c, err := s.loadNeeds(r.Context(), force)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	s.cfgMu.Lock()
	watched := lowerRepos(s.cfg.Repos)
	s.cfgMu.Unlock()
	now := timeNow()
	items := []needsItem{}
	hidden := 0
	for _, p := range c.prs {
		cat, reason, since := classifyNeeds(s.me, p)
		if cat == "" {
			continue
		}
		in := watched[strings.ToLower(p.Repo())]
		if !all && !in {
			hidden++
			continue
		}
		if since.IsZero() {
			since = p.UpdatedAt
		}
		items = append(items, needsItem{
			prView:   newPRView(needsAsPR(p), strings.EqualFold(p.Author.Login, s.me), now),
			Category: cat, Reason: reason, Since: since, SinceAge: gh.PR{UpdatedAt: since}.Age(now), Watched: in,
		})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if a, b := needsRank[items[i].Category], needsRank[items[j].Category]; a != b {
			return a < b
		}
		return items[i].Since.After(items[j].Since)
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "fetched_at": c.at, "truncated": c.truncated, "elsewhere": hidden, "all": all,
	})
}

// loadNeeds reads the searches, from the cache or from GitHub. One gh call,
// charged to the same budgets as the PR list and the details.
func (s *Server) loadNeeds(ctx context.Context, force bool) (needsCache, error) {
	now := timeNow()
	s.needsMu.Lock()
	defer s.needsMu.Unlock()
	c := s.needs
	fresh := !c.at.IsZero() && now.Sub(c.at) < needsFetchTTL
	if fresh && (!force || c.err != "") {
		return c, c.answer()
	}
	s.detMu.Lock()
	allowed := true
	if fresh {
		allowed = s.listReadLocked(now, 1)
	}
	var budgetErr error
	if allowed {
		budgetErr = s.ghReadLocked(now, false)
	}
	s.detMu.Unlock()
	if !allowed || budgetErr != nil {
		if !c.at.IsZero() {
			return c, c.answer()
		}
		if budgetErr == nil {
			budgetErr = errors.New("too many refreshes; wait a minute")
		}
		return c, budgetErr
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), detailsReadTimeout)
	defer cancel()
	prs, truncated, err := gh.FetchNeeds(ctx, s.me)
	if err != nil {
		// A failed read keeps the last good one on the page.
		if !c.at.IsZero() && c.err == "" {
			return c, nil
		}
		s.needs = needsCache{at: timeNow(), err: err.Error()}
		return s.needs, err
	}
	s.needs = needsCache{at: timeNow(), prs: prs, truncated: truncated}
	return s.needs, nil
}

func (c needsCache) answer() error {
	if c.err != "" {
		return errors.New(c.err)
	}
	return nil
}

// needsAsPR is the part of a needs-you PR the list's view knows how to draw.
func needsAsPR(p gh.NeedsPR) gh.PR {
	pr := gh.PR{
		Repo: p.Repo(), Number: p.Number, Title: p.Title, URL: p.URL, IsDraft: p.IsDraft,
		Additions: p.Additions, Deletions: p.Deletions, ChangedFiles: p.ChangedFiles,
		UpdatedAt: p.UpdatedAt, HeadRefName: p.HeadRefName, BaseRefName: p.BaseRefName,
		HeadRefOid: p.HeadRefOid, ReviewDecision: p.ReviewDecision,
	}
	pr.Author.Login = p.Author.Login
	return pr
}

func lowerRepos(repos []string) map[string]bool {
	m := make(map[string]bool, len(repos))
	for _, r := range repos {
		m[strings.ToLower(r)] = true
	}
	return m
}
