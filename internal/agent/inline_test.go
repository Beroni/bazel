package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/beroni/bazel/internal/config"
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
	fenced := "```markdown\n" + report + "\n```"
	cases := []struct {
		name, in, want, cut string
	}{
		{"narration goes", "Validei o relatório e agora imprimo.\n\n" + report, report, "Validei o relatório e agora imprimo."},
		{"two lines go", "Done.\nPrinting.\n\n" + report, report, "Done. Printing."},
		{"history too", "Done.\n<!-- history-pr:v1\npr: 1\n-->", "<!-- history-pr:v1\npr: 1\n-->", "Done."},
		{"before a wrapped report, the fence stays", "Here it is:\n\n" + fenced, fenced, "Here it is:"},
		{"report alone", report, report, ""},
		{"wrapped report alone", fenced, fenced, ""},
		{"three lines stay", "One.\nTwo.\nThree.\n" + report, "One.\nTwo.\nThree.\n" + report, ""},
		{"a long line stays", strings.Repeat("x", 121) + "\n" + report, strings.Repeat("x", 121) + "\n" + report, ""},
		{"a severity stays", "**critical** SQL injection\n" + report, "**critical** SQL injection\n" + report, ""},
		{"a place in the code stays", "see a.go:12\n" + report, "see a.go:12\n" + report, ""},
		{"a bug stays", "Found a bug.\n" + report, "Found a bug.\n" + report, ""},
		{"a heading stays", "## Step one\n\n" + report, "## Step one\n\n" + report, ""},
		{"a bare fence stays", "Here:\n```\n" + report + "\n```", "Here:\n```\n" + report + "\n```", ""},
		{"a tilde fence stays", "Here:\n~~~\n" + report + "\n~~~", "Here:\n~~~\n" + report + "\n~~~", ""},
		{"prose in a fence stays", "Note:\n```markdown\nsome code\n" + report + "\n```", "Note:\n```markdown\nsome code\n" + report + "\n```", ""},
		{"no marker", "Just a plain review.\n\n## Findings", "Just a plain review.\n\n## Findings", ""},
		{"a marker in a quote", "The header reads\n`<!-- review-fleet:v1`", "The header reads\n`<!-- review-fleet:v1`", ""},
	}
	for _, c := range cases {
		if got, cut := dropPreamble(c.in); got != c.want || cut != c.cut {
			t.Errorf("%s: got %q, cut %q", c.name, got, cut)
		}
	}
}

// wiredCLI puts on PATH a fake agent that saves its prompt, notes whether the
// plugin the prompt names exists, and prints output.
func wiredCLI(t *testing.T, name, output string) string {
	t.Helper()
	bin, state := t.TempDir(), t.TempDir()
	script := "#!/bin/sh\ncat > " + state + "/prompt\n" +
		"plugin=$(sed -n 's/.*`<plugin>` in them is \\([^:]*\\):.*/\\1/p' " + state + "/prompt | head -1)\n" +
		"[ -f \"$plugin/skills/bazel-review-fleet/SKILL.md\" ] && touch " + state + "/plugin-there\n" +
		"for a in \"$@\"; do [ \"$a\" = --plugin-dir ] && touch " + state + "/plugin-flag; done\n" +
		"cat <<'OUT'\n" + output + "\nOUT\n"
	if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return state
}

func wiredReview(t *testing.T, def config.AgentDef) (Result, []string, error) {
	t.Helper()
	hostileClone(t)
	cfg := config.Default()
	cfg.Agent.Checkout = true
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	def.Name = "lens"
	cfg.Agents = []config.AgentDef{def}
	choice, err := cfg.ChoiceByName("lens")
	if err != nil {
		t.Fatal(err)
	}
	pr := testPR()
	pr.BaseRefName = ""
	var log []string
	res, err := New(cfg).Review(context.Background(), pr, choice, func(e Event) {
		if e.Kind == EventLog {
			log = append(log, e.Text)
		}
	})
	return res, log, err
}

// The helpers above are tested on their own; these go through Review, so that
// unplugging one from the run fails too.

func TestRunInlinesForOtherCLIs(t *testing.T) {
	state := wiredCLI(t, "codex", "report")
	if _, _, err := wiredReview(t, config.AgentDef{Command: "codex", Format: "plain", Task: "/bazel-review-fleet {{number}}"}); err != nil {
		t.Fatalf("Review: %v", err)
	}
	prompt, _ := os.ReadFile(filepath.Join(state, "prompt"))
	if !strings.Contains(string(prompt), "# Bazel Review Fleet") || !strings.Contains(string(prompt), "Arguments: 482") {
		t.Errorf("another CLI gets the skill in the prompt:\n%.300s", prompt)
	}
	if _, err := os.Stat(filepath.Join(state, "plugin-there")); err != nil {
		t.Error("the plugin the prompt names should exist while the agent runs")
	}
	if _, err := os.Stat(filepath.Join(state, "plugin-flag")); err == nil {
		t.Error("--plugin-dir is Claude Code's")
	}
}

func TestRunCanonicalizesTheTask(t *testing.T) {
	state := wiredCLI(t, "claude", "report")
	if _, _, err := wiredReview(t, config.AgentDef{Command: "claude", Format: "plain", Task: "/BAZEL-REVIEW-FLEET {{number}}"}); err != nil {
		t.Fatalf("Review: %v", err)
	}
	prompt, _ := os.ReadFile(filepath.Join(state, "prompt"))
	if !strings.HasPrefix(string(prompt), "/bazel-review-fleet 482") {
		t.Errorf("the slash command should carry the skill's exact name: %.60q", prompt)
	}
}

func TestRunCutsPreambleOnlyForBuiltins(t *testing.T) {
	output := "Validated, printing now.\n\n<!-- review-fleet:v1\nrepo: a/b\n-->"
	wiredCLI(t, "claude", output)
	res, log, err := wiredReview(t, config.AgentDef{Command: "claude", Format: "plain", Task: "/bazel-review-fleet {{number}}"})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if !strings.HasPrefix(res.Body, "<!-- review-fleet:v1") {
		t.Errorf("a built-in's preamble goes: %q", res.Body)
	}
	if !slices.ContainsFunc(log, func(l string) bool { return strings.Contains(l, "cut from before the report") && strings.Contains(l, "Validated") }) {
		t.Errorf("what was cut should be in the job's log: %q", log)
	}

	res, _, err = wiredReview(t, config.AgentDef{Command: "claude", Format: "plain", Task: "/my-own-skill {{number}}"})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if !strings.HasPrefix(res.Body, "Validated") {
		t.Errorf("someone else's output is left alone: %q", res.Body)
	}
}
