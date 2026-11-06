package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beroni/bazel/internal/gh"
)

// needsFixture builds a PR in the shape of the GraphQL answer, from short
// pieces: t(n) is n hours after a fixed start.
func t0(h int) string {
	return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(h) * time.Hour).Format(time.RFC3339)
}

type np struct {
	author                        string
	requested, reviewed, authored bool
	decision, mergeable, checks   string
	draft                         bool
	pushed                        int // hour of the last commit, -1 for none
	forcePushed                   int // hour of the last force push, -1 for none
	requests                      []string
	reviews, comments, threads    []string
}

// pr turns np into gh.NeedsPR. requests are "login@h" or "team:slug@h";
// reviews "login STATE@h id"; comments "login@h" ("x[bot]@h" for a bot);
// threads "first@h review open|resolved:by last@h".
func (n np) pr(t *testing.T) gh.NeedsPR {
	t.Helper()
	at := func(s string) (string, string) { a, b, _ := strings.Cut(s, "@"); return a, b }
	hour := func(s string) string {
		var h int
		fmt.Sscanf(s, "%d", &h)
		return t0(h)
	}
	var reqs, revs, coms, ths []string
	for _, r := range n.requests {
		who, h := at(r)
		rr := fmt.Sprintf(`{"__typename":"User","login":%q}`, who)
		if slug, ok := strings.CutPrefix(who, "team:"); ok {
			rr = fmt.Sprintf(`{"__typename":"Team","slug":%q}`, slug)
		}
		reqs = append(reqs, fmt.Sprintf(`{"createdAt":%q,"requestedReviewer":%s}`, hour(h), rr))
	}
	for _, r := range n.reviews {
		f := strings.Fields(r)
		who, st := f[0], f[1]
		state, h := at(st)
		id := "r-" + who + h
		if len(f) > 2 {
			id = f[2]
		}
		revs = append(revs, fmt.Sprintf(`{"id":%q,"author":{"login":%q},"state":%q,"submittedAt":%q}`, id, who, state, hour(h)))
	}
	actor := func(who string) string {
		tn := "User"
		if strings.HasSuffix(who, "[bot]") {
			tn = "Bot"
		}
		return fmt.Sprintf(`{"__typename":%q,"login":%q}`, tn, who)
	}
	for _, c := range n.comments {
		who, h := at(c)
		coms = append(coms, fmt.Sprintf(`{"author":%s,"createdAt":%q}`, actor(who), hour(h)))
	}
	for _, th := range n.threads {
		f := strings.Fields(th)
		fw, fh := at(f[0])
		review := f[1]
		state, by, _ := strings.Cut(f[2], ":")
		lw, lh := at(f[3])
		resolvedBy := "null"
		if by != "" {
			resolvedBy = fmt.Sprintf(`{"login":%q}`, by)
		}
		ths = append(ths, fmt.Sprintf(`{"isResolved":%v,"resolvedBy":%s,"firstComment":{"nodes":[{"author":{"login":%q},"createdAt":%q,"pullRequestReview":{"id":%q}}]},"lastComment":{"nodes":[{"author":%s,"createdAt":%q}]}}`,
			state == "resolved", resolvedBy, fw, hour(fh), review, actor(lw), hour(lh)))
	}
	commit := `{"committedDate":` + fmt.Sprintf("%q", t0(0))
	if n.pushed >= 0 {
		commit = `{"committedDate":` + fmt.Sprintf("%q", t0(n.pushed))
	}
	if n.checks != "" {
		commit += fmt.Sprintf(`,"statusCheckRollup":{"state":%q}}`, n.checks)
	} else {
		commit += "}"
	}
	force := ""
	if n.forcePushed > 0 {
		force = fmt.Sprintf(`{"createdAt":%q}`, t0(n.forcePushed))
	}
	src := fmt.Sprintf(`{"number":1,"title":"T","url":"u","isDraft":%v,"updatedAt":%q,"baseRefName":"main","reviewDecision":%q,"mergeable":%q,
		"author":{"login":%q},"repository":{"nameWithOwner":"acme/api"},
		"commits":{"nodes":[{"commit":%s}]},"forcePushes":{"nodes":[%s]},
		"requests":{"nodes":[%s]},"reviews":{"nodes":[%s]},"comments":{"nodes":[%s]},"reviewThreads":{"nodes":[%s]}}`,
		n.draft, t0(100), n.decision, n.mergeable, n.author, commit, force,
		strings.Join(reqs, ","), strings.Join(revs, ","), strings.Join(coms, ","), strings.Join(ths, ","))
	var p gh.NeedsPR
	if err := json.Unmarshal([]byte(src), &p); err != nil {
		t.Fatalf("%v\n%s", err, src)
	}
	p.Requested, p.Reviewed, p.Authored = n.requested, n.reviewed, n.authored
	return p
}

