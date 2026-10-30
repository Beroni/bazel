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
	script := filepath.Join(dir, skills.SkillsSubdir, "bazel-review-fleet", "references", "validate-report.sh")
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

// Unticking every finding of a section on the page leaves the section empty,
// which the validator rejects. That is why bazel-post-report reads the
// metadata directly instead of gating on the validator: the file is exactly
// what the person approved.
func TestUntickedSectionFailsValidator(t *testing.T) {
	validate := validator(t)
	body := readGolden(t, "fleet.md")
	trimmed := store.DropFindings(body, []int{0, 1}) // both items of ## Findings
	if store.Findings(trimmed) != 2 {
		t.Fatalf("expected the cut and the needs-human item to remain")
	}
	if ok, _ := validate(trimmed); ok {
		t.Error("an emptied section should fail the validator; if it passes now, bazel-post-report can gate on it again")
	}
	skill, err := os.ReadFile(filepath.Join("builtin", "bazel-post-report", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(skill), "Do not\nrun `validate-report.sh` on this file") {
		t.Error("bazel-post-report must keep telling the agent not to gate on the validator")
	}
}

// TestCheckReport is the Go half of scripts/bazel-check.sh: pointed at a real
// agent's output through BAZEL_CHECK_REPORT, it prints how the page would split
// it. Skipped in a normal run.
func TestCheckReport(t *testing.T) {
	path := os.Getenv("BAZEL_CHECK_REPORT")
	if path == "" {
		t.Skip("BAZEL_CHECK_REPORT not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range store.SplitFindings(string(data)) {
		if p.Finding {
			n++
			first, _, _ := strings.Cut(strings.TrimSpace(p.Text), "\n")
			t.Logf("finding %d: %s", n, first)
		}
	}
	t.Logf("SplitFindings: %d findings", n)
}
