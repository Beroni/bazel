package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beroni/bazel/internal/config"
	"github.com/beroni/bazel/internal/gh"
	"github.com/beroni/bazel/internal/store"
)

// The Overview's data comes from one gh call per head sha: a second look at
// the same commit is served from the cache, a new commit reads it again, and
// a repository that isn't watched never reaches gh.
func TestPRDetailsCacheiaPorHead(t *testing.T) {
	srv := newTestServer(t)
	srv.prs = []gh.PR{testPR(482)}
	srv.prs[0].HeadRefOid = "aaa"
	calls := 0
	srv.fetchDetails = func(_ context.Context, repo string, number int) (gh.Details, error) {
		calls++
		var d gh.Details
		_ = json.Unmarshal([]byte(`{"headRefOid":"`+srv.prs[0].HeadRefOid+`","body":"**hi**",
			"commits":[{"oid":"a1f3c9e00","messageHeadline":"Add grok adapter","authors":[{"login":"beroni"}]}],
			"files":[{"path":"internal/agent/grok.go","additions":182,"deletions":0}],
			"reviewRequests":[{"login":"mrc"}],
			"latestReviews":[{"author":{"login":"ana"},"state":"APPROVED"}],
			"labels":[{"name":"agents"}],
			"statusCheckRollup":[{"name":"build","status":"COMPLETED","conclusion":"SUCCESS"},
				{"context":"lint","state":"PENDING"},{"name":"test","status":"COMPLETED","conclusion":"FAILURE"}]}`), &d)
		return d, nil
	}
	h := srv.Handler()
	get := func(path string) (int, detailsView) {
		r := httptest.NewRequest("GET", "http://127.0.0.1:7777"+path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var v detailsView
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return w.Code, v
	}
	path := "/api/prs/acme/api-core/482/details"
	code, v := get(path)
	if code != http.StatusOK || calls != 1 {
		t.Fatalf("first read: %d, %d gh calls", code, calls)
	}
	if len(v.Commits) != 1 || v.Commits[0].Short != "a1f3c9e" || v.Commits[0].Author != "beroni" {
		t.Errorf("commits: %+v", v.Commits)
	}
	if len(v.Files) != 1 || v.Files[0].Additions != 182 || len(v.Labels) != 1 || v.BodyHTML == "" {
		t.Errorf("files/labels/body: %+v %v %q", v.Files, v.Labels, v.BodyHTML)
	}
	wantRev := []reviewerView{{"ana", "approved"}, {"mrc", "requested"}}
	wantChk := []checkView{{"build", "passed"}, {"lint", "pending"}, {"test", "failed"}}
	for i, r := range wantRev {
		if i >= len(v.Reviewers) || v.Reviewers[i] != r {
			t.Errorf("reviewers: %+v, want %+v", v.Reviewers, wantRev)
			break
		}
	}
	for i, c := range wantChk {
		if i >= len(v.Checks) || v.Checks[i] != c {
			t.Errorf("checks: %+v, want %+v", v.Checks, wantChk)
			break
		}
	}

	get(path)
	if calls != 1 {
		t.Errorf("same head read again: %d gh calls, want 1", calls)
	}
	srv.prs[0].HeadRefOid = "bbb"
	if _, v := get(path); calls != 2 || v.Sha != "bbb" {
		t.Errorf("a new head must be read again: %d gh calls, sha %q", calls, v.Sha)
	}
	if code, _ := get("/api/prs/other/repo/1/details"); code != http.StatusNotFound || calls != 2 {
		t.Errorf("unwatched repo: %d, %d gh calls", code, calls)
	}
}

// History on demand: POST runs the history agent as a job; when it ends, GET
// serves the parsed story for this head. The PR is not marked reviewed and
// nothing shows up in the Saved tab — a history is not a review.
func TestHistorySobDemanda(t *testing.T) {
	t.Setenv("BAZEL_HOME", t.TempDir())
	md := "Commits read: 2\n\n## What it does\nAdds <b>X</b> ![x](https://attacker.example/p.png).\n\n## Decisions\n### 1. Use Y\n- Commit: abc1234 · description\nBecause Z.\n- Rejected: W.\n\n## Still open\n- Q?\n"
	cfg := cfgFor(t, "sh", "-c", "cat >/dev/null; printf '%s' \"$HIST\"")
	no := false
	cfg.HistoryAgent = config.AgentDef{Name: "history", Task: "/history-pr {{number}}", Checkout: &no, Env: map[string]string{"HIST": md}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := New(ctx, cfg, "beroni", Options{Addr: "127.0.0.1:0", Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	srv.prs = []gh.PR{testPR(482)}
	ch := srv.jobs.hub.Subscribe()
	defer srv.jobs.hub.Unsubscribe(ch)
	h := srv.Handler()
	do := func(method, path string) (int, []byte) {
		r := httptest.NewRequest(method, "http://127.0.0.1:7777"+path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.Bytes()
	}
	path := "/api/prs/acme/api-core/482/history"
	code, body := do("GET", path)
	var v historyView
	_ = json.Unmarshal(body, &v)
	if code != http.StatusOK || v.Found {
		t.Fatalf("before generating: %d %s", code, body)
	}
	code, body = do("POST", path)
	var job jobView
	_ = json.Unmarshal(body, &job)
	if code != http.StatusAccepted || !job.History {
		t.Fatalf("generate: %d %s", code, body)
	}
	waitFor(t, ch, job.ID, StateDone)
	_, body = do("GET", path)
	v = historyView{}
	_ = json.Unmarshal(body, &v)
	if !v.Found || v.Stale || v.CommitsRead != 2 || len(v.Decisions) != 1 || v.Decisions[0].Sha != "abc1234" ||
		len(v.Decisions[0].Rejected) != 1 || len(v.StillOpen) != 1 || v.WhatHTML == "" {
		t.Errorf("history after the job: %s", body)
	}
	// An agent's raw HTML shows as text, as in a review, and no image is
	// fetched from this machine.
	if !strings.Contains(v.WhatHTML, "&lt;b&gt;X&lt;/b&gt;") || strings.Contains(v.WhatHTML, "<img") {
		t.Errorf("what it does: %s", v.WhatHTML)
	}
	marks := store.LoadMarks(srv.reviewsDir)
	if _, ok := marks[testPR(482).Key()]; ok {
		t.Error("a history must not mark the PR reviewed")
	}
	if saved, _ := store.List(srv.reviewsDir); len(saved) != 0 {
		t.Errorf("a history must not land in Saved: %v", saved)
	}
}

// The details cache is bounded and gh is never hammered: a failed read is not
// retried for a while, an entry goes stale after detailsTTL even at the same
// head, a PR outside the list is only read on explicit request and a few
// times a minute, and the cache never holds more than detailsMax entries.
func TestPRDetailsLimites(t *testing.T) {
	srv := newTestServer(t)
	srv.prs = []gh.PR{testPR(482)}
	srv.prs[0].HeadRefOid = "aaa"
	calls, fail := 0, false
	srv.fetchDetails = func(_ context.Context, repo string, number int) (gh.Details, error) {
		calls++
		if fail {
			return gh.Details{}, errors.New("HTTP 403: API rate limit exceeded")
		}
		return gh.Details{HeadRefOid: "aaa"}, nil
	}
	now := time.Unix(1_000_000, 0)
	timeNow = func() time.Time { return now }
	oldMax, oldPer := detailsMax, offListPerMin
	t.Cleanup(func() { timeNow = time.Now; detailsMax, offListPerMin = oldMax, oldPer })
	h := srv.Handler()
	get := func(path string) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:7777"+path, nil))
		return w.Code
	}
	listed := "/api/prs/acme/api-core/482/details"

	// A failure is kept: asking again within detailsErrTTL does not call gh.
	fail = true
	if code := get(listed); code != http.StatusBadGateway || calls != 1 {
		t.Fatalf("failed read: %d, %d calls", code, calls)
	}
	if code := get(listed); code != http.StatusBadGateway || calls != 1 {
		t.Errorf("a failure must not be retried at once: %d, %d calls", code, calls)
	}
	// Retry reads past it, but not faster than detailsRetryGap, and it says when.
	retry := listed + "?retry=1"
	if code := get(retry); code != http.StatusBadGateway || calls != 2 {
		t.Errorf("Retry must read again: %d, %d calls", code, calls)
	}
	if code := get(retry); code != http.StatusTooManyRequests || calls != 2 {
		t.Errorf("a second Retry at once: %d, %d calls", code, calls)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:7777"+retry, nil))
	if !strings.Contains(w.Body.String(), "try again in 5s") {
		t.Errorf("the refusal must say when: %s", w.Body)
	}
	if code := get(listed); code != http.StatusBadGateway || calls != 2 {
		t.Errorf("a plain read still gets the cached failure: %d, %d calls", code, calls)
	}
	now = now.Add(detailsRetryGap)
	fail = false
	if code := get(retry); code != http.StatusOK || calls != 3 {
		t.Errorf("Retry after the gap, GitHub back: %d, %d calls", code, calls)
	}
	// With nothing failed, retry=1 is a plain read: it cannot force a call.
	if code := get(retry); code != http.StatusOK || calls != 3 {
		t.Errorf("Retry with no failure: %d, %d calls", code, calls)
	}
	now = now.Add(detailsTTL + time.Second)
	fail = true
	get(listed)
	now = now.Add(detailsErrTTL + time.Second)
	fail = false
	if code := get(listed); code != http.StatusOK || calls != 5 {
		t.Errorf("after the error window: %d, %d calls", code, calls)
	}
	// Same head, but checks and reviews move: stale after detailsTTL.
	get(listed)
	if calls != 5 {
		t.Errorf("within the TTL the cache answers: %d calls", calls)
	}
	now = now.Add(detailsTTL + time.Second)
	if get(listed); calls != 6 {
		t.Errorf("past the TTL it is read again: %d calls", calls)
	}

	// Outside the list: refused without offlist=1, rate limited with it.
	offListPerMin = 2
	if code := get("/api/prs/acme/api-core/7/details"); code != http.StatusNotFound || calls != 6 {
		t.Errorf("off-list without asking: %d, %d calls", code, calls)
	}
	for n, want := range []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests} {
		if code := get(fmt.Sprintf("/api/prs/acme/api-core/%d/details?offlist=1", 100+n)); code != want {
			t.Errorf("off-list read %d: %d, want %d", n, code, want)
		}
	}
	now = now.Add(time.Minute)
	if code := get("/api/prs/acme/api-core/200/details?offlist=1"); code != http.StatusOK {
		t.Errorf("a minute later off-list reads are allowed again: %d", code)
	}

	// The cache keeps at most detailsMax entries.
	detailsMax, offListPerMin = 3, 100
	for n := 300; n < 310; n++ {
		now = now.Add(time.Second)
		get(fmt.Sprintf("/api/prs/acme/api-core/%d/details?offlist=1", n))
	}
	srv.detMu.Lock()
	defer srv.detMu.Unlock()
	if len(srv.details) != 3 {
		t.Errorf("cache holds %d entries, want 3", len(srv.details))
	}
	for _, n := range []int{307, 308, 309} {
		if _, ok := srv.details[fmt.Sprintf("acme/api-core#%d", n)]; !ok {
			t.Errorf("#%d is among the newest and was evicted", n)
		}
	}
}

// Everything that reaches GitHub shares one budget: past it a PR with a good
// page keeps that page, one without gets 429, and a forced refresh of the list
// stops forcing. Retry's gap outlives its cache entry.
func TestLimiteGlobalDoGitHub(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Repos = nil
	srv.prs = []gh.PR{testPR(1), testPR(2), testPR(3)}
	calls, fail := 0, false
	srv.fetchDetails = func(context.Context, string, int) (gh.Details, error) {
		calls++
		if fail {
			return gh.Details{}, errors.New("HTTP 502")
		}
		return gh.Details{HeadRefOid: "aaa"}, nil
	}
	now := time.Unix(1_000_000, 0)
	timeNow = func() time.Time { return now }
	old := ghReadsPerMin
	ghReadsPerMin = 2
	t.Cleanup(func() { timeNow = time.Now; ghReadsPerMin = old })
	srv.cfg.Repos = []string{"acme/api-core"}
	h := srv.Handler()
	get := func(n int, query string) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:7777/api/prs/acme/api-core/%d/details%s", n, query), nil))
		return w.Code
	}
	if c1, c2 := get(1, ""), get(2, ""); c1 != http.StatusOK || c2 != http.StatusOK || calls != 2 {
		t.Fatalf("within the budget: %d %d, %d calls", c1, c2, calls)
	}
	if code := get(3, ""); code != http.StatusTooManyRequests || calls != 2 {
		t.Errorf("past the budget: %d, %d calls", code, calls)
	}
	now = now.Add(detailsTTL + time.Second)
	ghReadsPerMin = 0
	if code := get(1, ""); code != http.StatusOK || calls != 2 {
		t.Errorf("an old good page beats a refusal: %d, %d calls", code, calls)
	}

	// The page is not left without reads by forced refreshes, nor the other
	// way round: they have a budget of their own, charged per gh process.
	oldList := listReadsPerMin
	listReadsPerMin = 3
	t.Cleanup(func() { listReadsPerMin = oldList })
	srv.detMu.Lock()
	first, second, third := srv.listReadLocked(now, 2), srv.listReadLocked(now, 2), srv.listReadLocked(now.Add(time.Minute), 5)
	srv.detMu.Unlock()
	if !first || second || !third {
		t.Errorf("list budget: %v %v %v", first, second, third)
	}
	srv.prsAt = time.Now()
	at := srv.prsAt
	if _, _, got, err := srv.loadPRs(context.Background(), true); err != nil || !got.Equal(at) {
		t.Errorf("a refresh past the budget must serve the fresh list: %v %v", got, err)
	}

	ghReadsPerMin = 100
	fail = true
	get(3, "")
	now = now.Add(time.Second)
	if code := get(3, "?retry=1"); code != http.StatusBadGateway {
		t.Fatalf("first Retry: %d", code)
	}
	retryCalls := calls
	srv.detMu.Lock()
	delete(srv.details, "acme/api-core#3")
	srv.detMu.Unlock()
	if code := get(3, "?retry=1"); code != http.StatusTooManyRequests || calls != retryCalls {
		t.Errorf("second Retry: %d, %d calls", code, calls)
	}
}

// A history_agent that publishes is refused when a history is asked for, with
// the reason, and nothing is queued.
func TestHistoryRecusaAgentQuePublica(t *testing.T) {
	t.Setenv("BAZEL_HOME", t.TempDir())
	cfg := cfgFor(t, "sleep", "30")
	cfg.HistoryAgent = config.AgentDef{Name: "h", Task: "/bazel-post-report {{review_file}}"}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := New(ctx, cfg, "beroni", Options{Addr: "127.0.0.1:0", Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	srv.prs = []gh.PR{testPR(482)}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest("POST", "http://127.0.0.1:7777/api/prs/acme/api-core/482/history", nil))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "history_agent") {
		t.Errorf("generate: %d %s", w.Code, w.Body)
	}
	if views := srv.jobViews(); len(views) != 0 {
		t.Errorf("a refused history must not queue a job: %v", views)
	}
}

// With the list behind GitHub (a push after it loaded) a good read still stays
// in the cache; a newer list is what makes it old.
func TestPRDetailsListaAtrasada(t *testing.T) {
	srv := newTestServer(t)
	srv.prs = []gh.PR{testPR(482)}
	srv.prs[0].HeadRefOid = "old"
	calls := 0
	srv.fetchDetails = func(context.Context, string, int) (gh.Details, error) {
		calls++
		return gh.Details{HeadRefOid: "new"}, nil
	}
	h := srv.Handler()
	get := func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:7777/api/prs/acme/api-core/482/details", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("details: %d %s", w.Code, w.Body)
		}
	}
	for range 5 {
		get()
	}
	if calls != 1 {
		t.Errorf("the cache must answer with the list behind: %d calls", calls)
	}
	srv.prs[0].HeadRefOid = "new"
	if get(); calls != 2 {
		t.Errorf("a newer list reads again: %d calls", calls)
	}
}

// Many tabs asking at once for a PR nobody has read cost one gh call, and they
// all get its answer.
func TestPRDetailsLeituraEmVoo(t *testing.T) {
	srv := newTestServer(t)
	srv.prs = []gh.PR{testPR(482)}
	var calls atomic.Int32
	release := make(chan struct{})
	srv.fetchDetails = func(context.Context, string, int) (gh.Details, error) {
		calls.Add(1)
		<-release
		return gh.Details{HeadRefOid: "aaa"}, nil
	}
	h := srv.Handler()
	const n = 40
	codes := make(chan int, n)
	for range n {
		go func() {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:7777/api/prs/acme/api-core/482/details", nil))
			codes <- w.Code
		}()
	}
	for deadline := time.Now().Add(5 * time.Second); calls.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // the others reach the wait
	close(release)
	for range n {
		if code := <-codes; code != http.StatusOK {
			t.Errorf("status %d", code)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("%d gh calls for one PR", got)
	}
}

// The tab that started a read may go away: the read goes on and its result is
// kept, so that never lands in the cache as a failure.
func TestPRDetailsLeituraCancelada(t *testing.T) {
	srv := newTestServer(t)
	srv.prs = []gh.PR{testPR(482)}
	started, release := make(chan struct{}), make(chan struct{})
	var seen error
	srv.fetchDetails = func(ctx context.Context, _ string, _ int) (gh.Details, error) {
		close(started)
		<-release
		seen = ctx.Err()
		return gh.Details{HeadRefOid: "aaa"}, nil
	}
	h := srv.Handler()
	path := "http://127.0.0.1:7777/api/prs/acme/api-core/482/details"
	ctx, leave := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil).WithContext(ctx))
	}()
	<-started
	leave()
	close(release)
	<-done
	if seen != nil {
		t.Errorf("the tab leaving reached the read: %v", seen)
	}
	srv.detMu.Lock()
	defer srv.detMu.Unlock()
	if e := srv.details["acme/api-core#482"]; e.err != "" || e.sha == "" && e.at.IsZero() {
		t.Errorf("the read must be kept: %+v", e)
	}
}

