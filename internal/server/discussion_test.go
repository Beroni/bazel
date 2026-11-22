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

// discussionServer reads answer, through a fake gh, for acme/api-core#7.
func discussionServer(t *testing.T, answer string) (*Server, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "a.json")
	if err := os.WriteFile(file, []byte(answer), 0o600); err != nil {
		t.Fatal(err)
	}
	return threadsServer(t, "cat "+file)
}

// The fleet's discussion.json, in the shape discussion.jq reads: the PR and
// its head, its author on top, every thread (resolved or open, outdated too)
// with its replies, each author with type and association; long bodies cut
// and marked. It comes from the same read, and cache, as the open threads.
func TestDiscussionJSON(t *testing.T) {
	long := strings.Repeat("é", discussionBody+50)
	answer := strings.Replace(threadsAnswer, `"body":"two writers"`, `"body":"`+long+`"`, 1)
	answer = strings.Replace(answer, `"path":"a.go","line":12,`, `"path":"a.go","line":12,"diffSide":"RIGHT",`, 1)
	answer = strings.Replace(answer, `"resolvedBy":{"login":"beroni","__typename":"User"}`, `"resolvedBy":{"login":"mergify","__typename":"Bot"}`, 1)
	srv, log := discussionServer(t, answer)
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
	var d map[string]any
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	if d["repo"] != "acme/api-core" || d["pr"] != float64(7) || d["head_sha"] != headOne || d["complete"] != true {
		t.Errorf("top: %v %v %v %v", d["repo"], d["pr"], d["head_sha"], d["complete"])
	}
	if a := d["author"].(map[string]any); a["login"] != "maria" || a["type"] != "User" {
		t.Errorf("author: %v", a)
	}
	threads := d["threads"].([]any)
	if len(threads) != 4 {
		t.Fatalf("threads: %d", len(threads))
	}
	first := threads[0].(map[string]any)
	if first["url"] != "https://github.com/acme/api-core/pull/7#discussion_r1" || first["path"] != "a.go" || first["line"] != float64(12) ||
		first["start_line"] != nil || first["side"] != "RIGHT" || first["isResolved"] != false || first["isOutdated"] != false || first["resolvedBy"] != nil {
		t.Errorf("an open thread: %v", first)
	}
	asked := first["comments"].([]any)[0].(map[string]any)
	if w := asked["author"].(map[string]any); w["login"] != "beroni" || w["type"] != "User" || w["association"] != "MEMBER" || asked["createdAt"] != "2026-11-01T10:00:00Z" {
		t.Errorf("a reply: %v", asked)
	}
	reply := first["comments"].([]any)[1].(map[string]any)
	if body := reply["body"].(string); reply["cut"] != true || !strings.HasSuffix(body, " … [cut by Bazel]") || len([]rune(body)) != discussionBody+len([]rune(" … [cut by Bazel]")) {
		t.Errorf("a long body: cut=%v, %d runes", reply["cut"], len([]rune(body)))
	}
	resolved := threads[1].(map[string]any)
	if r := resolved["resolvedBy"].(map[string]any); resolved["isResolved"] != true || r["login"] != "mergify" || r["type"] != "Bot" {
		t.Errorf("a resolved thread: %v", resolved)
	}
	if old := threads[3].(map[string]any); old["isOutdated"] != true || old["line"] != float64(40) {
		t.Errorf("an outdated thread: %v", old)
	}
	for _, k := range []string{"comments", "reviews"} {
		if _, ok := d[k].([]any); !ok {
			t.Errorf("%s is not a list: %v", k, d[k])
		}
	}
}

// Past 200 threads, or 2 MB, the file keeps what fits and says it is not
// complete.
func TestDiscussionCortada(t *testing.T) {
	for name, limit := range map[string]func(){
		"threads": func() { discussionThreads = 2 },
		"bytes":   func() { discussionBytes = 600 },
	} {
		t.Run(name, func(t *testing.T) {
			oldT, oldB := discussionThreads, discussionBytes
			t.Cleanup(func() { discussionThreads, discussionBytes = oldT, oldB })
			limit()
			srv, _ := discussionServer(t, threadsAnswer)
			data, err := srv.discussionFor(context.Background(), prAt("acme/api-core", 7, headOne))
			if err != nil {
				t.Fatal(err)
			}
			var d struct {
				Complete bool  `json:"complete"`
				Threads  []any `json:"threads"`
			}
			if err := json.Unmarshal(data, &d); err != nil {
				t.Fatal(err)
			}
			if d.Complete || len(d.Threads) >= 4 || len(data) > max(discussionBytes, 600) && name == "bytes" {
				t.Errorf("complete=%v, %d threads, %d bytes", d.Complete, len(d.Threads), len(data))
			}
		})
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
