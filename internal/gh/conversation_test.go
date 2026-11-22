package gh

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const conversationAnswer = `{"data":{
 "p0":{"pullRequest":{
  "author":{"login":"alice","__typename":"User"},
  "reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[
   {"isResolved":false,"isOutdated":false,"path":"a.go","line":12,"originalLine":10,"startLine":null,"resolvedBy":null,
    "comments":{"totalCount":2,"nodes":[
     {"author":{"login":"me","__typename":"User"},"authorAssociation":"MEMBER","body":"why?","url":"https://github.com/o/r/pull/7#discussion_r1","createdAt":"2026-11-01T10:00:00Z"},
     {"author":{"login":"alice","__typename":"User"},"authorAssociation":"CONTRIBUTOR","body":"fixed","url":"https://github.com/o/r/pull/7#discussion_r2","createdAt":"2026-11-02T10:00:00Z"}]},
    "lastComment":{"nodes":[{"author":{"login":"carol","__typename":"User"},"authorAssociation":"MEMBER","body":"the real last","url":"https://github.com/o/r/pull/7#discussion_r9","createdAt":"2026-11-05T10:00:00Z"}]}},
   {"isResolved":true,"isOutdated":true,"path":"b.go","line":null,"originalLine":3,"startLine":1,"resolvedBy":{"login":"renovate","__typename":"Bot"},
    "comments":{"totalCount":60,"nodes":[{"author":null,"authorAssociation":"NONE","body":"ghost","url":"u","createdAt":"2026-11-01T10:00:00Z"}]}}]},
  "comments":{"pageInfo":{"hasNextPage":false},"nodes":[{"author":{"login":"bob","__typename":"User"},"authorAssociation":"OWNER","body":"lgtm","url":"c1","createdAt":"2026-11-03T10:00:00Z"}]},
  "reviews":{"pageInfo":{"hasNextPage":false},"nodes":[{"author":{"login":"bob","__typename":"User"},"authorAssociation":"OWNER","state":"APPROVED","body":"ok","url":"r1","submittedAt":"2026-11-03T11:00:00Z"}]}}},
 "p1":{"pullRequest":null}}}`