// GET /history of a PR outside the list goes through the same door as the
// Overview: refused unless asked for, read once, and under the same budget.
func TestHistoryForaDaListaTemLimite(t *testing.T) {
	srv := newTestServer(t)
	calls := 0
	srv.fetchDetails = func(context.Context, string, int) (gh.Details, error) {
		calls++
		return gh.Details{HeadRefOid: "abc1234"}, nil
	}
	oldPer := offListPerMin
	offListPerMin = 2
	t.Cleanup(func() { offListPerMin = oldPer })
	if _, err := store.SaveHistory(srv.reviewsDir, "acme/api-core", 7, "abc1234", "## What it does\nX.\n"); err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	get := func(path string) (int, historyView) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:7777"+path, nil))
		var v historyView
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return w.Code, v
	}
	if code, _ := get("/api/prs/acme/api-core/7/history"); code != http.StatusNotFound || calls != 0 {
		t.Errorf("without offlist=1: %d, %d calls", code, calls)
	}
	for range 3 {
		code, v := get("/api/prs/acme/api-core/7/history?offlist=1")
		if code != http.StatusOK || !v.Found || v.Stale {
			t.Errorf("off-list history: %d %+v", code, v)
		}
	}
	if calls != 1 {
		t.Errorf("the same PR costs one read: %d calls", calls)
	}
	get("/api/prs/acme/api-core/8/history?offlist=1")
	if code, _ := get("/api/prs/acme/api-core/9/history?offlist=1"); code != http.StatusTooManyRequests || calls != 2 {
		t.Errorf("past the budget: %d, %d calls", code, calls)
	}
}

