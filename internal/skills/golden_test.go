package skills_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beroni/bazel/internal/skills"
	"github.com/beroni/bazel/internal/store"
)

// The golden reports are what the built-in skills are told to print. Each one
// is checked against the two readers that matter: validate-report.sh, which
// the skills run before printing, and store.SplitFindings, which decides
// where the page puts a checkbox. A skill whose format drifts from either
// shows up here before it shows up on a PR.
var golden = []struct {
	file     string
	wantErr  string // a line validate-report.sh must print; "" = it must pass
	findings int    // parts SplitFindings marks as findings
}{
	{"fleet.md", "", 4},               // 2 findings, 1 cut, 1 needs-human item
	{"exploit-needs-human.md", "", 3}, // 1 finding, 2 needs-human items
	// Cuts go by lines_saved alone: a major cut after a bigger minor one.
	{"cuts-order.md", "", 5},
	{"single-lens-clean.md", "", 0},
	// A failure report must never pass as a review: files: 0 is rejected on
	// purpose, and there is nothing to tick.
	{"failure.md", "`files: 0` is a scope-resolution failure", 0},
	// A PR history is not a review: not the review format, no checkboxes.
	{"history.md", "line 1 must be exactly `<!-- review-fleet:v1`", 0},
}

// The rules about order, verdicts and how a report ends, broken on purpose: one
// that stops firing fails here, and a report that should be fixed does not get
// through.
func TestValidatorRefuses(t *testing.T) {
	validate := validator(t)
	fleet := readGolden(t, "fleet.md")
	cuts := readGolden(t, "cuts-order.md")
	stops := "the report stops before its title and summary are complete"
	cases := map[string]struct{ body, want string }{
		"the header alone":      {fleet[:strings.Index(fleet, "# Review Fleet")], stops},
		"cut after the title":   {fleet[:strings.Index(fleet, "**Verdict:**")], stops},
		"cut after the verdict": {fleet[:strings.Index(fleet, "**Scope:**")], stops},
		"cut after the scope":   {fleet[:strings.Index(fleet, "**Lenses:**")], stops},
		"cuts out of order": {
			strings.Replace(strings.Replace(strings.Replace(strings.Replace(cuts, "lines_saved=40", "lines_saved=X", 1), "lines_saved=25", "lines_saved=40", 1), "lines_saved=X", "lines_saved=25", 1), "−25 lines", "−40 lines", 1),
			"from the biggest",
		},
		"findings out of severity order": {
			strings.NewReplacer("severity=critical", "severity=minor", "severity=minor", "severity=critical",
				"**critical · authz**", "**minor · authz**", "**minor · test-gap**", "**critical · test-gap**").Replace(fleet),
			"after a lower severity",
		},
		"comment with no finding": {
			fleet[:strings.Index(fleet, "## Findings")],
			"with no finding and no human-verification item",
		},
		"approve with a question": {
			strings.ReplaceAll(fleet[:strings.Index(fleet, "## Findings")], "request_changes", "approve") + fleet[strings.Index(fleet, "## Needs human"):],
			"open human-verification item",
		},
		"comment with a critical": {
			strings.ReplaceAll(fleet, "request_changes", "comment"),
			"needs `verdict: request_changes`",
		},
		"request_changes on small findings": {
			strings.NewReplacer("severity=critical", "severity=nit", "**critical · authz**", "**nit · authz**").Replace(fleet),
			"needs a critical or major finding",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if ok, out := validate(c.body); ok || !strings.Contains(out, c.want) {
				t.Errorf("want %q, got passed=%v:\n%s", c.want, ok, out)
			}
		})
	}

	// A fence is prose: this `#` is a comment in a fix, not a second H1.
	fenced := strings.Replace(fleet, "**Fix:** resolve", "```bash\n# a comment\n```\n\n**Fix:** resolve", 1)
	if ok, out := validate(fenced); !ok {
		t.Errorf("a fence is not a heading:\n%s", out)
	}
}

func readGolden(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "reports", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// validator materializes the built-in skills and returns a function that runs
// the embedded validate-report.sh on a report — the same copy an agent runs.
func validator(t *testing.T) func(body string) (bool, string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	if err := skills.Materialize(dir); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "skills", "bazel-review-fleet", "references", "validate-report.sh")
	return func(body string) (bool, string) {
		f := filepath.Join(t.TempDir(), "report.md")
		if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("bash", script, f).CombinedOutput()
		return err == nil, string(out)
	}
}

func countFindings(parts []store.Part) int {
	n := 0
	for _, p := range parts {
		if p.Finding {
			n++
		}
	}
	return n
}

func TestGoldenReports(t *testing.T) {
	validate := validator(t)
	for _, g := range golden {
		t.Run(g.file, func(t *testing.T) {
			body := readGolden(t, g.file)
			ok, out := validate(body)
			if g.wantErr == "" && !ok {
				t.Errorf("validate-report.sh should pass:\n%s", out)
			}
			if g.wantErr != "" && (ok || !strings.Contains(out, g.wantErr)) {
				t.Errorf("validate-report.sh should fail with %q, got passed=%v:\n%s", g.wantErr, ok, out)
			}
			parts := store.SplitFindings(body)
			if n := countFindings(parts); n != g.findings {
				t.Errorf("SplitFindings marked %d findings, want %d", n, g.findings)
			}
			var joined strings.Builder
			for _, p := range parts {
				joined.WriteString(p.Text)
			}
			if joined.String() != body {
				t.Error("the parts must add back up to the report")
			}
		})
	}
}

// What bazel-post-report receives is Bazel's saved file: its own header, a
// `---` line, then the report. The header must not add or hide a finding.
func TestGoldenUnderBazelHeader(t *testing.T) {
	header := "# beroni/bazel#26 — Translate comments to English\n" +
		"- Author: @beroni\n- Branch: `chore/english-comments`\n" +
		"- Head sha: f231147aa0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5\n- Agent: bazel-review-fleet\n\n---\n\n"
	for _, g := range golden {
		saved := header + readGolden(t, g.file)
		if n := store.Findings(saved); n != g.findings {
			t.Errorf("%s under the Bazel header: %d findings, want %d", g.file, n, g.findings)
		}
	}
}

// The History tab reads what bazel-history-pr prints: a section the parser does
// not find is a blank part of the page, with no error anywhere.
func TestGoldenHistoryParses(t *testing.T) {
	h := store.ParseHistory(readGolden(t, "history.md"))
	if h.Meta["repo"] != "beroni/bazel" || h.Meta["threads"] != "1 (0 resolved, 1 open)" {
		t.Errorf("header = %v", h.Meta)
	}
	if h.CommitsRead != 4 {
		t.Errorf("commits read = %d", h.CommitsRead)
	}
	if !strings.Contains(h.WhatItDoes, "Actual footprint") {
		t.Errorf("what it does = %q", h.WhatItDoes)
	}
	if len(h.Decisions) != 2 || h.Decisions[1].N != 2 || len(h.Decisions[1].Rejected) != 1 {
		t.Fatalf("decisions = %+v", h.Decisions)
	}
	if len(h.StillOpen) != 1 || !strings.Contains(h.Rest, "## Timeline") {
		t.Errorf("still open = %v, rest = %.60q", h.StillOpen, h.Rest)
	}
}
