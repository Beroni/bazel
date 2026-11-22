package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beroni/bazel/internal/gh"
)

// One PR's conversation as gh answers it: a thread of mine still open, with
// the author's answer last; one of mine resolved; one of someone else's; and
// one of mine that is outdated and open.
const threadsAnswer = `{"data":{"p0":{"pullRequest":{
 "author":{"login":"maria","__typename":"User"},
 "reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[
  {"isResolved":false,"isOutdated":false,"path":"a.go","line":12,"resolvedBy":null,
   "comments":{"totalCount":2,"nodes":[
    {"author":{"login":"beroni","__typename":"User"},"authorAssociation":"MEMBER","body":"why a lock?","url":"https://github.com/acme/api-core/pull/7#discussion_r1","createdAt":"2026-11-01T10:00:00Z"},
    {"author":{"login":"maria","__typename":"User"},"authorAssociation":"CONTRIBUTOR","body":"two writers","url":"https://github.com/acme/api-core/pull/7#discussion_r2","createdAt":"2026-11-02T10:00:00Z"}]}},
  {"isResolved":true,"isOutdated":false,"path":"b.go","line":3,"resolvedBy":{"login":"beroni","__typename":"User"},
   "comments":{"totalCount":1,"nodes":[{"author":{"login":"beroni","__typename":"User"},"authorAssociation":"MEMBER","body":"done?","url":"u2","createdAt":"2026-11-01T10:00:00Z"}]}},
  {"isResolved":false,"isOutdated":false,"path":"c.go","line":9,"resolvedBy":null,
   "comments":{"totalCount":1,"nodes":[{"author":{"login":"bob","__typename":"User"},"authorAssociation":"MEMBER","body":"nit","url":"u3","createdAt":"2026-11-01T10:00:00Z"}]}},
  {"isResolved":false,"isOutdated":true,"path":"d.go","line":null,"originalLine":40,"resolvedBy":null,
   "comments":{"totalCount":1,"nodes":[{"author":{"login":"BERONI","__typename":"User"},"authorAssociation":"MEMBER","body":"still?","url":"https://github.com/acme/api-core/pull/7#discussion_r4","createdAt":"2026-11-01T10:00:00Z"}]}}]},
 "comments":{"pageInfo":{"hasNextPage":false},"nodes":[]},
 "reviews":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}}`

// threadsServer lists one PR, and reads its conversation through a fake gh.
func threadsServer(t *testing.T, script string) (*Server, string) {
	t.Helper()
	log := ghFalso(t, script)
	srv := newTestServer(t)
	srv.fetchConversations = gh.FetchConversations
	listAt(srv, time.Now(), prAt("acme/api-core", 7, headOne))
	return srv, log
}

// Only my threads still open come back: not resolved, not someone else's. An
// outdated one is still open, and says so. The author's reply is the last word.
func TestThreadsAbertasSoAsMinhas(t *testing.T) {
	answer := filepath.Join(t.TempDir(), "a.json")
	if err := os.WriteFile(answer, []byte(threadsAnswer), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, log := threadsServer(t, "cat "+answer)
	code, out := call(t, srv, "GET", "/api/open-threads", "")
	if code != http.StatusOK || out["error"] != nil {
		t.Fatalf("%d %v", code, out)
	}
	list, _ := out["threads"].(map[string]any)["acme/api-core#7"].([]any)
	if len(list) != 2 {
		t.Fatalf("threads: %v", out["threads"])
	}
	first := list[0].(map[string]any)
	if first["path"] != "a.go" || first["line"] != float64(12) || first["author_replied"] != true ||
		first["url"] != "https://github.com/acme/api-core/pull/7#discussion_r1" || first["last"].(map[string]any)["body"] != "two writers" {
		t.Errorf("first: %v", first)
	}
	second := list[1].(map[string]any)
	if second["path"] != "d.go" || second["line"] != float64(40) || second["outdated"] != true || second["author_replied"] != nil {
		t.Errorf("outdated: %v", second)
	}
	before := contaNoLog(log, "start")
	call(t, srv, "GET", "/api/open-threads", "")
	if n := contaNoLog(log, "start") - before; n != 0 {
		t.Errorf("read again within the minute: %d", n)
	}
	call(t, srv, "GET", "/api/open-threads?refresh=1", "")
	if n := contaNoLog(log, "start") - before; n != 1 {
		t.Errorf("the list's refresh reads them again: %d reads", n)
	}
	if data, _ := os.ReadFile(log); strings.Contains(string(data), "mutation") {
		t.Error("a write went to gh")
	}
}

// gh failing blocks nothing: no threads, the reason, and the backoff holds the
// next read back.
func TestThreadsComGhFalhando(t *testing.T) {
	srv, log := threadsServer(t, "echo 'HTTP 502: Bad Gateway' >&2; exit 1")
	for range 3 {
		code, out := call(t, srv, "GET", "/api/open-threads", "")
		if code != http.StatusOK || !strings.HasPrefix(out["error"].(string), "open threads could not be read") || len(out["threads"].(map[string]any)) != 0 {
			t.Errorf("%d %v", code, out)
		}
	}
	if n := contaNoLog(log, "start"); n != 1 {
		t.Errorf("%d gh reads inside the backoff, want 1", n)
	}
}

// Eight readers at once, one read.
func TestThreadsUmaLeitura(t *testing.T) {
	srv := newTestServer(t)
	var reads atomic.Int32
	release := make(chan struct{})
	srv.fetchConversations = func(_ context.Context, keys []string) (map[string]gh.Conversation, error) {
		reads.Add(1)
		<-release
		return map[string]gh.Conversation{}, nil
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := srv.conversationsOf(context.Background(), []string{"acme/api-core#7"}, false); err != nil {
				t.Error(err)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := reads.Load(); n != 1 {
		t.Errorf("%d reads", n)
	}
}
