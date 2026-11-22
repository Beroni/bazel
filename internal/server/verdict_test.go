package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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
	listAt(srv, time.Now(), moved)
	v := srv.verdictViews()[strings.ToLower(pr.Key())]
	if !v.Moved || v.Verdict != store.VerdictApprove || v.Head != headOne {
		t.Errorf("the PR moved: %+v", v)
	}
	if s := srv.summary(context.Background()); len(s.Decided) != 1 || !s.Decided[0].Moved {
		t.Errorf("summary: %+v", s)
	}
}

// The summary's text goes by the PR's author, A to Z without regard to case and
// "unknown" last, a header line each and a blank line between groups; within a
// group by repository, then number. Each PR comes once however many jobs it
// has, and the PRs with no decision are listed apart, by repository and number.
func by(pr gh.PR, author string) gh.PR {
	pr.Author.Login = author
	return pr
}

func TestResumo(t *testing.T) {
	sec213 := by(prAt("klever-io/klever-go-security", 213, headOne), "fbsobreira")
	go206 := by(prAt("klever-io/klever-go", 206, headOne), "RomuloSiebra")
	go203 := by(prAt("klever-io/klever-go", 203, headOne), "fbsobreira")
	go195 := by(prAt("klever-io/klever-go", 195, headOne), "FBSobreira")
	lost := by(prAt("acme/zeta", 1, headOne), "")
	none2 := by(prAt("klever-io/klever-go", 300, headOne), "zed")
	none1 := by(prAt("acme/api-core", 9, headOne), "zed")
	// Cards, newest first: none1, none2, lost, go195, go203, go195 (again), go206, sec213.
	srv, ids := verdictServer(t, sec213, go206, go195, go203, go195, lost, none2, none1)
	for i, v := range map[int]string{0: "approve", 1: "approve", 3: "request_changes", 4: "request_changes", 5: "approve_with_comments"} {
		if code, out := call(t, srv, "POST", "/api/jobs/"+ids[i]+"/verdict", `{"verdict":"`+v+`"}`); code != http.StatusOK {
			t.Fatalf("%s: %d %v", v, code, out)
		}
	}
	code, out := call(t, srv, "GET", "/api/summary", "")
	if code != http.StatusOK {
		t.Fatalf("%d %v", code, out)
	}
	want := "FBSobreira\n" +
		"https://github.com/klever-io/klever-go/pull/195 :git_change_request:\n" +
		"https://github.com/klever-io/klever-go/pull/203 :git_change_request:\n" +
		"https://github.com/klever-io/klever-go-security/pull/213 :white_check_mark:\n" +
		"\n" +
		"RomuloSiebra\n" +
		"https://github.com/klever-io/klever-go/pull/206 :white_check_mark:\n" +
		"\n" +
		"unknown\n" +
		"https://github.com/acme/zeta/pull/1 :memo::white_check_mark:"
	if out["text"] != want {
		t.Errorf("text:\n%v\nwant:\n%s", out["text"], want)
	}
	keys := func(field string) []string {
		var ks []string
		for _, p := range out[field].([]any) {
			ks = append(ks, p.(map[string]any)["key"].(string))
		}
		return ks
	}
	if got, want := keys("decided"), []string{go195.Key(), go203.Key(), sec213.Key(), go206.Key(), lost.Key()}; !slices.Equal(got, want) {
		t.Errorf("decided: %v, want %v", got, want)
	}
	if got, want := keys("undecided"), []string{none1.Key(), none2.Key()}; !slices.Equal(got, want) {
		t.Errorf("undecided: %v, want %v", got, want)
	}
}

// With nothing decided, the text is empty and every PR waits.
func TestResumoSemDecisao(t *testing.T) {
	srv, _ := verdictServer(t, prAt("acme/api-core", 7, headOne))
	s := srv.summary(context.Background())
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
	listAt(srv, time.Now(), now)
	if v := srv.verdictViews()[strings.ToLower(listed.Key())]; v.Moved {
		t.Errorf("the PR is where the review read it, yet: %+v", v)
	}
}

