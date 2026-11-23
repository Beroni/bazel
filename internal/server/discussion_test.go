package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beroni/bazel/internal/gh"
)

// discussionServer reads answer, through a fake gh, for acme/api-core#7.
func discussionServer(t *testing.T, answer string) (*Server, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "a.json")
	if err := os.WriteFile(file, []byte(answer), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, log := threadsServer(t, "cat "+file)
	srv.fetchConversations = gh.ReadConversations
	return srv, log
}

// The fleet's discussion.json, in the shape discussion.jq reads: the PR and
// its head, its author on top, every thread (resolved or open, outdated too)
// with its replies, each author with type and association; long bodies cut
// and marked. Its own full read, apart from the tags' thin one.
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
	// Its own full read: the tags' read is too thin for the fleet.
	if n := contaNoLog(log, "start") - before; n != 1 {
		t.Errorf("the discussion read gh %d times, want its own one", n)
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
	srv.fetchConversations = func(context.Context, []string) (map[string]gh.Conversation, gh.RateLimit, error) {
		return nil, gh.RateLimit{}, errors.New("HTTP 502: Bad Gateway")
	}
	if data, err := srv.discussionFor(context.Background(), prAt("acme/api-core", 7, headOne)); err == nil || data != nil {
		t.Errorf("%s %v", data, err)
	}
}

// The review's read keeps to the tags' rules: with GitHub's points below the
// margin it reads nothing and says why, and a full read that finds them low
// holds the tags' reads too.
func TestDiscussionRespeitaORateLimit(t *testing.T) {
	low := strings.Replace(threadsAnswer, `{"data":{`, `{"data":{"rateLimit":{"cost":9,"remaining":200,"resetAt":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"},`, 1)
	srv, log := discussionServer(t, low)
	pr := prAt("acme/api-core", 7, headOne)
	if _, err := srv.discussionFor(context.Background(), pr); err != nil {
		t.Fatalf("the read that finds them low still counts: %v", err)
	}
	before := contaNoLog(log, "start")
	if data, err := srv.discussionFor(context.Background(), pr); err == nil || data != nil || !strings.Contains(err.Error(), "rate limit is low") {
		t.Errorf("%s %v", data, err)
	}
	if _, out := call(t, srv, "GET", "/api/open-threads?refresh=1", ""); !strings.Contains(fmt.Sprint(out["error"]), "rate limit is low") {
		t.Errorf("the tags read on: %v", out)
	}
	if n := contaNoLog(log, "start") - before; n != 0 {
		t.Errorf("%d gh reads below the margin", n)
	}
}

// Inside the tags' backoff the review reads nothing either.
func TestDiscussionRespeitaOBackoff(t *testing.T) {
	srv := newTestServer(t)
	srv.fetchThreadTags = func(context.Context, []string) (map[string]gh.Conversation, gh.RateLimit, error) {
		return nil, gh.RateLimit{}, errors.New("HTTP 502")
	}
	read := false
	srv.fetchConversations = func(context.Context, []string) (map[string]gh.Conversation, gh.RateLimit, error) {
		read = true
		return map[string]gh.Conversation{}, gh.RateLimit{}, nil
	}
	srv.conversationsOf(context.Background(), []string{"acme/api-core#7"}, false)
	if _, err := srv.discussionFor(context.Background(), prAt("acme/api-core", 7, headOne)); err == nil || read {
		t.Errorf("read inside the backoff: %v %v", read, err)
	}
}
