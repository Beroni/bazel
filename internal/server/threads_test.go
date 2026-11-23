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
	srv.fetchThreadTags = gh.FetchThreadTags
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
	srv.fetchThreadTags = func(_ context.Context, keys []string) (map[string]gh.Conversation, gh.RateLimit, error) {
		reads.Add(1)
		<-release
		return map[string]gh.Conversation{}, gh.RateLimit{}, nil
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The others do not wait: they answer at once from what there is.
			srv.conversationsOf(context.Background(), []string{"acme/api-core#7"}, false)
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
	srv.fetchThreadTags = func(_ context.Context, keys []string) (map[string]gh.Conversation, gh.RateLimit, error) {
		if slices.Contains(keys, fmt.Sprintf("acme/api-core#%d", gh.ConversationBatch+1)) {
			return nil, gh.RateLimit{}, errors.New("HTTP 502")
		}
		return map[string]gh.Conversation{}, gh.RateLimit{}, nil
	}
	keys := make([]string, gh.ConversationBatch+1)
	for i := range keys {
		keys[i] = fmt.Sprintf("acme/api-core#%d", i+1)
	}
	srv.conversationsOf(context.Background(), keys, false)
	now = now.Add(2 * time.Minute) // past the backoff and the cache
	srv.conversationsOf(context.Background(), keys, false)
	bad := fmt.Sprintf("acme/api-core#%d", gh.ConversationBatch+1)
	if f := srv.convFails[bad].failures; f != 2 {
		t.Errorf("failures in a row of the batch that fails: %d, want 2", f)
	}
	if f, ok := srv.convFails["acme/api-core#1"]; ok {
		t.Errorf("the batch that worked carries a failure: %+v", f)
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
	srv.fetchThreadTags = func(context.Context, []string) (map[string]gh.Conversation, gh.RateLimit, error) {
		calls.Add(1)
		return map[string]gh.Conversation{}, gh.RateLimit{}, nil
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
	stale, _ := out["stale"].(map[string]any)
	if len(out["threads"].(map[string]any)) != 1 || stale == nil || out["errors"] == nil {
		t.Fatalf("a failed read wiped the last good one: %v", out)
	}
	if at, _ := time.Parse(time.RFC3339Nano, stale["acme/api-core#7"].(string)); !at.Equal(read) {
		t.Errorf("stale at %v, the good read was at %v", stale["acme/api-core#7"], read)
	}
	if out["error"] != nil {
		t.Errorf("the PR still shows what it said: no global error, %v", out["error"])
	}
	// Inside the backoff too.
	if _, out := call(t, srv, "GET", "/api/open-threads", ""); len(out["threads"].(map[string]any)) != 1 || out["stale"] == nil {
		t.Errorf("in the backoff: %v", out)
	}
}

// The reads go gh.ConversationBatch PRs a call: one past it is a second call.
func TestThreadsEmLotesDoTamanhoDoGh(t *testing.T) {
	srv := newTestServer(t)
	var calls atomic.Int32
	srv.fetchThreadTags = func(_ context.Context, keys []string) (map[string]gh.Conversation, gh.RateLimit, error) {
		calls.Add(1)
		if len(keys) > gh.ConversationBatch {
			t.Errorf("one call for %d PRs", len(keys))
		}
		return map[string]gh.Conversation{}, gh.RateLimit{}, nil
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

// OT2-1: GitHub's points, not only calls. Once a read says few are left, the
// tags' reads stop until the reset, as a failure with the last good tags
// stale; Needs you, which shares GitHub's hour, still reads.
func TestRateLimitBaixoParaAsEtiquetas(t *testing.T) {
	low := strings.Replace(threadsAnswer, `{"data":{`, `{"data":{"rateLimit":{"cost":5,"remaining":400,"resetAt":"`+time.Now().Add(30*time.Minute).UTC().Format(time.RFC3339)+`"},`, 1)
	answer := filepath.Join(t.TempDir(), "a.json")
	if err := os.WriteFile(answer, []byte(low), 0o600); err != nil {
		t.Fatal(err)
	}
	needs := `{"data":{"requested":{"issueCount":0,"nodes":[]},"reviewed":{"issueCount":0,"nodes":[]},"authored":{"issueCount":0,"nodes":[]}}}`
	srv, log := threadsServer(t, `case "$*" in *bazelNeedsYou*) echo '`+needs+`' ;; *) cat `+answer+` ;; esac`)
	_, out := call(t, srv, "GET", "/api/open-threads", "")
	if len(out["threads"].(map[string]any)) != 1 {
		t.Fatalf("the read that said so still counts: %v", out)
	}
	before := contaNoLog(log, "start")
	_, out = call(t, srv, "GET", "/api/open-threads?refresh=1", "")
	if n := contaNoLog(log, "start") - before; n != 0 {
		t.Errorf("read the tags %d times with the limit low", n)
	}
	errs, _ := out["errors"].(map[string]any)
	if e, _ := errs["acme/api-core#7"].(string); !strings.Contains(e, "rate limit is low") || out["stale"] == nil || len(out["threads"].(map[string]any)) != 1 {
		t.Errorf("%v", out)
	}
	if code, n := call(t, srv, "GET", "/api/needs", ""); code != http.StatusOK || contaNoLog(log, "start")-before != 1 {
		t.Errorf("Needs you did not read: %d %v", code, n)
	}
}

// OT2-1 (Luna): with the budget and the minute, the PRs read longest ago, or
// never, go first: 210 PRs, a few minutes, every one of them read.
func TestRodizioLeTodos(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now()
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = time.Now })
	read := map[string]bool{}
	srv.fetchThreadTags = func(_ context.Context, keys []string) (map[string]gh.Conversation, gh.RateLimit, error) {
		for _, k := range keys {
			read[k] = true
		}
		return map[string]gh.Conversation{}, gh.RateLimit{}, nil
	}
	keys := make([]string, 210)
	for i := range keys {
		keys[i] = fmt.Sprintf("acme/api-core#%d", i+1)
	}
	for range 3 {
		srv.conversationsOf(context.Background(), keys, false)
		now = now.Add(61 * time.Second)
	}
	if len(read) != len(keys) {
		t.Errorf("%d of %d PRs ever read", len(read), len(keys))
	}
}

// OT2-3: past its 30 minutes, a good read is no fallback any more: no tags, and
// the reason.
func TestFallbackExpira(t *testing.T) {
	answer := filepath.Join(t.TempDir(), "a.json")
	if err := os.WriteFile(answer, []byte(threadsAnswer), 0o600); err != nil {
		t.Fatal(err)
	}
	fail := filepath.Join(t.TempDir(), "fail")
	srv, _ := threadsServer(t, "if [ -f "+fail+" ]; then echo 'HTTP 502' >&2; exit 1; fi; cat "+answer)
	now := time.Now()
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = time.Now })
	call(t, srv, "GET", "/api/open-threads", "")
	if err := os.WriteFile(fail, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, later := range []time.Duration{31 * time.Minute, 24 * time.Hour} {
		now = now.Add(later)
		_, out := call(t, srv, "GET", "/api/open-threads?refresh=1", "")
		if len(out["threads"].(map[string]any)) != 0 || out["stale"] != nil || out["error"] == nil {
			t.Errorf("after %v: %v", later, out)
		}
	}
}

// OT3-1: a batch that always fails takes only its own PRs out: they go to the
// back of the rotation and wait their own backoff, and the healthy PR after
// them is read.
func TestLoteQueFalhaNaoSeguraOsOutros(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now()
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = time.Now })
	healthy := fmt.Sprintf("acme/api-core#%d", gh.ConversationBatch+1)
	read := map[string]int{}
	srv.fetchThreadTags = func(_ context.Context, keys []string) (map[string]gh.Conversation, gh.RateLimit, error) {
		if slices.Contains(keys, "acme/api-core#1") {
			return nil, gh.RateLimit{}, errors.New("HTTP 502")
		}
		for _, k := range keys {
			read[k]++
		}
		return map[string]gh.Conversation{}, gh.RateLimit{}, nil
	}
	keys := make([]string, gh.ConversationBatch+1)
	for i := range keys {
		keys[i] = fmt.Sprintf("acme/api-core#%d", i+1)
	}
	srv.conversationsOf(context.Background(), keys, false)
	if read[healthy] != 1 {
		t.Errorf("the healthy PR was read %d times in the first round", read[healthy])
	}
	now = now.Add(61 * time.Second)
	srv.conversationsOf(context.Background(), keys, false)
	if read[healthy] != 2 {
		t.Errorf("the healthy PR was read %d times after the next round", read[healthy])
	}
}