// The summary names who reviews each decided PR, from one read over a fake gh:
// requests pending and reviews, each once, without the author, the user, bots
// or a dismissed review, and a team by org/slug. No @: pasted on GitHub, the
// text mentions nobody. A PR with nobody keeps its line as it was.
func TestResumoComReviewers(t *testing.T) {
	answer := filepath.Join(t.TempDir(), "answer.json")
	if err := os.WriteFile(answer, []byte(`{"data":{
 "p0":{"pullRequest":{"author":{"login":"maria"},
  "reviewRequests":{"nodes":[{"requestedReviewer":{"__typename":"User","login":"fbsobreira"}},
   {"requestedReviewer":{"__typename":"Team","slug":"core","organization":{"login":"klever-io"}}},
   {"requestedReviewer":{"__typename":"User","login":"beroni"}}]},
  "latestReviews":{"nodes":[{"state":"APPROVED","author":{"__typename":"User","login":"RomuloSiebra"}},
   {"state":"COMMENTED","author":{"__typename":"User","login":"FBSOBREIRA"}},
   {"state":"COMMENTED","author":{"__typename":"User","login":"maria"}},
   {"state":"COMMENTED","author":{"__typename":"Bot","login":"coderabbitai"}},
   {"state":"DISMISSED","author":{"__typename":"User","login":"gone"}}]}}},
 "p1":{"pullRequest":{"author":{"login":"maria"},"reviewRequests":{"nodes":[]},"latestReviews":{"nodes":[]}}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	log := ghFalso(t, "cat "+answer)
	a := prAt("klever-io/klever-go", 206, headOne)
	b := prAt("klever-io/klever-go", 195, headOne)
	srv, ids := verdictServer(t, b, a) // cards: a, b
	srv.fetchReviewers = gh.FetchReviewers
	call(t, srv, "POST", "/api/jobs/"+ids[1]+"/verdict", `{"verdict":"approve"}`)
	call(t, srv, "POST", "/api/jobs/"+ids[0]+"/verdict", `{"verdict":"request_changes"}`)
	before := contaNoLog(log, "start")

	code, out := call(t, srv, "GET", "/api/summary", "")
	if code != http.StatusOK {
		t.Fatalf("%d %v", code, out)
	}
	want := "maria\n" + b.URL + " :git_change_request:\n" + a.URL + " :white_check_mark: — fbsobreira, klever-io/core, RomuloSiebra"
	if out["text"] != want {
		t.Errorf("text:\n%v\nwant:\n%s", out["text"], want)
	}
	if strings.Contains(out["text"].(string), "@") {
		t.Error("a login with @ would mention someone")
	}
	decided := out["decided"].([]any)
	if r := decided[1].(map[string]any)["reviewers"]; len(r.([]any)) != 3 {
		t.Errorf("reviewers in the API: %v", r)
	}
	if out["reviewers_error"] != nil {
		t.Errorf("error: %v", out["reviewers_error"])
	}
	if n := contaNoLog(log, "start") - before; n != 1 {
		t.Errorf("%d gh reads for the summary, want one", n)
	}
	// Opened again within the minute: no new read.
	call(t, srv, "GET", "/api/summary", "")
	if n := contaNoLog(log, "start") - before; n != 1 {
		t.Errorf("the second summary read gh again (%d)", n)
	}
}

// gh failing leaves the names out and says so; the summary still comes.
func TestResumoComGhFalhando(t *testing.T) {
	ghFalso(t, "echo 'HTTP 502: Bad Gateway' >&2; exit 1")
	pr := prAt("acme/api-core", 7, headOne)
	srv, ids := verdictServer(t, pr)
	srv.fetchReviewers = gh.FetchReviewers
	call(t, srv, "POST", "/api/jobs/"+ids[0]+"/verdict", `{"verdict":"approve"}`)
	code, out := call(t, srv, "GET", "/api/summary", "")
	if code != http.StatusOK {
		t.Fatalf("%d %v", code, out)
	}
	if out["text"] != "maria\n"+pr.URL+" :white_check_mark:" {
		t.Errorf("text: %v", out["text"])
	}
	if e, _ := out["reviewers_error"].(string); !strings.HasPrefix(e, "reviewers could not be read") {
		t.Errorf("reviewers_error: %q", e)
	}
}

// BUG-QA-38: a PR that moved after the decision is not approved in the text:
// pasted into Slack, the line would vouch for commits nobody read. It goes in
// a list of its own; deciding again on the new review brings it back.
func TestResumoDeixaForaOPRQueAndou(t *testing.T) {
	pr := prAt("acme/api-core", 7, headOne)
	other := prAt("acme/api-core", 8, headOne)
	srv, ids := verdictServer(t, other, pr) // cards: pr, other
	call(t, srv, "POST", "/api/jobs/"+ids[1]+"/verdict", `{"verdict":"approve"}`)
	call(t, srv, "POST", "/api/jobs/"+ids[0]+"/verdict", `{"verdict":"approve"}`)
	moved := pr
	moved.HeadRefOid = headTwo
	listAt(srv, time.Now(), moved, other)

	s := srv.summary(context.Background())
	if s.Text != "maria\n"+other.URL+" :white_check_mark:" {
		t.Errorf("text: %q", s.Text)
	}
	if len(s.Moved) != 1 || s.Moved[0].Key != pr.Key() || !s.Moved[0].Moved {
		t.Errorf("moved: %+v", s.Moved)
	}
	if len(s.Decided) != 2 || !s.Decided[0].Moved {
		t.Errorf("decided: %+v", s.Decided)
	}

	// A new review reads the new head; deciding on it puts the line back.
	ch := srv.hub.Subscribe()
	defer srv.hub.Unsubscribe(ch)
	v, err := srv.jobs.Enqueue(moved, false, srv.cfg.DefaultChoice())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, v.ID, StateDone)
	call(t, srv, "POST", "/api/jobs/"+v.ID+"/verdict", `{"verdict":"request_changes"}`)
	s = srv.summary(context.Background())
	if want := "maria\n" + pr.URL + " :git_change_request:\n" + other.URL + " :white_check_mark:"; s.Text != want || len(s.Moved) != 0 {
		t.Errorf("after deciding again: %q %+v", s.Text, s.Moved)
	}
}

// A decision by hand goes on the head the list read from GitHub, not on the
// review's: after a push, the PR comes back to the summary with no new review.
func TestDecisaoManualNoHeadAtual(t *testing.T) {
	log := ghFalso(t, "exit 1")
	pr := prAt("acme/api-core", 7, headOne)
	srv, ids := verdictServer(t, pr)
	call(t, srv, "POST", "/api/jobs/"+ids[0]+"/verdict", `{"verdict":"approve"}`)
	moved := pr
	moved.HeadRefOid = headTwo
	listAt(srv, time.Now(), moved)
	if s := srv.summary(context.Background()); len(s.Moved) != 1 || s.Text != "" {
		t.Fatalf("before: %+v", s)
	}

	code, out := call(t, srv, "POST", "/api/prs/acme/api-core/7/verdict", `{"verdict":"approve","head":"`+headOne+`"}`)
	if code != http.StatusOK {
		t.Fatalf("%d %v", code, out)
	}
	got, _ := store.LoadMarks(srv.reviewsDir).VerdictOf(pr.Key())
	if got.Head != headTwo || got.Source != store.SourceManual || got.Verdict != store.VerdictApprove {
		t.Errorf("on disk: %+v (the head is the server's, not the page's)", got)
	}
	v := srv.verdictViews()[strings.ToLower(pr.Key())]
	if v.Moved || v.Source != store.SourceManual || v.Current != headTwo {
		t.Errorf("view: %+v", v)
	}
	if s := srv.summary(context.Background()); s.Text != "maria\n"+pr.URL+" :white_check_mark:" || len(s.Moved) != 0 {
		t.Errorf("after: %q %+v", s.Text, s.Moved)
	}
	// A decision under a review says so.
	call(t, srv, "POST", "/api/jobs/"+ids[0]+"/verdict", `{"verdict":"approve"}`)
	if got, _ := store.LoadMarks(srv.reviewsDir).VerdictOf(pr.Key()); got.Source != store.SourceReview {
		t.Errorf("source: %+v", got)
	}
	if n := contaNoLog(log, "start"); n != 0 {
		t.Errorf("gh ran %d times", n)
	}
}

// With the PR out of the list there is no head to decide on: 409, nothing kept.
// The route takes only a watched repository and a number, and refuses a page
// of another site like every door.
func TestDecisaoManualRecusa(t *testing.T) {
	srv := newTestServer(t)
	for path, want := range map[string]int{
		"/api/prs/acme/api-core/7/verdict":  http.StatusConflict,
		"/api/prs/acme/api-core/x/verdict":  http.StatusBadRequest,
		"/api/prs/acme/api-core/0/verdict":  http.StatusBadRequest,
		"/api/prs/evil/api-core/7/verdict":  http.StatusNotFound,
		"/api/prs/acme/elsewhere/7/verdict": http.StatusNotFound,
	} {
		code, out := call(t, srv, "POST", path, `{"verdict":"approve"}`)
		if code != want {
			t.Errorf("%s: %d %v, want %d", path, code, out, want)
		}
		if want == http.StatusConflict && !strings.Contains(fmt.Sprint(out["error"]), "refresh the PR list first") {
			t.Errorf("%s: %v", path, out)
		}
	}
	listAt(srv, time.Now(), prAt("acme/api-core", 7, headOne))
	if code, _ := call(t, srv, "POST", "/api/prs/acme/api-core/7/verdict", `{"verdict":"lgtm"}`); code != http.StatusBadRequest {
		t.Errorf("bad verdict: %d", code)
	}
	req := httptest.NewRequest("POST", "http://127.0.0.1/api/prs/acme/api-core/7/verdict", strings.NewReader(`{"verdict":"approve"}`))
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site: %d", rec.Code)
	}
	if len(store.LoadMarks(srv.reviewsDir).Verdicts()) != 0 {
		t.Error("a refused decision was kept")
	}
}

// listAt puts prs in the server's list as a read from GitHub that began at at.
func listAt(srv *Server, at time.Time, prs ...gh.PR) {
	srv.prMu.Lock()
	defer srv.prMu.Unlock()
	srv.prs = prs
	srv.prsAt = at
	srv.prsReadAt = map[string]time.Time{}
	for _, pr := range prs {
		srv.prsReadAt[strings.ToLower(pr.Key())] = at
	}
}

// PV-1, the rest: the current head is the newest Bazel knows, by time. A list
// read before the review's checkout says less than the clone did: with A in
// it and B in the clone, nothing moved, and a decision by hand ("keep") goes
// on B, not back on A.
func TestListaVelhaNaoVenceOCheckout(t *testing.T) {
	ghFalso(t, "exit 1")
	listed := prAt("acme/api-core", 7, headOne)
	srv := newTestServer(t)
	listAt(srv, time.Now().Add(-time.Minute), listed) // A, read before the review
	ch := srv.hub.Subscribe()
	defer srv.hub.Unsubscribe(ch)
	v, err := srv.jobs.Enqueue(listed, false, srv.cfg.DefaultChoice())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, v.ID, StateDone)
	srv.jobs.mu.Lock()
	srv.jobs.jobs[v.ID].Result.PR.HeadRefOid = headTwo // B, what the clone read
	srv.jobs.mu.Unlock()

	call(t, srv, "POST", "/api/jobs/"+v.ID+"/verdict", `{"verdict":"approve"}`)
	if got := srv.verdictViews()[strings.ToLower(listed.Key())]; got.Moved || got.Current != headTwo {
		t.Errorf("an older list made it move: %+v", got)
	}
	if code, out := call(t, srv, "POST", "/api/prs/acme/api-core/7/verdict", `{"verdict":"approve"}`); code != http.StatusOK {
		t.Fatalf("keep: %d %v", code, out)
	}
	if got, _ := store.LoadMarks(srv.reviewsDir).VerdictOf(listed.Key()); got.Head != headTwo || got.Source != store.SourceManual {
		t.Errorf("keep went on %s, the newest known head is %s: %+v", got.Head, headTwo, got)
	}

	// A list read after the review is newer: its head is the current one.
	moved := listed
	moved.HeadRefOid = "3333333333333333333333333333333333333333"
	listAt(srv, time.Now(), moved)
	if got := srv.verdictViews()[strings.ToLower(listed.Key())]; !got.Moved || got.Current != moved.HeadRefOid {
		t.Errorf("a newer list: %+v", got)
	}
}
