package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/beroni/bazel/internal/gh"
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