// A batch that times out is split in two until the PR that will not answer is
// alone, in a quarantine of its own; every other PR is read.
func TestTimeoutDivideOLote(t *testing.T) {
	srv := newTestServer(t)
	calls := 0
	read := map[string]bool{}
	srv.fetchThreadTags = func(_ context.Context, keys []string) (map[string]gh.Conversation, gh.RateLimit, error) {
		calls++
		if slices.Contains(keys, "acme/api-core#3") {
			return nil, gh.RateLimit{}, context.DeadlineExceeded
		}
		for _, k := range keys {
			read[k] = true
		}
		return map[string]gh.Conversation{}, gh.RateLimit{}, nil
	}
	keys := make([]string, gh.ConversationBatch)
	for i := range keys {
		keys[i] = fmt.Sprintf("acme/api-core#%d", i+1)
	}
	srv.conversationsOf(context.Background(), keys, false)
	if len(read) != gh.ConversationBatch-1 || read["acme/api-core#3"] {
		t.Errorf("read %d of %d: %v", len(read), gh.ConversationBatch-1, read)
	}
	if f := srv.convFails["acme/api-core#3"]; f.failures != 1 || len(srv.convFails) != 1 {
		t.Errorf("quarantine: %+v", srv.convFails)
	}
	if calls > 8 {
		t.Errorf("%d calls to isolate one PR in %d", calls, gh.ConversationBatch)
	}
}