func TestClassifyNeeds(t *testing.T) {
	cases := []struct {
		name     string
		pr       np
		category string
		reason   string // a part of it
		since    int    // hour, -1 to skip
	}{
		// 1. Asked to review.
		{"pedido sem review meu", np{author: "bob", requested: true, requests: []string{"me@5"}}, needsRequested, "review requested", 5},
		{"pedido a um time meu", np{author: "bob", requested: true, requests: []string{"team:core@6"}}, needsRequested, "review requested", 6},
		{"pedido a outra pessoa não conta como o meu", np{author: "bob", requested: true, requests: []string{"me@5", "carol@9"}}, needsRequested, "", 5},
		{"pedido de novo depois do meu review", np{author: "bob", requested: true, requests: []string{"me@10"}, reviews: []string{"me APPROVED@8"}}, needsRequested, "", 10},
		{"meu review depois do pedido responde", np{author: "bob", requested: true, requests: []string{"team:core@5"}, reviews: []string{"me COMMENTED@8"}}, "", "", -1},
		{"review pendente (rascunho) não responde", np{author: "bob", requested: true, requests: []string{"me@5"}, reviews: []string{"me PENDING@8"}}, needsRequested, "", 5},
		{"pedido acima de pedi mudanças", np{author: "bob", requested: true, reviewed: true, requests: []string{"me@20"}, reviews: []string{"me CHANGES_REQUESTED@10"}, pushed: 15}, needsRequested, "", 20},

		// 2 and 3. I asked for changes.
		{"pedi mudanças, autor fez push depois", np{author: "bob", reviewed: true, reviews: []string{"me CHANGES_REQUESTED@10"}, pushed: 12}, needsReplied, "@bob pushed", 12},
		{"pedi mudanças, push antes do review não conta", np{author: "bob", reviewed: true, reviews: []string{"me CHANGES_REQUESTED@10"}, pushed: 9}, needsWaiting, "waiting for @bob", 10},
		{"force push depois conta", np{author: "bob", reviewed: true, reviews: []string{"me CHANGES_REQUESTED@10"}, pushed: 2, forcePushed: 11}, needsReplied, "pushed", 11},
		{"autor comentou depois", np{author: "bob", reviewed: true, reviews: []string{"me CHANGES_REQUESTED@10"}, pushed: 1, comments: []string{"bob@13"}}, needsReplied, "@bob commented", 13},
		{"comentário de outro não é resposta", np{author: "bob", reviewed: true, reviews: []string{"me CHANGES_REQUESTED@10"}, pushed: 1, comments: []string{"carol@13", "ci[bot]@14"}}, needsWaiting, "", 10},
		{"autor respondeu numa thread", np{author: "bob", reviewed: true, reviews: []string{"me CHANGES_REQUESTED@10 rv1"}, pushed: 1, threads: []string{"me@9 rv1 open bob@14"}}, needsReplied, "replied in a thread", 14},
		{"autor resolveu thread do review que pediu", np{author: "bob", reviewed: true, reviews: []string{"me CHANGES_REQUESTED@10 rv1"}, pushed: 1, threads: []string{"me@9 rv1 resolved:bob me@9"}}, needsReplied, "resolved your threads", 10},
		{"thread resolvida de outro review não conta", np{author: "bob", reviewed: true, reviews: []string{"me CHANGES_REQUESTED@10 rv2"}, pushed: 1, threads: []string{"me@3 rv1 resolved:bob me@3"}}, needsWaiting, "", 10},
		{"thread resolvida por outro não conta", np{author: "bob", reviewed: true, reviews: []string{"me CHANGES_REQUESTED@10 rv1"}, pushed: 1, threads: []string{"me@9 rv1 resolved:carol me@9"}}, needsWaiting, "", 10},
		{"aprovei depois de pedir: fora", np{author: "bob", reviewed: true, reviews: []string{"me CHANGES_REQUESTED@10", "me APPROVED@20"}, pushed: 15}, "", "", -1},
		{"aprovei, mas minha thread segue aberta", np{author: "bob", reviewed: true, reviews: []string{"me APPROVED@20 rv1"}, pushed: 1, threads: []string{"me@19 rv1 open me@19"}}, needsWaiting, "1 open thread", 19},
		{"thread aberta minha, autor fez push depois", np{author: "bob", reviewed: true, reviews: []string{"me COMMENTED@8 rv1"}, pushed: 12, threads: []string{"me@8 rv1 open me@8"}}, needsReplied, "pushed", 12},
		{"minha última fala na thread é depois do push", np{author: "bob", reviewed: true, reviews: []string{"me COMMENTED@8 rv1"}, pushed: 12, threads: []string{"me@8 rv1 open me@13"}}, needsWaiting, "", 13},
		{"comentário depois de pedir mudanças não encerra o pedido", np{author: "bob", reviewed: true, reviews: []string{"me CHANGES_REQUESTED@10", "me COMMENTED@11"}, pushed: 9}, needsWaiting, "you asked for changes", 10},
		{"comentário depois do pedido, autor fez push depois", np{author: "bob", reviewed: true, reviews: []string{"me CHANGES_REQUESTED@10", "me COMMENTED@11"}, pushed: 12}, needsReplied, "@bob pushed", 12},
		{"pedido dispensado encerra", np{author: "bob", reviewed: true, reviews: []string{"me CHANGES_REQUESTED@10", "me DISMISSED@11"}, pushed: 9}, "", "", -1},
		{"só comentei: fora", np{author: "bob", reviewed: true, reviews: []string{"me COMMENTED@8"}, pushed: 12}, "", "", -1},

		// 4. My own PRs.
		{"meu: mudanças pedidas", np{author: "me", authored: true, decision: "CHANGES_REQUESTED", reviews: []string{"carol CHANGES_REQUESTED@7"}, pushed: 2}, needsYours, "changes requested by @carol", 7},
		{"meu: thread esperando", np{author: "me", authored: true, pushed: 2, threads: []string{"carol@5 rv1 open carol@6"}}, needsYours, "1 thread waiting", 6},
		{"meu: thread em que respondi por último", np{author: "me", authored: true, pushed: 2, threads: []string{"carol@5 rv1 open me@6"}}, "", "", -1},
		{"meu: comentário novo", np{author: "me", authored: true, pushed: 2, comments: []string{"carol@4"}}, needsYours, "new comment from @carol", 4},
		{"meu: comentário antes do meu push não é novo", np{author: "me", authored: true, pushed: 5, comments: []string{"carol@4"}}, "", "", -1},
		{"meu: bot não conta", np{author: "me", authored: true, pushed: 2, comments: []string{"deploy[bot]@4"}}, "", "", -1},
		{"meu: CI falhando", np{author: "me", authored: true, pushed: 2, checks: "FAILURE"}, needsYours, "checks failing", 2},
		{"meu: conflito", np{author: "me", authored: true, pushed: 2, mergeable: "CONFLICTING"}, needsYours, "conflicts with main", 100},
		{"meu: aprovado e pronto", np{author: "me", authored: true, pushed: 2, decision: "APPROVED", mergeable: "MERGEABLE", checks: "SUCCESS", reviews: []string{"carol APPROVED@8"}}, needsYours, "ready to merge", 8},
		{"meu: aprovado com CI pendente não está pronto", np{author: "me", authored: true, pushed: 2, decision: "APPROVED", mergeable: "MERGEABLE", checks: "PENDING"}, "", "", -1},
		{"meu: rascunho aprovado não está pronto", np{author: "me", authored: true, pushed: 2, draft: true, decision: "APPROVED", mergeable: "MERGEABLE"}, "", "", -1},
		{"meu: nada", np{author: "me", authored: true, pushed: 2}, "", "", -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.pr.pushed == 0 && c.pr.forcePushed == 0 {
				c.pr.pushed = -1
			}
			cat, reason, since := classifyNeeds("me", c.pr.pr(t))
			if cat != c.category || !strings.Contains(reason, c.reason) {
				t.Fatalf("got %q %q, want %q %q", cat, reason, c.category, c.reason)
			}
			if c.since >= 0 && since.Format(time.RFC3339) != t0(c.since) {
				t.Errorf("since %v, want hour %d", since, c.since)
			}
		})
	}
}

