package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
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

// threadsRead is one read of the tags, PR by PR: what each said (fresh or,
// past a failure, the last good read), when a stale one was read, and why a PR
// has nothing fresh.
type threadsRead struct {
	convs map[string]gh.Conversation
	stale map[string]time.Time
	errs  map[string]string
	fresh int
	// reading says some PRs wait on a read under way: ask again shortly.
	// loading says one of them has nothing yet: the page says so, not that it
	// could not be read.
	reading, loading bool
}

// conversationsOf is readThreads for callers that want a map and an error: the
// PRs with something to say, and the first reason one has nothing fresh.
func (s *Server) conversationsOf(ctx context.Context, keys []string, force bool) (map[string]gh.Conversation, error) {
	r := s.readThreads(ctx, keys, force)
	for _, key := range keys {
		if e, ok := r.errs[strings.ToLower(key)]; ok {
			return r.convs, errors.New(e)
		}
	}
	return r.convs, nil
}

// readThreads is what each PR has discussed, by lower-case key: what was read
// in the last minute, and gh reads for the rest, a batch per call. force reads
// them all again (the list's ↻), but not a PR inside its own backoff.
//
// One read at a time, and nobody waits on it: the state is only locked to look
// and to write, never across a gh call, so a call that hangs holds no page; a
// request that comes while a read is out answers at once from the cache, or
// what the last good read said.
//
// A failure is a PR's or its batch's, never the whole list's: the PRs it took
// go back in the rotation and wait out their own backoff, the next batch still
// goes, a batch that times out is split in two until the PR that will not
// answer is alone, and from then on that PR is read alone.
func (s *Server) readThreads(ctx context.Context, keys []string, force bool) threadsRead {
	r := threadsRead{convs: map[string]gh.Conversation{}, stale: map[string]time.Time{}, errs: map[string]string{}}
	s.convMu.Lock()
	now := timeNow()
	// What is past keeping goes now, failure or not: a read that keeps failing
	// must not leave a fallback that never ages.
	for k, e := range s.convs {
		if now.Sub(e.at) >= conversationKept {
			delete(s.convs, k)
		}
	}
	var missing []string
	for _, key := range keys {
		k := strings.ToLower(key)
		// A PR whose last read failed is not fresh, whatever its cache says,
		// until a read of it works: within its backoff it waits, past it, it
		// is read again.
		if f, ok := s.convFails[k]; ok {
			if now.Sub(f.failedAt) < f.backoff() {
				r.errs[k] = f.err
			} else {
				missing = append(missing, key)
			}
			continue
		}
		if e, ok := s.convs[k]; ok && !force && now.Sub(e.at) < conversationTTL {
			r.convs[k] = e.conv
			r.fresh++
			continue
		}
		missing = append(missing, key)
	}
	// The ones tried longest ago, or never, go first: with the budget spent
	// before the end of the list, the same PRs must not be the ones left out
	// every minute, and a PR that failed goes to the back like one just read.
	slices.SortStableFunc(missing, func(a, b string) int {
		return s.convTried[strings.ToLower(a)].Compare(s.convTried[strings.ToLower(b)])
	})
	var why string
	switch {
	case len(missing) == 0:
	case now.Before(s.convHold):
		why = fmt.Sprintf("GitHub's rate limit is low: the open threads wait until %s", s.convHold.Local().Format("15:04"))
	case s.convReading:
		why = "the open threads are being read"
	default:
		s.convReading = true
	}
	// A PR that failed before is read alone: one that will not answer never
	// takes a batch of good ones down with it again.
	var queue [][]string
	var batch []string
	for _, key := range missing {
		if _, failed := s.convFails[strings.ToLower(key)]; failed {
			queue = append(queue, []string{key})
			continue
		}
		if batch = append(batch, key); len(batch) == gh.ConversationBatch {
			queue, batch = append(queue, batch), nil
		}
	}
	if len(batch) > 0 {
		queue = append(queue, batch)
	}
	s.convMu.Unlock()

	// Another read is out: its PRs are being read, not failing. What they
	// have is shown as it is, and one with nothing yet says it is loading.
	reading := map[string]bool{}
	switch {
	case why == "the open threads are being read":
		for _, key := range missing {
			reading[strings.ToLower(key)] = true
		}
		r.reading = true
	case why != "":
		for _, key := range missing {
			r.errs[strings.ToLower(key)] = why
		}
	case len(missing) > 0:
		s.readQueueOnce(ctx, queue, &r)
	}

	// What has nothing fresh shows what the last good read said: stale, with
	// when, unless it only waits on the read under way.
	s.convMu.Lock()
	for _, key := range keys {
		k := strings.ToLower(key)
		if _, ok := r.convs[k]; ok {
			continue
		}
		e, ok := s.convs[k]
		switch {
		case ok && timeNow().Sub(e.at) < conversationKept:
			r.convs[k] = e.conv
			if !reading[k] {
				r.stale[k] = e.at
			}
		case reading[k]:
			r.loading = true
		}
	}
	s.convMu.Unlock()
	return r
}