// The page learns from the step that runs: an `agent` block with an executable
// of its own reaches the History with no history_agent in sight.
func TestHistoryAvisaPassoCustom(t *testing.T) {
	cfg := cfgFor(t, "/usr/local/bin/review-and-post")
	t.Setenv("BAZEL_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := New(ctx, cfg, "beroni", Options{Addr: "127.0.0.1:0", Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	srv.prs = []gh.PR{testPR(482)}
	h := srv.Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:7777/api/state", nil))
	if !strings.Contains(w.Body.String(), `"history_custom":true`) {
		t.Errorf("/api/state does not say the step is custom: %s", w.Body)
	}
}

// Forced refreshes that wait on one another are one: the list that landed in
// the meantime answers them, at no cost.
func TestRefreshEmFilaNaoRelê(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.Repos = []string{"acme/api-core"}
	srv.prsAt = time.Now()
	srv.fetchMu.Lock()
	done := make(chan time.Time)
	go func() {
		_, _, at, _ := srv.loadPRs(context.Background(), true)
		done <- at
	}()
	time.Sleep(50 * time.Millisecond)
	srv.prMu.Lock()
	srv.prsAt = time.Now()
	landed := srv.prsAt
	srv.prMu.Unlock()
	srv.fetchMu.Unlock()
	if at := <-done; !at.Equal(landed) {
		t.Errorf("got the list of %v, want the one that landed", at)
	}
	if len(srv.listReads) != 0 {
		t.Errorf("a refresh that did not run was charged: %d", len(srv.listReads))
	}
}

// The read has a limit of its own, and the failure window starts when it ends:
// a gh that took long to fail does not come back already expired.
func TestPRDetailsTempoDaLeitura(t *testing.T) {
	srv := newTestServer(t)
	srv.prs = []gh.PR{testPR(482)}
	now := time.Unix(1_000_000, 0)
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = time.Now })
	calls := 0
	srv.fetchDetails = func(ctx context.Context, _ string, _ int) (gh.Details, error) {
		calls++
		if _, ok := ctx.Deadline(); !ok {
			t.Error("the read has no timeout")
		}
		now = now.Add(detailsErrTTL + time.Second)
		return gh.Details{}, errors.New("HTTP 502")
	}
	h := srv.Handler()
	for range 2 {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://127.0.0.1:7777/api/prs/acme/api-core/482/details", nil))
	}
	if calls != 1 {
		t.Errorf("a slow failure must stay cached: %d calls", calls)
	}
}

