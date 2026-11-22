package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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

// L-OT-1: a thread longer than one read: the last word is read on its own, so
// the 50th reply, the author's, is not taken for it; and the read says how many
// replies it did not bring.
func TestThreadLongaUsaAUltimaDeVerdade(t *testing.T) {
	c := gh.Conversation{Author: gh.Who{Login: "maria"}, Threads: []gh.Thread{{
		Path: "a.go", Line: 1, URL: "https://github.com/acme/api-core/pull/7#discussion_r1", Total: 51,
		Comments: []gh.Said{{Author: gh.Who{Login: "beroni"}, Body: "why?"}, {Author: gh.Who{Login: "maria"}, Body: "the 50th"}},
		Last:     &gh.Said{Author: gh.Who{Login: "bob"}, Body: "the 51st"},
	}}}
	got := openThreadsOf(c, "beroni", "acme/api-core#7")
	if len(got) != 1 {
		t.Fatalf("%+v", got)
	}
	if ot := got[0]; ot.AuthorReplied || ot.Last == nil || ot.Last.Body != "the 51st" || ot.Replies != 50 || ot.More != 49 {
		t.Errorf("%+v %+v", ot, ot.Last)
	}
}

// O-OT-2: on your own PR, "author replied" is you: not flagged.
func TestNoProprioPRNaoHaAutorQueRespondeu(t *testing.T) {
	c := gh.Conversation{Author: gh.Who{Login: "Beroni"}, Threads: []gh.Thread{{
		Total: 2, Comments: []gh.Said{{Author: gh.Who{Login: "beroni"}, Body: "note"}, {Author: gh.Who{Login: "beroni"}, Body: "done"}},
	}}}
	if got := openThreadsOf(c, "beroni", "acme/api-core#7"); len(got) != 1 || got[0].AuthorReplied {
		t.Errorf("%+v", got)
	}
}

// O-OT-3: a link the page draws goes to this PR on GitHub, or there is none.
func TestLinkSoParaOProprioPR(t *testing.T) {
	for u, keep := range map[string]bool{
		"https://github.com/acme/api-core/pull/7#discussion_r1":        true,
		"https://github.com/Acme/Api-Core/pull/7/files#r2":             true,
		"https://github.com/acme/api-core/pull/7":                      true,
		"https://github.com/acme/api-core/pull/70#discussion_r":        false,
		"https://github.com/evil/api-core/pull/7#discussion_r1":        false,
		"javascript:alert(1)//https://github.com/acme/api-core/pull/7": false,
		"https://github.com.evil.example/acme/api-core/pull/7":         false,
		"": false,
	} {
		c := gh.Conversation{Threads: []gh.Thread{{URL: u, Comments: []gh.Said{{Author: gh.Who{Login: "beroni"}}}}}}
		got := openThreadsOf(c, "beroni", "acme/api-core#7")[0].URL
		if keep && got != u || !keep && got != "" {
			t.Errorf("%q → %q", u, got)
		}
	}
}

// L-OT-3: the backoff only ends when every call of a read went well: a batch
// that works must not reset the one that keeps failing, so the wait doubles.
func TestBackoffPorLote(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now()
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = time.Now })
	srv.fetchConversations = func(_ context.Context, keys []string) (map[string]gh.Conversation, error) {
		if slices.Contains(keys, fmt.Sprintf("acme/api-core#%d", gh.ConversationBatch+1)) {
			return nil, errors.New("HTTP 502")
		}
		return map[string]gh.Conversation{}, nil
	}
	keys := make([]string, gh.ConversationBatch+1)
	for i := range keys {
		keys[i] = fmt.Sprintf("acme/api-core#%d", i+1)
	}
	srv.conversationsOf(context.Background(), keys, false)
	now = now.Add(2 * time.Minute) // past the backoff and the cache
	srv.conversationsOf(context.Background(), keys, false)
	if f := srv.convFailed.failures; f != 2 {
		t.Errorf("failures in a row: %d, want 2", f)
	}
}

// O-OT-1: the tags' reads have a ceiling of their own and never spend the
// Overview's budget; past it, the rest wait for the next minute.
func TestThreadsTemOrcamentoProprio(t *testing.T) {
	srv := newTestServer(t)
	old := convReadsPerMin
	convReadsPerMin = 1
	t.Cleanup(func() { convReadsPerMin = old })
	var calls atomic.Int32
	srv.fetchConversations = func(context.Context, []string) (map[string]gh.Conversation, error) {
		calls.Add(1)
		return map[string]gh.Conversation{}, nil
	}
	keys := make([]string, gh.ConversationBatch+1)
	for i := range keys {
		keys[i] = fmt.Sprintf("acme/api-core#%d", i+1)
	}
	got, err := srv.conversationsOf(context.Background(), keys, false)
	if err == nil || calls.Load() != 1 || len(got) != gh.ConversationBatch {
		t.Errorf("%d calls, %d read, err %v", calls.Load(), len(got), err)
	}
	srv.detMu.Lock()
	spent := len(srv.ghReads)
	srv.detMu.Unlock()
	if spent != 0 {
		t.Errorf("the Overview's budget paid %d", spent)
	}
}

// BUG-QA-40: a read that fails after one that worked keeps what that one said,
// marked stale with its time, as Needs you keeps its last list.
func TestFalhaMantemAUltimaLeituraBoa(t *testing.T) {
	answer := filepath.Join(t.TempDir(), "a.json")
	if err := os.WriteFile(answer, []byte(threadsAnswer), 0o600); err != nil {
		t.Fatal(err)
	}
	fail := filepath.Join(t.TempDir(), "fail")
	srv, _ := threadsServer(t, "if [ -f "+fail+" ]; then echo 'HTTP 502' >&2; exit 1; fi; cat "+answer)
	now := time.Now()
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = time.Now })
	if _, out := call(t, srv, "GET", "/api/open-threads", ""); len(out["threads"].(map[string]any)) != 1 {
		t.Fatalf("the good read: %v", out)
	}
	if err := os.WriteFile(fail, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	read := now
	now = now.Add(2 * time.Minute) // past the minute: a new read, and it fails
	_, out := call(t, srv, "GET", "/api/open-threads?refresh=1", "")
	if len(out["threads"].(map[string]any)) != 1 || out["stale"] != true || out["error"] == nil {
		t.Fatalf("a failed read wiped the last good one: %v", out)
	}
	if at, _ := time.Parse(time.RFC3339Nano, out["stale_at"].(string)); !at.Equal(read) {
		t.Errorf("stale_at %v, the good read was at %v", out["stale_at"], read)
	}
	// Inside the backoff too.
	if _, out := call(t, srv, "GET", "/api/open-threads", ""); len(out["threads"].(map[string]any)) != 1 || out["stale"] != true {
		t.Errorf("in the backoff: %v", out)
	}
}

// The reads go gh.ConversationBatch PRs a call: one past it is a second call.
func TestThreadsEmLotesDoTamanhoDoGh(t *testing.T) {
	srv := newTestServer(t)
	var calls atomic.Int32
	srv.fetchConversations = func(_ context.Context, keys []string) (map[string]gh.Conversation, error) {
		calls.Add(1)
		if len(keys) > gh.ConversationBatch {
			t.Errorf("one call for %d PRs", len(keys))
		}
		return map[string]gh.Conversation{}, nil
	}
	keys := make([]string, gh.ConversationBatch+1)
	for i := range keys {
		keys[i] = fmt.Sprintf("acme/api-core#%d", i+1)
	}
	srv.conversationsOf(context.Background(), keys, false)
	if calls.Load() != 2 {
		t.Errorf("%d calls", calls.Load())
	}
}
