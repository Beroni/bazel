package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/beroni/bazel/internal/agent"
	"github.com/beroni/bazel/internal/gh"
	"github.com/beroni/bazel/internal/store"
)

const (
	headOne = "1111111111111111111111111111111111111111"
	headTwo = "2222222222222222222222222222222222222222"
)

// verdictServer is a test server whose queue holds a finished review of each PR
// given, in that order (the last one is the newest card).
func verdictServer(t *testing.T, prs ...gh.PR) (*Server, []string) {
	t.Helper()
	srv := newTestServer(t)
	ch := srv.hub.Subscribe()
	t.Cleanup(func() { srv.hub.Unsubscribe(ch) })
	var ids []string
	for _, pr := range prs {
		v, err := srv.jobs.Enqueue(pr, false, srv.cfg.DefaultChoice())
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, ch, v.ID, StateDone)
		ids = append(ids, v.ID)
	}
	return srv, ids
}

func prAt(repo string, number int, head string) gh.PR {
	pr := testPR(number)
	pr.Repo = repo
	pr.URL = "https://github.com/" + repo + "/pull/" + strconv.Itoa(number)
	pr.HeadRefOid = head
	return pr
}

func call(t *testing.T, srv *Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// The buttons under a job's report: the decision goes in the index with the
// head the review read, changes, clears, and nothing calls gh.
func TestDecisaoPeloJob(t *testing.T) {
	log := ghFalso(t, "exit 1")
	pr := prAt("acme/api-core", 7, headOne)
	srv, ids := verdictServer(t, pr)

	code, out := call(t, srv, "POST", "/api/jobs/"+ids[0]+"/verdict", `{"verdict":"approve"}`)
	if code != http.StatusOK {
		t.Fatalf("approve: %d %v", code, out)
	}
	got, ok := store.LoadMarks(srv.reviewsDir).VerdictOf(pr.Key())
	if !ok || got.Verdict != store.VerdictApprove || got.Head != headOne {
		t.Fatalf("on disk: %+v %v", got, ok)
	}

	call(t, srv, "POST", "/api/jobs/"+ids[0]+"/verdict", `{"verdict":"request_changes"}`)
	if got, _ := store.LoadMarks(srv.reviewsDir).VerdictOf(pr.Key()); got.Verdict != store.VerdictChanges {
		t.Errorf("changed: %+v", got)
	}
	call(t, srv, "POST", "/api/jobs/"+ids[0]+"/verdict", `{"verdict":""}`)
	if _, ok := store.LoadMarks(srv.reviewsDir).VerdictOf(pr.Key()); ok {
		t.Error("clearing left the decision")
	}

	for body, want := range map[string]int{`{"verdict":"lgtm"}`: 400, `{}`: 400, `nope`: 400} {
		if code, _ := call(t, srv, "POST", "/api/jobs/"+ids[0]+"/verdict", body); code != want {
			t.Errorf("%s: %d", body, code)
		}
	}
	if code, _ := call(t, srv, "POST", "/api/jobs/nope/verdict", `{"verdict":"approve"}`); code != http.StatusNotFound {
		t.Errorf("unknown job: %d", code)
	}
	if n := contaNoLog(log, "start"); n != 0 {
		t.Errorf("a decision ran gh %d times", n)
	}
}

// A decision outlives the server: a new one on the same folder reads it.
func TestDecisaoSobreviveAoRestart(t *testing.T) {
	pr := prAt("acme/api-core", 7, headOne)
	srv, ids := verdictServer(t, pr)
	call(t, srv, "POST", "/api/jobs/"+ids[0]+"/verdict", `{"verdict":"approve_with_comments"}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	again, err := New(ctx, srv.cfg, "beroni", Options{Addr: "127.0.0.1:0", Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, out := call(t, again, "GET", "/api/state", "")
	vs, _ := out["verdicts"].(map[string]any)
	v, _ := vs[strings.ToLower(pr.Key())].(map[string]any)
	if v["verdict"] != store.VerdictComment || v["head"] != headOne {
		t.Errorf("after the restart: %v", out["verdicts"])
	}
}

// A saved review decides on the head its file says it read.
func TestDecisaoPeloSalvo(t *testing.T) {
	srv := newTestServer(t)
	pr := prAt("acme/api-core", 9, headOne)
	path, err := store.Save(srv.reviewsDir, agent.Result{Agent: "lens", PR: pr, Body: "# Verdict\n\nok"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	code, out := call(t, srv, "POST", "/api/reviews/"+filepath.Base(path)+"/verdict", `{"verdict":"request_changes"}`)
	if code != http.StatusOK {
		t.Fatalf("%d %v", code, out)
	}
	got, ok := store.LoadMarks(srv.reviewsDir).VerdictOf(pr.Key())
	if !ok || got.Verdict != store.VerdictChanges || got.Head != headOne {
		t.Errorf("on disk: %+v %v", got, ok)
	}
	if code, _ := call(t, srv, "POST", "/api/reviews/nope.md/verdict", `{"verdict":"approve"}`); code != http.StatusNotFound {
		t.Errorf("unknown file: %d", code)
	}
}

// A PR that moved past the decided head says so; the decision stays.
func TestDecisaoDePRQueAndou(t *testing.T) {
	pr := prAt("acme/api-core", 7, headOne)
	srv, ids := verdictServer(t, pr)
	call(t, srv, "POST", "/api/jobs/"+ids[0]+"/verdict", `{"verdict":"approve"}`)
	if v := srv.verdictViews()[strings.ToLower(pr.Key())]; v.Moved || v.Verdict != store.VerdictApprove {
		t.Fatalf("same head: %+v", v)
	}

	moved := pr
	moved.HeadRefOid = headTwo
	srv.prMu.Lock()
	srv.prs = []gh.PR{moved}
	srv.prMu.Unlock()
	v := srv.verdictViews()[strings.ToLower(pr.Key())]
	if !v.Moved || v.Verdict != store.VerdictApprove || v.Head != headOne {
		t.Errorf("the PR moved: %+v", v)
	}
	if s := srv.summary(); len(s.Decided) != 1 || !s.Decided[0].Moved {
		t.Errorf("summary: %+v", s)
	}
}

// The summary: one line per PR of the queue with a decision, in the order of the
// cards (newest first), each PR once however many jobs it has; the PRs with no
// decision are listed apart and stay out of the text.
func TestResumo(t *testing.T) {
	a := prAt("klever-io/klever-go", 195, headOne)
	b := prAt("klever-io/klever-go-security", 215, headOne)
	c := prAt("klever-io/klever-go", 206, headOne)
	d := prAt("klever-io/klever-go", 300, headOne)
	// Cards, newest first: d, c, a (again), b, a.
	srv, ids := verdictServer(t, a, b, a, c, d)
	for id, v := range map[string]string{ids[3]: "approve", ids[1]: "request_changes", ids[0]: "approve_with_comments"} {
		if code, out := call(t, srv, "POST", "/api/jobs/"+id+"/verdict", `{"verdict":"`+v+`"}`); code != http.StatusOK {
			t.Fatalf("%s: %d %v", v, code, out)
		}
	}
	code, out := call(t, srv, "GET", "/api/summary", "")
	if code != http.StatusOK {
		t.Fatalf("%d %v", code, out)
	}
	want := "https://github.com/klever-io/klever-go/pull/206 :white_check_mark:\n" +
		"https://github.com/klever-io/klever-go/pull/195 :memo::white_check_mark:\n" +
		"https://github.com/klever-io/klever-go-security/pull/215 :git_change_request:"
	if out["text"] != want {
		t.Errorf("text:\n%v\nwant:\n%s", out["text"], want)
	}
	undecided, _ := out["undecided"].([]any)
	if len(undecided) != 1 || undecided[0].(map[string]any)["number"] != float64(300) {
		t.Errorf("undecided: %v", out["undecided"])
	}
	if decided, _ := out["decided"].([]any); len(decided) != 3 {
		t.Errorf("decided: %v", out["decided"])
	}
}

// With nothing decided, the text is empty and every PR waits.
func TestResumoSemDecisao(t *testing.T) {
	srv, _ := verdictServer(t, prAt("acme/api-core", 7, headOne))
	s := srv.summary()
	if s.Text != "" || len(s.Decided) != 0 || len(s.Undecided) != 1 {
		t.Errorf("%+v", s)
	}
}

// V1: the clone may find the PR past the head it was listed at (a force push in
// between), and the review is of what the clone read. The job's door decides on
// that head, as the saved file's does: a list that then shows the PR there
// says nothing moved.
func TestDecisaoNoHeadRevisadoNaoNoListado(t *testing.T) {
	listed := prAt("acme/api-core", 7, headOne)
	srv, ids := verdictServer(t, listed)
	srv.jobs.mu.Lock()
	srv.jobs.jobs[ids[0]].Result.PR.HeadRefOid = headTwo // what the clone read
	srv.jobs.mu.Unlock()

	if code, out := call(t, srv, "POST", "/api/jobs/"+ids[0]+"/verdict", `{"verdict":"approve"}`); code != http.StatusOK {
		t.Fatalf("%d %v", code, out)
	}
	if got, _ := store.LoadMarks(srv.reviewsDir).VerdictOf(listed.Key()); got.Head != headTwo {
		t.Errorf("decided on %s, the review read %s", got.Head, headTwo)
	}
	now := listed
	now.HeadRefOid = headTwo
	srv.prMu.Lock()
	srv.prs = []gh.PR{now}
	srv.prMu.Unlock()
	if v := srv.verdictViews()[strings.ToLower(listed.Key())]; v.Moved {
		t.Errorf("the PR is where the review read it, yet: %+v", v)
	}
}