// OT3-2: how long the hold lasts: what GitHub says, between a minute and an
// hour; five minutes when its reset time is gone, unreadable, or already past
// by this clock.
func TestTravaComResetAtLimitado(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now()
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = time.Now })
	for name, c := range map[string]struct {
		rl   gh.RateLimit
		wait time.Duration
	}{
		"in 20 minutes":       {gh.RateLimit{Known: true, Remaining: 10, ResetAt: now.Add(20 * time.Minute)}, 20 * time.Minute},
		"in 5 seconds":        {gh.RateLimit{Known: true, Remaining: 10, ResetAt: now.Add(5 * time.Second)}, time.Minute},
		"in 3 hours":          {gh.RateLimit{Known: true, Remaining: 10, ResetAt: now.Add(3 * time.Hour)}, time.Hour},
		"in the past":         {gh.RateLimit{Known: true, Remaining: 10, ResetAt: now.Add(-time.Minute)}, 5 * time.Minute},
		"unreadable or none":  {gh.RateLimit{Known: true, Remaining: 10}, 5 * time.Minute},
		"plenty left":         {gh.RateLimit{Known: true, Remaining: 4000, ResetAt: now.Add(time.Hour)}, 0},
		"no rate limit given": {gh.RateLimit{}, 0},
	} {
		srv.convHold = time.Time{}
		srv.holdIfLow(c.rl)
		got := time.Duration(0)
		if !srv.convHold.IsZero() {
			got = srv.convHold.Sub(now)
		}
		if got != c.wait {
			t.Errorf("%s: held %v, want %v", name, got, c.wait)
		}
	}
}

