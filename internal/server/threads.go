package server

import (
	"context"
	"errors"
	"net/http"
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
	if f := s.convFailed; !f.failedAt.IsZero() && now.Sub(f.failedAt) < f.backoff() {
		return out, errors.New(f.err)
	}
	// Not the request's context: a tab closed mid-read must not fail the
	// others waiting on this one.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), detailsReadTimeout)
	defer cancel()
	for len(missing) > 0 {
		n := min(len(missing), conversationBatch)
		batch := missing[:n]
		missing = missing[n:]
		s.detMu.Lock()
		err := s.ghReadLocked(timeNow(), false)
		s.detMu.Unlock()
		if err != nil {
			return out, err
		}
		got, err := s.fetchConversations(rctx, batch)
		if err != nil {
			s.convFailed = needsCache{failedAt: timeNow(), failures: s.convFailed.failures + 1, err: err.Error()}
			return out, err
		}
		s.convFailed = needsCache{}
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
	}
	for k, e := range s.convs {
		if timeNow().Sub(e.at) >= conversationTTL {
			delete(s.convs, k)
		}
	}
	return out, nil
}

// conversationBatch is how many PRs one gh call asks about.
const conversationBatch = 10

// openThread is a thread you opened on a PR that nobody resolved yet.
type openThread struct {
	Path     string    `json:"path,omitempty"`
	Line     int       `json:"line,omitempty"`
	URL      string    `json:"url,omitempty"`
	Outdated bool      `json:"outdated,omitempty"`
	Replies  int       `json:"replies"`
	Last     *lastSaid `json:"last,omitempty"`
	// AuthorReplied says the last word is the PR author's: they answered, and
	// the thread waits on you.
	AuthorReplied bool `json:"author_replied,omitempty"`
}

type lastSaid struct {
	Author string    `json:"author"`
	Body   string    `json:"body"`
	At     time.Time `json:"at"`
}

// openThreadsOf picks, from a conversation, the threads me started that are
// still open. An outdated one is still open, and says so.
func openThreadsOf(c gh.Conversation, me string) []openThread {
	out := []openThread{}
	if me == "" {
		return out
	}
	for _, t := range c.Threads {
		if t.IsResolved || len(t.Comments) == 0 || !strings.EqualFold(t.Comments[0].Author.Login, me) {
			continue
		}
		ot := openThread{Path: t.Path, Line: t.Line, URL: t.URL, Outdated: t.IsOutdated, Replies: len(t.Comments) - 1}
		if len(t.Comments) > 1 {
			last := t.Comments[len(t.Comments)-1]
			ot.Last = &lastSaid{Author: last.Author.Login, Body: clip(last.Body, 280), At: last.At}
			ot.AuthorReplied = c.Author.Login != "" && strings.EqualFold(last.Author.Login, c.Author.Login)
		}
		out = append(out, ot)
	}
	return out
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
	threads := map[string][]openThread{}
	for k, c := range convs {
		if t := openThreadsOf(c, s.me); len(t) > 0 {
			threads[k] = t
		}
	}
	out := map[string]any{"threads": threads}
	if err != nil {
		out["error"] = "open threads could not be read: " + err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}
