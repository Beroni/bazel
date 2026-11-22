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
	got, err := parseConversations(conversationAnswer, []string{"o/r#7", "o/r#99"})
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
