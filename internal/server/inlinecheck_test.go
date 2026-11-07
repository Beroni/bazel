package server

import (
	"context"
	"github.com/beroni/bazel/internal/config"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/beroni/bazel/internal/agent"
	"github.com/beroni/bazel/internal/gh"
	"github.com/beroni/bazel/internal/store"
)

// The publisher's own reader runs over the saved file: a report in the format
// passes, and one that is not says why, before anyone clicks Publish.
func TestInlineCheckUsaOLeitorDoPublicador(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}
	good, err := os.ReadFile("../skills/testdata/reports/fleet.md")
	if err != nil {
		t.Fatal(err)
	}
	pr := gh.PR{Repo: "klever/app", Number: 123, Title: "Add transfer endpoint", HeadRefOid: "a1b2c3d4e5f67890abcdef1234567890abcdef12"}
	dir := t.TempDir()
	for _, c := range []struct {
		body, want string
	}{
		{string(good), ""},
		{"No issues found.", "no review-fleet header"},
		{strings.Replace(string(good), "-->", "", 1), "not closed"},
	} {
		path, err := store.Save(dir, agent.Result{Agent: "fleet", PR: pr, Body: c.body}, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := inlineCheck(context.Background(), path)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%.30q: %q, want %q", c.body, got, c.want)
		}
	}
}

// The Saved list checks a few files at a time and gives up at its budget: what
// it did not get to says unchecked, and is not cached as a verdict.
func TestListaDoSavedTemTeto(t *testing.T) {
	t.Setenv("BAZEL_HOME", t.TempDir())
	srv := newTestServer(t)
	oldCheck, oldBudget, oldN := inlineCheck, savedCheckBudget, savedCheckers
	t.Cleanup(func() { inlineCheck, savedCheckBudget, savedCheckers = oldCheck, oldBudget, oldN })
	inlineCheck = func(ctx context.Context, path string) string {
		select {
		case <-time.After(200 * time.Millisecond):
			return "no header"
		case <-ctx.Done():
			return "it could not be read"
		}
	}
	savedCheckBudget, savedCheckers = 300*time.Millisecond, 2
	for i := range 10 {
		pr := testPR(500 + i)
		if _, err := store.Save(srv.reviewsDir, agent.Result{Agent: srv.cfg.DefaultChoice().Name, PR: pr, Body: "x"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	srv.cfg.PostAgent = config.AgentDef{Name: "bazel-post-report", Task: "/bazel-post-report {{review_file}}"}
	start := time.Now()
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:7777/api/reviews", nil))
	if took := time.Since(start); took > time.Second {
		t.Errorf("the list took %v", took)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"inline_unchecked":true`) || !strings.Contains(body, `"inline_issue":"no header"`) {
		t.Errorf("list: %.400s", body)
	}
}
