package server

import (
	"context"
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
// reads them all again (the list's ↻), but not a PR inside its own backoff.
//
// A failure is a PR's or its batch's, never the whole list's: the PRs it took
// go back in the rotation and wait out their own backoff, the next batch
// still goes, and a batch that times out is split in two until the PR that
// will not answer is alone in its quarantine.
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
	var lastErr error
	for _, key := range keys {
		k := strings.ToLower(key)
		if e, ok := s.convs[k]; ok && !force && now.Sub(e.at) < conversationTTL {
			out[k] = e.conv
			continue
		}
		if f, ok := s.convFails[k]; ok && now.Sub(f.failedAt) < f.backoff() {
			lastErr = errors.New(f.err)
			continue
		}
		missing = append(missing, key)
	}
	if len(missing) == 0 {
		return out, lastErr
	}
	// The ones tried longest ago, or never, go first: with the budget spent
	// before the end of the list, the same PRs must not be the ones left out
	// every minute, and a PR that failed goes to the back like one just read.
	slices.SortStableFunc(missing, func(a, b string) int {
		return s.convTried[strings.ToLower(a)].Compare(s.convTried[strings.ToLower(b)])
	})
	if now.Before(s.convHold) {
		return out, fmt.Errorf("GitHub's rate limit is low: the open threads wait until %s", s.convHold.Local().Format("15:04"))
	}
	queue := [][]string{}
	for len(missing) > 0 {
		n := min(len(missing), gh.ConversationBatch)
		queue = append(queue, missing[:n])
		missing = missing[n:]
	}
	for len(queue) > 0 {
		batch := queue[0]
		queue = queue[1:]
		if !s.takeConvRead() {
			return out, errors.New("too many reads of the PRs' threads; the rest come next minute")
		}
		// Not the request's context: a tab closed mid-read must not fail the
		// others waiting on this one. The time limit is per call.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), convCallTimeout)
		got, rl, err := s.fetchThreadTags(rctx, batch)
		cancel()
		at := timeNow()
		for _, key := range batch {
			s.convTried[strings.ToLower(key)] = at
		}
		if err != nil {
			lastErr = err
			if timedOut(err) && len(batch) > 1 {
				half := len(batch) / 2
				queue = append([][]string{batch[:half], batch[half:]}, queue...)
				continue
			}
			for _, key := range batch {
				k := strings.ToLower(key)
				f := s.convFails[k]
				s.convFails[k] = needsCache{failedAt: at, failures: f.failures + 1, err: err.Error()}
			}
			continue
		}
		for _, key := range batch {
			k := strings.ToLower(key)
			delete(s.convFails, k)
			c, ok := got[key]
			if !ok { // GitHub did not answer for it: nothing to say, for a minute
				c = gh.Conversation{Threads: []gh.Thread{}, Comments: []gh.Said{}, Reviews: []gh.Said{}}
			}
			if c.ThreadsAfter != "" && !s.holdIfLow(rl) {
				c = s.moreThreads(ctx, key, c)
			}
			s.convs[k] = conversationEntry{conv: c, at: at}
			out[k] = c
		}
		// Past the margin, these reads stop until GitHub's hour starts again:
		// the list, Needs you, the summary and publishing keep the rest.
		if s.holdIfLow(rl) && len(queue) > 0 {
			return out, fmt.Errorf("GitHub's rate limit is low (%d left): the open threads wait until %s", rl.Remaining, s.convHold.Local().Format("15:04"))
		}
	}
	return out, lastErr
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

// moreThreads reads the rest of a PR's threads, a page a call, each from the
// same budget: past it, or past the rate limit's margin, the tags keep what
// they have, which says less than all. Call with convMu held.
func (s *Server) moreThreads(ctx context.Context, key string, c gh.Conversation) gh.Conversation {
	for c.ThreadsAfter != "" {
		if !s.takeConvRead() {
			break
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), convCallTimeout)
		page, rl, err := s.fetchThreadTagsPage(rctx, key, c.ThreadsAfter)
		cancel()
		if err != nil {
			break
		}
		c.Threads = append(c.Threads, page.Threads...)
		c.ThreadsAfter = page.ThreadsAfter
		if s.holdIfLow(rl) {
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
	convCallTimeout = detailsReadTimeout
)

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