// A Retry refused by a budget did not read anything, so it does not start the
// gap; and a POST for a PR outside the list spends the same budget as a GET.
func TestRecusaNaoConsomeRetry(t *testing.T) {
	srv := newTestServer(t)
	srv.prs = []gh.PR{testPR(482)}
	now := time.Unix(1_000_000, 0)
	timeNow = func() time.Time { return now }
	oldGh, oldOff := ghReadsPerMin, offListPerMin
	t.Cleanup(func() { timeNow = time.Now; ghReadsPerMin, offListPerMin = oldGh, oldOff })
	calls := 0
	srv.fetchDetails = func(context.Context, string, int) (gh.Details, error) {
		calls++
		return gh.Details{}, errors.New("HTTP 502")
	}
	h := srv.Handler()
	do := func(method, path string) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, "http://127.0.0.1:7777"+path, nil))
		return w.Code
	}
	retry := "/api/prs/acme/api-core/482/details?retry=1"
	do("GET", "/api/prs/acme/api-core/482/details")
	ghReadsPerMin = 0
	if code := do("GET", retry); code != http.StatusTooManyRequests || calls != 1 {
		t.Errorf("past the budget: %d, %d calls", code, calls)
	}
	ghReadsPerMin = 100
	if code := do("GET", retry); code != http.StatusBadGateway || calls != 2 {
		t.Errorf("the refused Retry took the gap: %d, %d calls", code, calls)
	}
	offListPerMin = 0
	if code := do("POST", "/api/prs/acme/api-core/999/history"); code != http.StatusTooManyRequests {
		t.Errorf("POST outside the list: %d", code)
	}
}

