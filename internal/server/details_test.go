package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	now = now.Add(detailsErrTTL + time.Second)
	fail = false
	if code := get(listed); code != http.StatusOK || calls != 2 {
		t.Errorf("after the error window: %d, %d calls", code, calls)
	}
	// Same head, but checks and reviews move: stale after detailsTTL.
	get(listed)
	if calls != 2 {
		t.Errorf("within the TTL the cache answers: %d calls", calls)
	}
	now = now.Add(detailsTTL + time.Second)
	if get(listed); calls != 3 {
		t.Errorf("past the TTL it is read again: %d calls", calls)
	}

	// Outside the list: refused without offlist=1, rate limited with it.
	offListPerMin = 2
	if code := get("/api/prs/acme/api-core/7/details"); code != http.StatusNotFound || calls != 3 {
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
		get(fmt.Sprintf("/api/prs/acme/api-core/%d/details?offlist=1", n))
	}
	srv.detMu.Lock()
	size := len(srv.details)
	srv.detMu.Unlock()
	if size > 3 {
		t.Errorf("cache holds %d entries, want at most 3", size)
	}
}
