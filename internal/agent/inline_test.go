package agent

import (
	"strings"
	"testing"

	"github.com/beroni/bazel/internal/skills"
)

// The inlined skill still goes through the template: its placeholders expand
// like any task's.
func TestInlinedTaskInPrompt(t *testing.T) {
	task := skills.Inline("/bazel-review-fleet {{number}}", "/rt/plugin")
	got := buildPrompt("{{task}}\n\nRepo {{repo}}", task, testPR(), "", "/tmp/clone", nil)
	if !strings.Contains(got, "Arguments: 482") {
		t.Errorf("the PR number should reach the inlined skill:\n%s", got[:300])
	}
	if !strings.Contains(got, "/rt/plugin/skills/bazel-review-fleet/") {
		t.Error("the inlined skill should say where its files are, as an absolute path")
	}
	if !strings.Contains(got, "# Bazel Review Fleet") || !strings.Contains(got, "Repo acme/api-core") {
		t.Error("the prompt should carry the skill body and the rest of the template")
	}
}

func TestDropPreamble(t *testing.T) {
	report := "<!-- review-fleet:v1\nrepo: a/b\n-->\n\n# Review Fleet — PR #1 · x"
	cases := []struct {
		name, in, want string
	}{
		{"preamble goes", "Validei o relatório e agora imprimo.\n\n" + report, report},
		{"history preamble goes", "Done.\n<!-- history-pr:v1\npr: 1\n-->", "<!-- history-pr:v1\npr: 1\n-->"},
		{"report alone stays", report, report},
		{"fenced report is Unwrap's", "```markdown\n" + report + "\n```", "```markdown\n" + report + "\n```"},
		{"a heading before is not a preamble", "## Step one\n\n" + report, "## Step one\n\n" + report},
		{"no marker stays", "Just a plain review.\n\n## Findings", "Just a plain review.\n\n## Findings"},
		{"marker in a quote stays", "The header reads\n`<!-- review-fleet:v1`", "The header reads\n`<!-- review-fleet:v1`"},
	}
	for _, c := range cases {
		if got := dropPreamble(c.in); got != c.want {
			t.Errorf("%s: got %q", c.name, got)
		}
	}
}