// Who waits on a read: the tab that leaves stops waiting, a Retry joins the
// read under way instead of being refused or starting another, and if the read
// blows up the others get an error, not an empty page.
func TestPRDetailsQuemEspera(t *testing.T) {
	srv := newTestServer(t)
	srv.prs = []gh.PR{testPR(482)}
	path := "http://127.0.0.1:7777/api/prs/acme/api-core/482/details"
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	srv.fetchDetails = func(context.Context, string, int) (gh.Details, error) {
		calls.Add(1)
		close(started)
		<-release
		panic("boom")
	}
	h := srv.Handler()
	leader := make(chan struct{})
	go func() {
		defer close(leader)
		defer func() { _ = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}()
	<-started

	gone, leave := context.WithCancel(context.Background())
	leave()
	left := make(chan struct{})
	go func() {
		defer close(left)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil).WithContext(gone))
	}()
	select {
	case <-left:
	case <-time.After(5 * time.Second):
		t.Fatal("a waiter whose tab left keeps waiting")
	}

	codes := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path+"?retry=1", nil))
		codes <- w.Code
	}()
	time.Sleep(50 * time.Millisecond) // the Retry reaches the wait
	close(release)
	<-leader
	if code := <-codes; code != http.StatusBadGateway || calls.Load() != 1 {
		t.Errorf("waiter after the read blew up: %d, %d calls", code, calls.Load())
	}
}
