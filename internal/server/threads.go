package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/beroni/bazel/internal/gh"
)

// What a PR has discussed is read from GitHub in one batched GraphQL call, only
// reads, and kept a minute: the list's "open" tags, the Overview's section and
// the discussion.json the review fleet gets all come from the same read. One
// read at a time, charged to the GitHub budget, and a failed one waits out the
// same backoff as Needs you. Nothing here blocks anything: without the read,
// there are no tags and no warnings.

// conversationTTL is how long a read of a PR's conversation serves.
const conversationTTL = time.Minute

type conversationEntry struct {
	conv gh.Conversation
	at   time.Time
}

// conversationsOf is what each PR has discussed, by lower-case key: what was
// read in the last minute, and gh reads for the rest, a batch per call. force
// reads them all again (the list's ↻), but not inside a failure's backoff.
func (s *Server) conversationsOf(ctx context.Context, keys []string, force bool) (map[string]gh.Conversation, error) {
	out := map[string]gh.Conversation{}
	var missing []string
	s.convMu.Lock()
	defer s.convMu.Unlock()
	now := timeNow()
	// What is past keeping goes now, failure or not: a read that keeps failing
	// must not leave a fallback that never ages.
	for k, e := range s.convs {
		if now.Sub(e.at) >= conversationKept {
			delete(s.convs, k)
		}
	}
	for _, key := range keys {
		k := strings.ToLower(key)
		if e, ok := s.convs[k]; ok && !force && now.Sub(e.at) < conversationTTL {
			out[k] = e.conv
		} else {
			missing = append(missing, key)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	// The ones read longest ago, or never, go first: with the budget spent
	// before the end of the list, the same PRs must not be the ones left out
	// every minute.
	slices.SortStableFunc(missing, func(a, b string) int {
		return s.convs[strings.ToLower(a)].at.Compare(s.convs[strings.ToLower(b)].at)
	})
	if now.Before(s.convHold) {
		return out, fmt.Errorf("GitHub's rate limit is low: the open threads wait until %s", s.convHold.Local().Format("15:04"))
	}
	if f := s.convFailed; !f.failedAt.IsZero() && now.Sub(f.failedAt) < f.backoff() {
		return out, errors.New(f.err)
	}
	for len(missing) > 0 {
		n := min(len(missing), gh.ConversationBatch)
		batch := missing[:n]
		missing = missing[n:]
		// A budget of their own: however many PRs the list has, these reads
		// never take the Overview's.
		s.convReads = lastMinute(s.convReads, timeNow())
		if len(s.convReads) >= convReadsPerMin {
			return out, errors.New("too many reads of the PRs' threads; the rest come next minute")
		}
		s.convReads = append(s.convReads, timeNow())
		// Not the request's context: a tab closed mid-read must not fail the
		// others waiting on this one. The time limit is per call.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), detailsReadTimeout)
		got, rl, err := s.fetchThreadTags(rctx, batch)
		cancel()
		if err != nil {
			s.convFailed = needsCache{failedAt: timeNow(), failures: s.convFailed.failures + 1, err: err.Error()}
			return out, err
		}
		// Past the margin, these reads stop until GitHub's hour starts again:
		// the list, Needs you, the summary and publishing keep the rest.
		low := !rl.ResetAt.IsZero() && rl.Remaining < convRateMargin
		if low {
			s.convHold = rl.ResetAt
		}
		at := timeNow()
		for _, key := range batch {
			k := strings.ToLower(key)
			c, ok := got[key]
			if !ok { // GitHub did not answer for it: nothing to say, for a minute
				c = gh.Conversation{Threads: []gh.Thread{}, Comments: []gh.Said{}, Reviews: []gh.Said{}}
			}
			s.convs[k] = conversationEntry{conv: c, at: at}
			out[k] = c
		}
		if low && len(missing) > 0 {
			return out, fmt.Errorf("GitHub's rate limit is low (%d left): the open threads wait until %s", rl.Remaining, rl.ResetAt.Local().Format("15:04"))
		}
	}
	// Only a read where every call went well ends the backoff: one batch that
	// worked says nothing of the one that keeps failing.
	s.convFailed = needsCache{}
	return out, nil
}

// lastConversations fills out with the last good read of each key it lacks,
// however old (within conversationKept), and returns the oldest such read: a
// failed read still shows what the one before said, as stale.
func (s *Server) lastConversations(keys []string, out map[string]gh.Conversation) time.Time {
	var oldest time.Time
	s.convMu.Lock()
	defer s.convMu.Unlock()
	for _, key := range keys {
		k := strings.ToLower(key)
		if _, ok := out[k]; ok {
			continue
		}
		if e, ok := s.convs[k]; ok && timeNow().Sub(e.at) < conversationKept {
			out[k] = e.conv
			if oldest.IsZero() || e.at.Before(oldest) {
				oldest = e.at
			}
		}
	}
	return oldest
}

// convReadsPerMin caps the gh calls of these reads, gh.ConversationBatch PRs
// each, apart from the budget of the Overview and the list.
var convReadsPerMin = 20

// convRateMargin is the GraphQL points these reads always leave to the rest of
// Bazel: below it, they stop until the rate limit resets.
var convRateMargin = 1000

// conversationKept is how long a good read is kept past its minute: when the
// next one fails, the page still shows what it said, as stale.
const conversationKept = 30 * time.Minute

// openThread is a thread you opened on a PR that nobody resolved yet.
type openThread struct {
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	URL      string `json:"url,omitempty"`
	Outdated bool   `json:"outdated,omitempty"`
	Replies  int    `json:"replies"`
	// More is how many replies one read did not bring; the last one is read
	// on its own, so it is the real last word.
	More int       `json:"more,omitempty"`
	Last *lastSaid `json:"last,omitempty"`
	// AuthorReplied says the last word is the PR author's: they answered, and
	// the thread waits on you.
	AuthorReplied bool `json:"author_replied,omitempty"`
}

type lastSaid struct {
	Author string    `json:"author"`
	Body   string    `json:"body"`
	At     time.Time `json:"at"`
}

// openThreadsOf picks, from the conversation of the PR key, the threads me
// started that are still open. An outdated one is still open, and says so.
func openThreadsOf(c gh.Conversation, me, key string) []openThread {
	out := []openThread{}
	if me == "" {
		return out
	}
	// On your own PR the author's reply is yours: nothing to flag.
	mine := strings.EqualFold(c.Author.Login, me)
	for _, t := range c.Threads {
		if t.IsResolved || len(t.Comments) == 0 || !strings.EqualFold(t.Comments[0].Author.Login, me) {
			continue
		}
		total := max(t.Total, len(t.Comments))
		ot := openThread{Path: t.Path, Line: t.Line, URL: threadURL(t.URL, key), Outdated: t.IsOutdated,
			Replies: total - 1, More: total - len(t.Comments)}
		last := t.Last
		if last == nil {
			last = &t.Comments[len(t.Comments)-1]
		}
		if total > 1 {
			ot.Last = &lastSaid{Author: last.Author.Login, Body: clip(last.Body, 280), At: last.At}
			ot.AuthorReplied = !mine && c.Author.Login != "" && strings.EqualFold(last.Author.Login, c.Author.Login)
		}
		out = append(out, ot)
	}
	return out
}

// threadURL is u when it is a page of the PR key on GitHub, or "": a link the
// page draws goes nowhere else.
func threadURL(u, key string) string {
	repo, number, ok := strings.Cut(key, "#")
	if !ok {
		return ""
	}
	prefix := "https://github.com/" + strings.ToLower(repo) + "/pull/" + number
	if lu := strings.ToLower(u); lu == prefix || strings.HasPrefix(lu, prefix+"#") || strings.HasPrefix(lu, prefix+"/") || strings.HasPrefix(lu, prefix+"?") {
		return u
	}
	return ""
}

// clip cuts s to at most n runes on one line, with an ellipsis when it did.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// handleOpenThreads answers, for the PRs of the list, the threads you opened
// that are still open. A failed read answers what it has and says why.
func (s *Server) handleOpenThreads(w http.ResponseWriter, r *http.Request) {
	s.prMu.RLock()
	keys := make([]string, 0, len(s.prs))
	for _, pr := range s.prs {
		keys = append(keys, pr.Key())
	}
	s.prMu.RUnlock()
	convs, err := s.conversationsOf(r.Context(), keys, r.URL.Query().Get("refresh") == "1")
	var staleAt time.Time
	if err != nil {
		staleAt = s.lastConversations(keys, convs)
	}
	threads := map[string][]openThread{}
	for k, c := range convs {
		if t := openThreadsOf(c, s.me, k); len(t) > 0 {
			threads[k] = t
		}
	}
	out := map[string]any{"threads": threads}
	if err != nil {
		out["error"] = "open threads could not be read: " + err.Error()
		// What the last good read said, as Needs you keeps its last list.
		if !staleAt.IsZero() {
			out["stale"] = true
			out["stale_at"] = staleAt
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// discussionBody caps each text in discussion.json: a long body is cut and
// says so, as "cut". Past discussionThreads threads or discussionBytes in all,
// the file keeps what fits and says complete: false.
const discussionBody = 4000

var (
	discussionThreads = 200
	discussionBytes   = 2 << 20
)

// The shape bazel-review-fleet's discussion.jq reads. Every author carries its
// type (User or Bot) and, on what they wrote, its association.
type (
	discussionWho struct {
		Login       string `json:"login"`
		Type        string `json:"type"`
		Association string `json:"association,omitempty"`
	}
	discussionSaid struct {
		Author    discussionWho `json:"author"`
		CreatedAt time.Time     `json:"createdAt"`
		Body      string        `json:"body"`
		URL       string        `json:"url,omitempty"`
		Cut       bool          `json:"cut,omitempty"`
	}
	discussionReview struct {
		Author      discussionWho `json:"author"`
		State       string        `json:"state"`
		SubmittedAt time.Time     `json:"submittedAt"`
		Body        string        `json:"body"`
		URL         string        `json:"url,omitempty"`
		Cut         bool          `json:"cut,omitempty"`
	}
	discussionThread struct {
		URL        string           `json:"url,omitempty"`
		Path       string           `json:"path,omitempty"`
		Line       *int             `json:"line"`
		StartLine  *int             `json:"start_line"`
		Side       string           `json:"side,omitempty"`
		IsResolved bool             `json:"isResolved"`
		IsOutdated bool             `json:"isOutdated"`
		ResolvedBy *discussionWho   `json:"resolvedBy"`
		Comments   []discussionSaid `json:"comments"`
	}
	discussionFile struct {
		Repo     string             `json:"repo"`
		PR       int                `json:"pr"`
		HeadSha  string             `json:"head_sha"`
		Complete bool               `json:"complete"`
		Author   discussionWho      `json:"author"`
		Threads  []discussionThread `json:"threads"`
		Comments []discussionSaid   `json:"comments"`
		Reviews  []discussionReview `json:"reviews"`
	}
)

func whoOf(w gh.Who) discussionWho {
	t := "User"
	if w.IsBot() {
		t = "Bot"
	}
	return discussionWho{Login: w.Login, Type: t, Association: w.Association}
}

// cutBody keeps a body within discussionBody, marked when it was cut.
func cutBody(b string) (string, bool) {
	if utf8.RuneCountInString(b) <= discussionBody {
		return b, false
	}
	return string([]rune(b)[:discussionBody]) + " … [cut by Bazel]", true
}

func saidOf(x gh.Said) discussionSaid {
	body, cut := cutBody(x.Body)
	return discussionSaid{Author: whoOf(x.Author), CreatedAt: x.At, Body: body, URL: x.URL, Cut: cut}
}

func optLine(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

// discussionFor is what the review fleet reads as <plugin>/discussion.json:
// the PR's threads (resolved or open, with their replies), comments and
// reviews, its author on top, at the head the review reads. A failed read is
// an error and no file: the review goes without it.
func (s *Server) discussionFor(ctx context.Context, pr gh.PR) ([]byte, error) {
	// The full read, for this job alone: the tags' read is too thin for it.
	key := pr.Key()
	rctx, cancel := context.WithTimeout(ctx, detailsReadTimeout)
	defer cancel()
	convs, err := s.fetchConversations(rctx, []string{key})
	if err != nil {
		return nil, err
	}
	c, ok := convs[key]
	if !ok {
		return nil, errors.New("GitHub said nothing of this PR")
	}
	author := c.Author
	if author.Login == "" {
		author = gh.Who{Login: pr.Author.Login}
	}
	f := discussionFile{
		Repo: pr.Repo, PR: pr.Number, HeadSha: pr.HeadRefOid, Complete: c.Complete,
		Author:  whoOf(gh.Who{Login: author.Login, Type: author.Type}),
		Threads: []discussionThread{}, Comments: []discussionSaid{}, Reviews: []discussionReview{},
	}
	for _, t := range c.Threads {
		dt := discussionThread{URL: t.URL, Path: t.Path, Line: optLine(t.Line), StartLine: optLine(t.StartLine), Side: t.Side,
			IsResolved: t.IsResolved, IsOutdated: t.IsOutdated, Comments: []discussionSaid{}}
		if t.ResolvedBy != nil {
			w := whoOf(*t.ResolvedBy)
			w.Association = ""
			dt.ResolvedBy = &w
		}
		for _, x := range t.Comments {
			dt.Comments = append(dt.Comments, saidOf(x))
		}
		f.Threads = append(f.Threads, dt)
	}
	for _, x := range c.Comments {
		f.Comments = append(f.Comments, saidOf(x))
	}
	for _, x := range c.Reviews {
		body, cut := cutBody(x.Body)
		f.Reviews = append(f.Reviews, discussionReview{Author: whoOf(x.Author), State: x.State, SubmittedAt: x.At, Body: body, URL: x.URL, Cut: cut})
	}
	if len(f.Threads) > discussionThreads {
		f.Threads, f.Complete = f.Threads[:discussionThreads], false
	}
	data, err := json.Marshal(f)
	// Too big: the newest of each list goes first, until it fits.
	for err == nil && len(data) > discussionBytes && len(f.Threads)+len(f.Comments)+len(f.Reviews) > 0 {
		f.Complete = false
		switch {
		case len(f.Reviews) > 0:
			f.Reviews = f.Reviews[:len(f.Reviews)/2]
		case len(f.Comments) > 0:
			f.Comments = f.Comments[:len(f.Comments)/2]
		default:
			f.Threads = f.Threads[:len(f.Threads)/2]
		}
		data, err = json.Marshal(f)
	}
	return data, err
}
