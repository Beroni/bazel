package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/beroni/bazel/internal/agent"
	"github.com/beroni/bazel/internal/gh"
	"github.com/beroni/bazel/internal/store"
)

// prsServer returns a server with four cached PRs, one per review state, so
// the list never calls gh:
//
//	acme/api-core#1  maria  no review
//	acme/api-core#2  beroni reviewed, same commit
//	other/lib#3      joao   reviewed, but the PR got a commit later
//	acme/api-core#4  maria  reviewed and published
func prsServer(t *testing.T) *Server {
	t.Helper()
	srv := newTestServer(t)
	mk := func(repo string, n int, author, oid string) gh.PR {
		pr := testPR(n)
		pr.Repo = repo
		pr.Author.Login = author
		pr.HeadRefOid = oid
		return pr
	}
	prs := []gh.PR{
		mk("acme/api-core", 1, "maria", "a1"),
		mk("acme/api-core", 2, "beroni", "b2"),
		mk("other/lib", 3, "joao", "c3-novo"),
		mk("acme/api-core", 4, "maria", "d4"),
	}
	srv.prs = prs
	srv.prsAt = time.Now()

	mark := func(pr gh.PR, oid string) {
		pr.HeadRefOid = oid
		if err := store.MarkReviewed(srv.reviewsDir, agent.Result{PR: pr, Agent: "lente"}, ""); err != nil {
			t.Fatalf("MarkReviewed: %v", err)
		}
	}
	mark(prs[1], "b2")
	mark(prs[2], "c3-velho")
	mark(prs[3], "d4")
	if err := store.MarkPosted(srv.reviewsDir, prs[3].Key()); err != nil {
		t.Fatalf("MarkPosted: %v", err)
	}
	return srv
}

func keysOf(t *testing.T, out map[string]any) []string {
	t.Helper()
	var keys []string
	prs, _ := out["prs"].([]any)
	for _, p := range prs {
		keys = append(keys, p.(map[string]any)["key"].(string))
	}
	slices.Sort(keys)
	return keys
}

func TestMCPListPRsFilters(t *testing.T) {
	h := prsServer(t).Handler()
	cases := []struct {
		nome string
		args map[string]any
		want []string
	}{
		{"sem filtro", map[string]any{}, []string{"acme/api-core#1", "acme/api-core#2", "acme/api-core#4", "other/lib#3"}},
		{"any", map[string]any{"status": "any"}, []string{"acme/api-core#1", "acme/api-core#2", "acme/api-core#4", "other/lib#3"}},
		{"unreviewed", map[string]any{"status": "unreviewed"}, []string{"acme/api-core#1"}},
		{"reviewed", map[string]any{"status": "reviewed"}, []string{"acme/api-core#2", "acme/api-core#4", "other/lib#3"}},
		{"changed", map[string]any{"status": "changed"}, []string{"other/lib#3"}},
		{"posted", map[string]any{"status": "posted"}, []string{"acme/api-core#4"}},
		{"repo", map[string]any{"repo": "other/lib"}, []string{"other/lib#3"}},
		{"repo em outra caixa", map[string]any{"repo": " ACME/Api-Core "}, []string{"acme/api-core#1", "acme/api-core#2", "acme/api-core#4"}},
		{"mine", map[string]any{"mine": true}, []string{"acme/api-core#2"}},
		{"author com @ e outra caixa", map[string]any{"author": "@MARIA"}, []string{"acme/api-core#1", "acme/api-core#4"}},
		{"repo e status", map[string]any{"repo": "acme/api-core", "status": "reviewed"}, []string{"acme/api-core#2", "acme/api-core#4"}},
		{"author e status", map[string]any{"author": "maria", "status": "posted"}, []string{"acme/api-core#4"}},
		{"mine e unreviewed não se cruzam", map[string]any{"mine": true, "status": "unreviewed"}, nil},
	}
	for _, c := range cases {
		t.Run(c.nome, func(t *testing.T) {
			got := keysOf(t, mcpToolCall(t, h, "list_prs", c.args))
			if !slices.Equal(got, c.want) {
				t.Errorf("list_prs(%v) = %v, queria %v", c.args, got, c.want)
			}
		})
	}
}

func TestMCPListPRsStatusFlags(t *testing.T) {
	h := prsServer(t).Handler()
	out := mcpToolCall(t, h, "list_prs", map[string]any{})
	by := map[string]map[string]any{}
	for _, p := range out["prs"].([]any) {
		v := p.(map[string]any)
		by[v["key"].(string)] = v
	}
	if by["acme/api-core#1"]["reviewed"] != nil {
		t.Errorf("#1 não foi revisado: %v", by["acme/api-core#1"])
	}
	if by["other/lib#3"]["changed_since_review"] != true {
		t.Errorf("#3 mudou depois do review: %v", by["other/lib#3"])
	}
	if by["acme/api-core#4"]["review_posted"] != true {
		t.Errorf("#4 foi publicado: %v", by["acme/api-core#4"])
	}
	if by["acme/api-core#2"]["review_posted"] != nil || by["acme/api-core#2"]["changed_since_review"] != nil {
		t.Errorf("#2 está revisado e em dia: %v", by["acme/api-core#2"])
	}
}

func TestMCPListPRsInvalidStatus(t *testing.T) {
	h := prsServer(t).Handler()
	for _, st := range []string{"nope", "REVIEWED", "mine"} {
		res := mcpToolResult(t, h, "list_prs", map[string]any{"status": st})
		if res["isError"] != true {
			t.Errorf("status %q devia ser erro da ferramenta: %v", st, res)
		}
	}
}

// The page uses the same list: scope=mine and author match the MCP filter.
func TestAPIPRsScopeAndAuthor(t *testing.T) {
	h := prsServer(t).Handler()
	get := func(query string) map[string]any {
		t.Helper()
		r := httptest.NewRequest("GET", "http://127.0.0.1:7777/api/prs"+query, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /api/prs%s: %d %s", query, w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	cases := []struct {
		query string
		want  []string
	}{
		{"", []string{"acme/api-core#1", "acme/api-core#2", "acme/api-core#4", "other/lib#3"}},
		{"?scope=mine", []string{"acme/api-core#2"}},
		{"?author=joao", []string{"other/lib#3"}},
		{"?author=@Maria", []string{"acme/api-core#1", "acme/api-core#4"}},
		{"?scope=mine&author=maria", nil},
		{"?scope=all&author=maria", []string{"acme/api-core#1", "acme/api-core#4"}},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.query), func(t *testing.T) {
			if got := keysOf(t, get(c.query)); !slices.Equal(got, c.want) {
				t.Errorf("GET /api/prs%s = %v, queria %v", c.query, got, c.want)
			}
		})
	}
}
