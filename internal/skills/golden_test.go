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
	{"fleet-discussed.md", "", 4},     // the same, and two points the PR already discussed
	{"incremental.md", "", 4},         // the same, over the changes since an earlier head
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
	discussed := readGolden(t, "fleet-discussed.md")
	stops := "the report stops before its title and summary are complete"
	cases := map[string]struct{ body, want string }{
		"the header alone":           {fleet[:strings.Index(fleet, "# Review Fleet")], stops},
		"a major point left out":     {strings.Replace(discussed, "· minor · naming", "· major · naming", 1), "a critical or major point is never left out"},
		"a critical point left out":  {strings.Replace(discussed, "`PR` · nit", "`PR` · critical", 1), "a critical or major point is never left out"},
		"a thread on another PR":     {strings.Replace(discussed, "klever/app/pull/123#discussion", "klever/app/pull/9#discussion", 1), "is not on this PR"},
		"a thread on another repo":   {strings.Replace(discussed, "github.com/klever/app/pull/123#issue", "github.com/evil/app/pull/123#issue", 1), "is not on this PR"},
		"a link that is no thread":   {strings.Replace(discussed, "#discussion_r42)", "#x)", 1), "an already-discussed item is"},
		"no marker":                  {strings.Replace(discussed, "<!-- already-discussed -->\n", "", 1), "needs the `<!-- already-discussed -->` marker"},
		"text in the section":        {discussed + "The rest was fine.\n", "holds only its items"},
		"before the human questions": {strings.Replace(discussed, "## Needs human verification", "## Already discussed\n<!-- already-discussed -->\n\n- `PR` · nit · docs — x — [thread](https://github.com/klever/app/pull/123#issuecomment-1)\n\n## Needs human verification", 1), "out of order"},
		"cut after the title":        {fleet[:strings.Index(fleet, "**Verdict:**")], stops},
		"cut after the verdict":      {fleet[:strings.Index(fleet, "**Scope:**")], stops},
		"cut after the scope":        {fleet[:strings.Index(fleet, "**Lenses:**")], stops},
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

// The check headless.md tells a skill to run on --since, run as written, in a real
// repository: only an ancestor of HEAD that is in the clone makes the review
// incremental, and everything else is the whole PR.
func TestSinceCheck(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	data, err := os.ReadFile(filepath.Join("builtin", "bazel-review-fleet", "references", "headless.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	i := strings.Index(text, "S=<the 40 characters>")
	if i < 0 {
		t.Fatal("headless.md lost the --since check")
	}
	recipe := text[i : i+strings.Index(text[i:], "```")]

	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "one")
	old := git("rev-parse", "HEAD")
	git("checkout", "-q", "-b", "side")
	git("commit", "-q", "--allow-empty", "-m", "elsewhere")
	side := git("rev-parse", "HEAD")
	git("checkout", "-q", "main")
	git("commit", "-q", "--allow-empty", "-m", "two")

	for name, c := range map[string]struct {
		sha  string
		want bool
	}{
		"an ancestor":       {old, true},
		"not an ancestor":   {side, false},
		"not in the clone":  {strings.Repeat("a", 40), false},
		"short":             {old[:12], false},
		"upper case":        {strings.ToUpper(old), false},
		"empty":             {"", false},
		"a ref, not a sha":  {"HEAD~1", false},
		"text built to run": {"$(touch ran)", false},
		"a quote":           {"x'; touch ran; '", false},
	} {
		t.Run(name, func(t *testing.T) {
			// The value comes in as data, through the environment: the check itself
			// has to be safe on anything, and only a value that passes is ever typed.
			script := strings.Replace(recipe, "<the 40 characters>", `"$CANDIDATE"`, 1)
			cmd := exec.Command("bash", "-c", script)
			cmd.Dir = repo
			cmd.Env = append(os.Environ(), "CANDIDATE="+c.sha)
			out, _ := cmd.CombinedOutput()
			if got := strings.Contains(string(out), "incremental"); got != c.want {
				t.Errorf("incremental = %v, want %v: %s", got, c.want, out)
			}
			if _, err := os.Stat(filepath.Join(repo, "ran")); err == nil {
				t.Error("the sha was executed")
			}
		})
	}
}

// The value from the task is the one thing a person, or a PR, could make anything:
// the skill is told to look at it before any command, and the recipe never quotes it.
func TestSinceNeverGoesIntoTheShellUnchecked(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("builtin", "bazel-review-fleet", "references", "headless.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"Look at the value before it goes into any command", "never reaches the shell, not even quoted"} {
		if !strings.Contains(text, want) {
			t.Errorf("headless.md lost %q", want)
		}
	}
	if strings.Contains(text, "S='<") {
		t.Error("the recipe puts the raw value in a shell literal again")
	}
}
