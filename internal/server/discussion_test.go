package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beroni/bazel/internal/gh"
)

// The fleet's discussion.json: every thread, resolved or open, with its
// replies; the comments and reviews; the PR's author on top; long bodies cut
// and marked. It comes from the same read, and cache, as the open threads.
func TestDiscussionJSON(t *testing.T) {
	long := strings.Repeat("é", discussionBody+50)
	answer := strings.Replace(threadsAnswer, `"body":"two writers"`, `"body":"`+long+`"`, 1)
	file := filepath.Join(t.TempDir(), "a.json")
	if err := os.WriteFile(file, []byte(answer), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, log := threadsServer(t, "cat "+file)
	if srv.jobs.runner.Discussion == nil {
		t.Fatal("the reviews get no reader")
	}
	call(t, srv, "GET", "/api/open-threads", "")
	before := contaNoLog(log, "start")

	data, err := srv.discussionFor(context.Background(), prAt("acme/api-core", 7, headOne))
	if err != nil {
		t.Fatal(err)
	}
	if n := contaNoLog(log, "start") - before; n != 0 {
		t.Errorf("the discussion read gh again (%d): it shares the open threads' read", n)
	}
	var d struct {
		Author   gh.Who      `json:"author"`
		Complete bool        `json:"complete"`
		Threads  []gh.Thread `json:"threads"`
		Comments []gh.Said   `json:"comments"`
		Reviews  []gh.Said   `json:"reviews"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Author.Login != "maria" || !d.Complete || len(d.Threads) != 4 || d.Comments == nil || d.Reviews == nil {
		t.Fatalf("%+v", d)
	}
	if !d.Threads[1].IsResolved || d.Threads[1].ResolvedBy.Login != "beroni" || !d.Threads[3].IsOutdated {
		t.Errorf("resolved and outdated threads: %+v", d.Threads)
	}
	reply := d.Threads[0].Comments[1]
	if !reply.Cut || !strings.HasSuffix(reply.Body, " … [cut by Bazel]") || len([]rune(reply.Body)) != discussionBody+len([]rune(" … [cut by Bazel]")) {
		t.Errorf("a long body: cut=%v, %d runes", reply.Cut, len([]rune(reply.Body)))
	}
	if first := d.Threads[0].Comments[0]; first.Cut || first.Author.Association != "MEMBER" || first.URL == "" {
		t.Errorf("a short body: %+v", first)
	}
	if !strings.Contains(string(data), `"association": "MEMBER"`) {
		t.Error("the association goes inside author, where the fleet reads it")
	}
}

// A failed read is an error and no file.
func TestDiscussionComFalha(t *testing.T) {
	srv := newTestServer(t)
	srv.fetchConversations = func(context.Context, []string) (map[string]gh.Conversation, error) {
		return nil, errors.New("HTTP 502: Bad Gateway")
	}
	if data, err := srv.discussionFor(context.Background(), prAt("acme/api-core", 7, headOne)); err == nil || data != nil {
		t.Errorf("%s %v", data, err)
	}
}