// readQueueOnce runs the read this call took, and lets the next one go however
// it ends: a panic in it must not leave the tags stuck until a restart.
func (s *Server) readQueueOnce(ctx context.Context, queue [][]string, r *threadsRead) {
	defer func() {
		s.convMu.Lock()
		s.convReading = false
		s.convMu.Unlock()
		if p := recover(); p != nil {
			fmt.Fprintf(os.Stderr, "bazel: the open threads' read failed: %v\n", p)
			for _, batch := range queue {
				for _, key := range batch {
					r.errs[strings.ToLower(key)] = fmt.Sprintf("the read failed: %v", p)
				}
			}
		}
	}()
	s.readQueue(ctx, queue, r)
}

// readQueue reads the batches, the state locked only to take from the budget
// and to write down what came back.
func (s *Server) readQueue(ctx context.Context, queue [][]string, r *threadsRead) {
	for len(queue) > 0 {
		batch := queue[0]
		queue = queue[1:]
		s.convMu.Lock()
		ok := s.takeConvRead()
		s.convMu.Unlock()
		if !ok {
			for _, b := range append([][]string{batch}, queue...) {
				for _, key := range b {
					r.errs[strings.ToLower(key)] = "too many reads of the PRs' threads; the rest come next minute"
				}
			}
			return
		}
		// Not the request's context: a tab closed mid-read must not cut a read
		// others will use. The time limit is per call.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), convCallTimeout)
		got, rl, err := s.fetchThreadTags(rctx, batch)
		cancel()

		s.convMu.Lock()
		at := timeNow()
		for _, key := range batch {
			s.convTried[strings.ToLower(key)] = at
		}
		if err != nil {
			if timedOut(err) && len(batch) > 1 {
				s.convMu.Unlock()
				half := len(batch) / 2
				queue = append([][]string{batch[:half], batch[half:]}, queue...)
				continue
			}
			for _, key := range batch {
				k := strings.ToLower(key)
				f := s.convFails[k]
				s.convFails[k] = needsCache{failedAt: at, failures: f.failures + 1, err: err.Error()}
				r.errs[k] = err.Error()
			}
			s.convMu.Unlock()
			continue
		}
		low := s.holdIfLow(rl)
		s.convMu.Unlock()
		for _, key := range batch {
			k := strings.ToLower(key)
			c, ok := got[key]
			if !ok { // GitHub did not answer for it: nothing to say, for a minute
				c = gh.Conversation{Threads: []gh.Thread{}, Comments: []gh.Said{}, Reviews: []gh.Said{}}
			}
			if c.ThreadsAfter != "" && !low {
				c = s.moreThreads(ctx, key, c)
			}
			s.convMu.Lock()
			delete(s.convFails, k)
			s.convs[k] = conversationEntry{conv: c, at: at}
			s.convMu.Unlock()
			r.convs[k] = c
			r.fresh++
		}
		// Past the margin, these reads stop until GitHub's hour starts again:
		// the list, Needs you, the summary and publishing keep the rest.
		if low {
			s.convMu.Lock()
			why := fmt.Sprintf("GitHub's rate limit is low (%d left): the open threads wait until %s", rl.Remaining, s.convHold.Local().Format("15:04"))
			s.convMu.Unlock()
			for _, b := range queue {
				for _, key := range b {
					r.errs[strings.ToLower(key)] = why
				}
			}
			return
		}
	}
}

// takeConvRead takes one call from these reads' own budget, which never is
// the Overview's. Call with convMu held.
func (s *Server) takeConvRead() bool {
	now := timeNow()
	s.convReads = lastMinute(s.convReads, now)
	if len(s.convReads) >= convReadsPerMin {
		return false
	}
	s.convReads = append(s.convReads, now)
	return true
}

// convMorePages caps the pages a PR reads past its first: past it, the PR is
// partial, and says so, instead of spending the minute's budget alone.
var convMorePages = 3

