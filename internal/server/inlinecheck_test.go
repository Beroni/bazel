package server

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

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