// A PR past one page of threads reads the rest page by page, each a call of
// the same budget.
func TestThreadsPaginaSoAquelePR(t *testing.T) {
	srv := newTestServer(t)
	mine := func(path string) gh.Thread {
		return gh.Thread{Path: path, URL: "https://github.com/acme/api-core/pull/7#d", Comments: []gh.Said{{Author: gh.Who{Login: "beroni"}}}}
	}
	srv.fetchThreadTags = func(_ context.Context, keys []string) (map[string]gh.Conversation, gh.RateLimit, error) {
		return map[string]gh.Conversation{"acme/api-core#7": {Threads: []gh.Thread{mine("a.go")}, ThreadsAfter: "c1"}}, gh.RateLimit{}, nil
	}
	var pages []string
	srv.fetchThreadTagsPage = func(_ context.Context, key, after string) (gh.Conversation, gh.RateLimit, error) {
		pages = append(pages, key+"@"+after)
		next := ""
		if after == "c1" {
			next = "c2"
		}
		return gh.Conversation{Threads: []gh.Thread{mine("p" + after + ".go")}, ThreadsAfter: next}, gh.RateLimit{}, nil
	}
	got, err := srv.conversationsOf(context.Background(), []string{"acme/api-core#7", "acme/api-core#8"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(got["acme/api-core#7"].Threads); n != 3 || !slices.Equal(pages, []string{"acme/api-core#7@c1", "acme/api-core#7@c2"}) {
		t.Errorf("%d threads, pages %v", n, pages)
	}
	if n := len(srv.convReads); n != 3 {
		t.Errorf("%d calls charged, want 3", n)
	}
}

// Q1: a PR that always times out holds nobody: while its call is out, the
// page's reads answer at once from what there is; and once it was isolated,
// it is read alone, never in a batch with good PRs again.
func TestTimeoutNaoSeguraAPagina(t *testing.T) {
	srv := newTestServer(t)
	old := convCallTimeout
	convCallTimeout = 2 * time.Second
	t.Cleanup(func() { convCallTimeout = old })
	listAt(srv, time.Now(), prAt("acme/api-core", 1, headOne), prAt("acme/api-core", 2, headOne), prAt("acme/api-core", 3, headOne))
	var mu sync.Mutex
	var batches [][]string
	srv.fetchThreadTags = func(ctx context.Context, keys []string) (map[string]gh.Conversation, gh.RateLimit, error) {
		mu.Lock()
		batches = append(batches, slices.Clone(keys))
		mu.Unlock()
		if slices.Contains(keys, "acme/api-core#3") {
			<-ctx.Done()
			return nil, gh.RateLimit{}, ctx.Err()
		}
		return map[string]gh.Conversation{}, gh.RateLimit{}, nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		call(t, srv, "GET", "/api/open-threads", "")
	}()
	time.Sleep(50 * time.Millisecond) // the first read is out, on its slow call
	start := time.Now()
	code, _ := call(t, srv, "GET", "/api/open-threads", "")
	if d := time.Since(start); code != http.StatusOK || d > time.Second {
		t.Errorf("a read waited %v on the slow call (%d)", d, code)
	}
	<-done
	if f := srv.convFails["acme/api-core#3"]; f.failures != 1 {
		t.Fatalf("not in quarantine: %+v", srv.convFails)
	}

	// Past its backoff, it goes alone.
	srv.convMu.Lock()
	srv.convFails["acme/api-core#3"] = needsCache{failedAt: time.Now().Add(-time.Hour), failures: 1, err: "timeout"}
	for k := range srv.convs {
		srv.convs[k] = conversationEntry{at: time.Now().Add(-2 * time.Minute)}
	}
	srv.convMu.Unlock()
	mu.Lock()
	batches = nil
	mu.Unlock()
	call(t, srv, "GET", "/api/open-threads", "")
	for _, b := range batches {
		if slices.Contains(b, "acme/api-core#3") && len(b) > 1 {
			t.Errorf("the PR in quarantine went with others: %v", b)
		}
	}
}

// Q2: one PR in its backoff is that PR's news: the others are fresh, nothing
// is stale, and there is no error for the whole page.
func TestUmPREmBackoffNaoEDaPaginaToda(t *testing.T) {
	srv := newTestServer(t)
	listAt(srv, time.Now(), prAt("acme/api-core", 1, headOne), prAt("acme/api-core", 2, headOne))
	srv.fetchThreadTags = func(_ context.Context, keys []string) (map[string]gh.Conversation, gh.RateLimit, error) {
		if slices.Contains(keys, "acme/api-core#2") {
			return nil, gh.RateLimit{}, errors.New("HTTP 502")
		}
		return map[string]gh.Conversation{}, gh.RateLimit{}, nil
	}
	srv.convMu.Lock()
	srv.convFails["acme/api-core#2"] = needsCache{failedAt: time.Now(), failures: 1, err: "HTTP 502"}
	srv.convMu.Unlock()
	_, out := call(t, srv, "GET", "/api/open-threads", "")
	errs, _ := out["errors"].(map[string]any)
	if out["error"] != nil || out["stale"] != nil || len(errs) != 1 || errs["acme/api-core#2"] == nil {
		t.Errorf("%v", out)
	}
}

// Q3: a PR with more pages than the cap reads that many and says it is partial.
func TestPaginasTemTeto(t *testing.T) {
	srv := newTestServer(t)
	listAt(srv, time.Now(), prAt("acme/api-core", 7, headOne))
	mine := gh.Thread{URL: "https://github.com/acme/api-core/pull/7#d", Comments: []gh.Said{{Author: gh.Who{Login: "beroni"}}}}
	srv.fetchThreadTags = func(context.Context, []string) (map[string]gh.Conversation, gh.RateLimit, error) {
		return map[string]gh.Conversation{"acme/api-core#7": {Threads: []gh.Thread{mine}, ThreadsAfter: "c"}}, gh.RateLimit{}, nil
	}
	pages := 0
	srv.fetchThreadTagsPage = func(context.Context, string, string) (gh.Conversation, gh.RateLimit, error) {
		pages++
		return gh.Conversation{Threads: []gh.Thread{mine}, ThreadsAfter: "more"}, gh.RateLimit{}, nil
	}
	_, out := call(t, srv, "GET", "/api/open-threads", "")
	if pages != convMorePages {
		t.Errorf("%d more pages read, the cap is %d", pages, convMorePages)
	}
	if p, _ := out["partial"].(map[string]any); p["acme/api-core#7"] != true {
		t.Errorf("not marked partial: %v", out)
	}
	if n := len(out["threads"].(map[string]any)["acme/api-core#7"].([]any)); n != 1+convMorePages {
		t.Errorf("%d threads", n)
	}
}

// N5-1: a read that panics lets the next one go.
func TestPanicNaoTravaAsEtiquetas(t *testing.T) {
	srv := newTestServer(t)
	listAt(srv, time.Now(), prAt("acme/api-core", 7, headOne))
	calls := 0
	srv.fetchThreadTags = func(context.Context, []string) (map[string]gh.Conversation, gh.RateLimit, error) {
		calls++
		if calls == 1 {
			panic("boom")
		}
		return map[string]gh.Conversation{}, gh.RateLimit{}, nil
	}
	call(t, srv, "GET", "/api/open-threads", "")
	call(t, srv, "GET", "/api/open-threads?refresh=1", "")
	if calls != 2 {
		t.Errorf("after a panic the tags read %d times, want 2", calls)
	}
}

// N5-2: while a read is out, the page gets what there is as it is: a PR read a
// moment ago is not stale, and a page with nothing yet is loading, not failed.
func TestLeituraEmCursoNaoEStale(t *testing.T) {
	srv := newTestServer(t)
	now := time.Now()
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = time.Now })
	listAt(srv, now, prAt("acme/api-core", 7, headOne))
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv.fetchThreadTags = func(context.Context, []string) (map[string]gh.Conversation, gh.RateLimit, error) {
		entered <- struct{}{}
		<-release
		return map[string]gh.Conversation{}, gh.RateLimit{}, nil
	}

	// At boot, nothing yet.
	go call(t, srv, "GET", "/api/open-threads", "")
	<-entered
	_, out := call(t, srv, "GET", "/api/open-threads", "")
	if out["loading"] != true || out["reading"] != true || out["error"] != nil || out["stale"] != nil {
		t.Errorf("at boot: %v", out)
	}
	release <- struct{}{}
	waitUnlocked(t, srv)

	// Past the minute, a read goes out again: the PR read the last time is not
	// stale meanwhile.
	now = now.Add(61 * time.Second)
	go call(t, srv, "GET", "/api/open-threads", "")
	<-entered
	_, out = call(t, srv, "GET", "/api/open-threads", "")
	if out["stale"] != nil || out["error"] != nil || out["loading"] != nil || out["reading"] != true {
		t.Errorf("while a read is out: %v", out)
	}
	release <- struct{}{}
	waitUnlocked(t, srv)
}