// moreThreads reads the rest of a PR's threads, a page a call, each from the
// same budget, up to convMorePages: past that, or past the budget or the rate
// limit's margin, the tags keep what they have, which is partial
// (ThreadsAfter is left set). Called without convMu.
func (s *Server) moreThreads(ctx context.Context, key string, c gh.Conversation) gh.Conversation {
	for page := 0; c.ThreadsAfter != "" && page < convMorePages; page++ {
		s.convMu.Lock()
		ok := s.takeConvRead()
		s.convMu.Unlock()
		if !ok {
			break
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), convCallTimeout)
		more, rl, err := s.fetchThreadTagsPage(rctx, key, c.ThreadsAfter)
		cancel()
		if err != nil {
			break
		}
		c.Threads = append(c.Threads, more.Threads...)
		c.ThreadsAfter = more.ThreadsAfter
		s.convMu.Lock()
		low := s.holdIfLow(rl)
		s.convMu.Unlock()
		if low {
			break
		}
	}
	return c
}

// holdIfLow holds the tags' reads when GitHub says few points are left, for
// as long as it says, between a minute and an hour; with no reset time it can
// trust (none, or one already past: a clock ahead of GitHub's), five minutes.
// Call with convMu held.
func (s *Server) holdIfLow(rl gh.RateLimit) bool {
	if !rl.Known || rl.Remaining >= convRateMargin {
		return false
	}
	now := timeNow()
	wait := convHoldUnknown
	if d := rl.ResetAt.Sub(now); !rl.ResetAt.IsZero() && d > 0 {
		wait = min(max(d, convHoldMin), convHoldMax)
	}
	if until := now.Add(wait); until.After(s.convHold) {
		s.convHold = until
	}
	return true
}

func timedOut(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "timeout") ||
		strings.Contains(err.Error(), "deadline exceeded")
}

// How long the tags' reads wait once GitHub's points run low, and how long one
// call may take.
var (
	convHoldMin     = time.Minute
	convHoldMax     = time.Hour
	convHoldUnknown = 5 * time.Minute
	convCallTimeout = 15 * time.Second
)

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
// that are still open, PR by PR: what came from an earlier read says when
// (stale), a PR with nothing says why (errors), one past its pages says it is
// partial; error is only for when no PR could be read at all.
func (s *Server) handleOpenThreads(w http.ResponseWriter, r *http.Request) {
	s.prMu.RLock()
	keys := make([]string, 0, len(s.prs))
	for _, pr := range s.prs {
		keys = append(keys, pr.Key())
	}
	s.prMu.RUnlock()
	read := s.readThreads(r.Context(), keys, r.URL.Query().Get("refresh") == "1")
	threads := map[string][]openThread{}
	partial := map[string]bool{}
	for k, c := range read.convs {
		if t := openThreadsOf(c, s.me, k); len(t) > 0 {
			threads[k] = t
			if c.ThreadsAfter != "" {
				partial[k] = true
			}
		}
	}
	out := map[string]any{"threads": threads}
	if len(read.stale) > 0 {
		out["stale"] = read.stale
	}
	if len(partial) > 0 {
		out["partial"] = partial
	}
	if read.reading {
		out["reading"] = true
	}
	if read.loading {
		out["loading"] = true
	}
	if len(read.errs) > 0 {
		out["errors"] = read.errs
		if read.fresh == 0 && len(read.stale) == 0 && !read.loading {
			for _, key := range keys {
				if e, ok := read.errs[strings.ToLower(key)]; ok {
					out["error"] = "open threads could not be read: " + e
					break
				}
			}
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
	// It keeps to the same rules as the tags: not while GitHub's points are
	// below the margin, nor inside a failure's backoff, and a read that says
	// they ran low holds the tags too.
	key := pr.Key()
	s.convMu.Lock()
	now := timeNow()
	hold, failed := s.convHold, s.convFails[strings.ToLower(key)]
	s.convMu.Unlock()
	if now.Before(hold) {
		return nil, fmt.Errorf("GitHub's rate limit is low: no discussion is read until %s", hold.Local().Format("15:04"))
	}
	if !failed.failedAt.IsZero() && now.Sub(failed.failedAt) < failed.backoff() {
		return nil, fmt.Errorf("reading this PR failed a moment ago (%s): read again after the backoff", failed.err)
	}
	rctx, cancel := context.WithTimeout(ctx, detailsReadTimeout)
	defer cancel()
	convs, rl, err := s.fetchConversations(rctx, []string{key})
	if err != nil {
		return nil, err
	}
	s.convMu.Lock()
	s.holdIfLow(rl)
	s.convMu.Unlock()
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
