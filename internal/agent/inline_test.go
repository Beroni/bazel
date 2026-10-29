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