// waitUnlocked waits for the read under way to end.
func waitUnlocked(t *testing.T, srv *Server) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		srv.convMu.Lock()
		busy := srv.convReading
		srv.convMu.Unlock()
		if !busy {
			return
		}
	}
	t.Fatal("the read never ended")
}

// QA: after a ↻ that failed, a plain reload within the minute does not serve
// that PR's last good read as fresh: it stays stale until a read of it works.
func TestFalhaFicaStaleMesmoDentroDoMinuto(t *testing.T) {
	answer := filepath.Join(t.TempDir(), "a.json")
	if err := os.WriteFile(answer, []byte(threadsAnswer), 0o600); err != nil {
		t.Fatal(err)
	}
	fail := filepath.Join(t.TempDir(), "fail")
	srv, _ := threadsServer(t, "if [ -f "+fail+" ]; then echo 'HTTP 502' >&2; exit 1; fi; cat "+answer)
	call(t, srv, "GET", "/api/open-threads", "")
	if err := os.WriteFile(fail, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	call(t, srv, "GET", "/api/open-threads?refresh=1", "")
	_, out := call(t, srv, "GET", "/api/open-threads", "") // a plain reload, within the minute
	stale, _ := out["stale"].(map[string]any)
	if stale["acme/api-core#7"] == nil || len(out["threads"].(map[string]any)) != 1 {
		t.Errorf("the PR that failed reads as fresh: %v", out)
	}
	// A read of it that works ends it.
	os.Remove(fail)
	srv.convMu.Lock()
	srv.convFails["acme/api-core#7"] = needsCache{failedAt: time.Now().Add(-time.Hour), failures: 1, err: "HTTP 502"}
	srv.convMu.Unlock()
	if _, out := call(t, srv, "GET", "/api/open-threads", ""); out["stale"] != nil {
		t.Errorf("after a good read: %v", out)
	}
}