func TestConversaLida(t *testing.T) {
	got, _, err := parseConversations(conversationAnswer, []string{"o/r#7", "o/r#99"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["o/r#99"]; ok {
		t.Error("a PR GitHub did not answer for is in the map")
	}
	c := got["o/r#7"]
	if c.Author.Login != "alice" || len(c.Threads) != 2 || len(c.Comments) != 1 || len(c.Reviews) != 1 {
		t.Fatalf("%+v", c)
	}
	if c.Complete {
		t.Error("a thread with 60 replies, 1 read, is not complete")
	}
	open := c.Threads[0]
	if open.IsResolved || open.Path != "a.go" || open.Line != 12 || open.URL != "https://github.com/o/r/pull/7#discussion_r1" ||
		open.Comments[0].Author.Association != "MEMBER" || open.Comments[1].Author.Login != "alice" {
		t.Errorf("open thread: %+v", open)
	}
	if open.Last == nil || open.Last.Body != "the real last" || open.Total != 2 {
		t.Errorf("the last word is read on its own: %+v", open.Last)
	}
	old := c.Threads[1]
	if old.Total != 60 || old.Last == nil {
		t.Errorf("without lastComment, the last read stands: %d %+v", old.Total, old.Last)
	}
	if !old.IsOutdated || old.Line != 3 || old.StartLine != 1 || old.ResolvedBy == nil || !old.ResolvedBy.IsBot() || old.Comments[0].Author.Login != "" {
		t.Errorf("outdated thread: %+v", old)
	}
	if c.Reviews[0].State != "APPROVED" || c.Reviews[0].At.IsZero() {
		t.Errorf("review: %+v", c.Reviews[0])
	}
}

func TestConversaEmLoteSoLeitura(t *testing.T) {
	answer := filepath.Join(t.TempDir(), "a.json")
	if err := os.WriteFile(answer, []byte(conversationAnswer), 0o600); err != nil {
		t.Fatal(err)
	}
	log := fakeGH(t, "cat "+answer)
	got, err := FetchConversations(context.Background(), []string{"o/r#7", "o/r#99", "bad key"})
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	data, _ := os.ReadFile(log)
	call := string(data)
	if n := strings.Count(call, "api graphql"); n != 1 {
		t.Errorf("%d gh calls", n)
	}
	for _, a := range []string{"query bazelConversation(", "-f o0=o", "-F p0=7", "-F p1=99"} {
		if !strings.Contains(call, a) {
			t.Errorf("missing %q", a)
		}
	}
	if strings.Contains(call, "mutation") || strings.Contains(call, "bad key") {
		t.Errorf("not a read, or a bad key went in: %s", call)
	}
	fakeGH(t, "echo 'HTTP 502' >&2; exit 1")
	if _, err := FetchConversations(context.Background(), []string{"o/r#7"}); err == nil {
		t.Error("gh failed and FetchConversations said nothing")
	}
}

// The tags' read is the least query that tells whose threads are open, and it
// asks GitHub what it cost and what is left.
func TestThreadTagsLeituraMinima(t *testing.T) {
	q := threadTagsQuery(2)
	for _, want := range []string{"query bazelThreadTags(", "comments(first: 1)", "lastComment: comments(last: 1)", "rateLimit { cost remaining resetAt }", "isResolved", "isOutdated", "path", "line"} {
		if !strings.Contains(q, want) {
			t.Errorf("no %q in:\n%s", want, q)
		}
	}
	for _, not := range []string{"comments(first: 50)", "comments(first: 100)", "reviews(", "authorAssociation", "resolvedBy"} {
		if strings.Contains(q, not) {
			t.Errorf("the tags' query asks for %q", not)
		}
	}
	answer := filepath.Join(t.TempDir(), "a.json")
	if err := os.WriteFile(answer, []byte(`{"data":{"rateLimit":{"cost":3,"remaining":812,"resetAt":"2026-11-22T20:00:00Z"},
	 "p0":{"pullRequest":{"author":{"login":"alice","__typename":"User"},"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[
	  {"isResolved":false,"isOutdated":false,"path":"a.go","line":3,"comments":{"totalCount":4,"nodes":[{"author":{"login":"me","__typename":"User"},"url":"https://github.com/o/r/pull/7#discussion_r1","createdAt":"2026-11-01T10:00:00Z"}]},
	   "lastComment":{"nodes":[{"author":{"login":"alice","__typename":"User"},"body":"done","createdAt":"2026-11-02T10:00:00Z"}]}}]}}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeGH(t, "cat "+answer)
	got, rl, err := FetchThreadTags(context.Background(), []string{"o/r#7", "bad"})
	if err != nil {
		t.Fatal(err)
	}
	if rl.Remaining != 812 || rl.Cost != 3 || rl.ResetAt.IsZero() {
		t.Errorf("rate limit: %+v", rl)
	}
	th := got["o/r#7"].Threads[0]
	if th.Total != 4 || th.Last == nil || th.Last.Author.Login != "alice" || th.Comments[0].Author.Login != "me" || th.URL == "" {
		t.Errorf("%+v %+v", th, th.Last)
	}
}

// What the tags' read costs, by GitHub's formula: a connection counts once per
// node its parents may bring, a call is that sum over 100. A thread needs two
// children (the first comment says who opened it, the last is the last word),
// so a PR is 1 + 2×threads requests. A batch of ten:
//
//	before: reviewThreads(first: 100) → 10 × (1 + 200) = 2010 → 20 points
//	now:    reviewThreads(first: 50)  → 10 × (1 + 100) = 1010 → 10 points
//
// A PR past 50 threads pays its next pages one call at a time.
func TestTagsCustamMetade(t *testing.T) {
	if got := TagsCost(ConversationBatch, 100); got != 20 {
		t.Errorf("before: %d points", got)
	}
	if got := TagsCost(ConversationBatch, ThreadTagsPage); got != 10 {
		t.Errorf("now: %d points", got)
	}
	if q := threadTagsQuery(1); !strings.Contains(q, "reviewThreads(first: 50)") || !strings.Contains(q, "endCursor") {
		t.Errorf("the tags' query: %s", q)
	}
	if q := threadTagsPageQuery(); !strings.Contains(q, "query bazelThreadTagsPage(") || !strings.Contains(q, "after: $after") || !strings.Contains(q, "rateLimit") {
		t.Errorf("the page query: %s", q)
	}
}

// A rate limit with no resetAt it can read still says what is left; one with
// no remaining is not known.
func TestRateLimitMalFormado(t *testing.T) {
	if rl := parseRateLimit([]byte(`{"cost":1,"remaining":12,"resetAt":"soon"}`)); !rl.Known || rl.Remaining != 12 || !rl.ResetAt.IsZero() {
		t.Errorf("%+v", rl)
	}
	if rl := parseRateLimit([]byte(`{"cost":1}`)); rl.Known {
		t.Errorf("%+v", rl)
	}
	if rl := parseRateLimit(nil); rl.Known {
		t.Errorf("%+v", rl)
	}
}