// The login goes into a search query: anything that is not a login is refused
// before gh runs.
func TestFetchNeedsRecusaLoginEstranho(t *testing.T) {
	for _, login := range []string{"", "a b", "me repo:x/y", "-me", strings.Repeat("a", 40)} {
		if _, _, err := gh.FetchNeeds(t.Context(), login); err == nil || !strings.Contains(err.Error(), "invalid GitHub login") {
			t.Errorf("%q: %v", login, err)
		}
	}
}

// One gh call answers the page: the watched repos by default, all with all=1,
// the cache in between, and nothing written.
func TestHandleNeeds(t *testing.T) {
	t.Setenv("BAZEL_HOME", t.TempDir())
	mine := np{author: "bob", requested: true, requests: []string{"me@5"}}.pr(t)
	other := np{author: "me", authored: true, checks: "FAILURE", pushed: 3}.pr(t)
	other.Repository.NameWithOwner, other.Number = "elsewhere/x", 9
	none := np{author: "bob", reviewed: true, reviews: []string{"me APPROVED@8"}}.pr(t)
	none.Number = 2
	enc := func(ps ...gh.NeedsPR) string {
		b, _ := json.Marshal(map[string]any{"issueCount": len(ps), "nodes": ps})
		return string(b)
	}
	answer := `{"data":{"requested":` + enc(mine) + `,"reviewed":` + enc(none) + `,"authored":` + enc(other) + `}}`
	bin, dir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "answer.json"), []byte(answer), 0o600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "calls")
	script := "#!/bin/sh\nprintf '%s ' \"$@\" | tr '\\n' ' ' >> " + log + "\necho >> " + log + "\ncat " + filepath.Join(dir, "answer.json") + "\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := cfgFor(t, "echo")
	cfg.Repos = []string{"ACME/api"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := New(ctx, cfg, "me", Options{Addr: "127.0.0.1:0", Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	get := func(q string) (items []needsItem, elsewhere int) {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:7777/api/needs"+q, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", q, w.Code, w.Body)
		}
		var body struct {
			Items     []needsItem `json:"items"`
			Elsewhere int         `json:"elsewhere"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return body.Items, body.Elsewhere
	}
	items, elsewhere := get("")
	if len(items) != 1 || items[0].Key != "acme/api#1" || items[0].Category != needsRequested || !items[0].Watched || elsewhere != 1 {
		t.Fatalf("watched: %+v %d", items, elsewhere)
	}
	items, _ = get("?all=1")
	if len(items) != 2 || items[1].Category != needsYours || items[1].Reason != "checks failing" || items[1].Watched || !items[1].Mine {
		t.Fatalf("all: %+v", items)
	}
	calls, _ := os.ReadFile(log)
	if n := strings.Count(string(calls), "\n"); n != 1 || !strings.HasPrefix(string(calls), "api graphql") || !strings.Contains(string(calls), "review-requested:me") {
		t.Errorf("calls:\n%s", calls)
	}
	get("?refresh=1")
	calls, _ = os.ReadFile(log)
	if strings.Count(string(calls), "\n") != 2 {
		t.Errorf("refresh did not read again:\n%s", calls)
	}
}

// A read that fails after a good one keeps the good list on the page, says it
// is stale, and waits before trying GitHub again.
func TestNeedsFalhaMantemListaEEspera(t *testing.T) {
	t.Setenv("BAZEL_HOME", t.TempDir())
	p := np{author: "bob", requested: true, requests: []string{"me@5"}}.pr(t)
	b, _ := json.Marshal(map[string]any{"issueCount": 1, "nodes": []gh.NeedsPR{p}})
	empty := `{"issueCount":0,"nodes":[]}`
	answer := `{"data":{"requested":` + string(b) + `,"reviewed":` + empty + `,"authored":` + empty + `}}`
	bin, dir := t.TempDir(), t.TempDir()
	failFlag, log := filepath.Join(dir, "fail"), filepath.Join(dir, "calls")
	_ = os.WriteFile(filepath.Join(dir, "answer.json"), []byte(answer), 0o600)
	script := "#!/bin/sh\necho x >> " + log + "\nif [ -e " + failFlag + " ]; then echo 'HTTP 502' >&2; exit 1; fi\ncat " + filepath.Join(dir, "answer.json") + "\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	now := time.Now()
	old := timeNow
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = old })
	cfg := cfgFor(t, "echo")
	cfg.Repos = []string{"acme/api"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := New(ctx, cfg, "me", Options{Addr: "127.0.0.1:0", Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	get := func(q string) (int, map[string]any) {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:7777/api/needs"+q, nil))
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body
	}
	calls := func() int { b, _ := os.ReadFile(log); return strings.Count(string(b), "x") }
	if code, body := get(""); code != 200 || body["stale"] != "" {
		t.Fatalf("first: %d %v", code, body)
	}
	_ = os.WriteFile(failFlag, nil, 0o600)
	now = now.Add(2 * time.Minute)
	for range 5 {
		code, body := get("?refresh=1")
		items, _ := body["items"].([]any)
		if code != 200 || len(items) != 1 || !strings.Contains(fmt.Sprint(body["stale"]), "HTTP 502") {
			t.Fatalf("after failure: %d %v", code, body)
		}
	}
	if calls() != 2 {
		t.Errorf("gh ran %d times; the failure should hold the next reads back", calls())
	}
	now = now.Add(needsBackoff + time.Second)
	get("")
	if calls() != 3 {
		t.Errorf("after the backoff: %d calls", calls())
	}
	now = now.Add(needsBackoff + time.Second) // the second failure waits twice as long
	get("")
	if calls() != 3 {
		t.Errorf("backoff did not grow: %d calls", calls())
	}
	_ = os.Remove(failFlag)
	now = now.Add(needsBackoff)
	if _, body := get(""); body["stale"] != "" || calls() != 4 {
		t.Errorf("recovered: %v, %d calls", body, calls())
	}
}
