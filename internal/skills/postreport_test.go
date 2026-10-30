package skills

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const postSkill = builtinRoot + "/bazel-post-report/SKILL.md"

func postReportText(t *testing.T) string {
	t.Helper()
	data, err := builtinFS.ReadFile(postSkill)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A report quotes someone else's PR, and a file can be named to run: the recipes
// that put a path in a command, or the model's text in a shell line, stay out.
func TestPostReportKeepsTheReportOutOfTheShell(t *testing.T) {
	text := postReportText(t)
	for _, old := range []string{
		`grep -n "^+++`, "--patch", "/tmp/review.json", "/tmp/report.md", "/tmp/all-clear.md",
		"<<'JSON'", "cat >", "na prosa (",
	} {
		if strings.Contains(text, old) {
			t.Errorf("the old recipe is back: %q", old)
		}
	}
	// No command may hold a literal /tmp file, or a double-quoted path.
	for _, line := range bashLines(text) {
		if strings.Contains(line, "/tmp/") && !strings.Contains(line, "${TMPDIR:-/tmp}") {
			t.Errorf("a fixed file in /tmp: %s", line)
		}
		if regexp.MustCompile(`"[^"]*\.[a-z]+"`).MatchString(line) {
			t.Errorf("a file name in double quotes: %s", line)
		}
	}
	for _, want := range []string{"nunca entram numa linha de shell", "grep -Fx -f", "mktemp -d", "ferramenta Write"} {
		if !strings.Contains(text, want) {
			t.Errorf("the skill should say %q", want)
		}
	}
	if !regexp.MustCompile(`(?m)^allowed-tools:.*\bWrite\b`).MatchString(text) {
		t.Error("without Write, the only way to a file is the shell")
	}
}

// bashLines returns the lines of the ```bash blocks.
func bashLines(text string) []string {
	var out []string
	in := false
	for _, l := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(l, "```bash"):
			in = true
		case strings.HasPrefix(l, "```"):
			in = false
		case in:
			out = append(out, l)
		}
	}
	return out
}

// The path check is run, as the skill writes it, on a report with a hostile path:
// the name comes out as text, and nothing in it executes.
func TestPostReportPathCheckRunsNothing(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	var recipe []string
	for _, l := range bashLines(postReportText(t)) {
		if strings.Contains(l, "/paths'") {
			recipe = append(recipe, l)
		}
	}
	if len(recipe) != 2 {
		t.Fatalf("the path check should be two lines, got %q", recipe)
	}

	work, bin := t.TempDir(), t.TempDir()
	marker := filepath.Join(work, "ran")
	hostile := "$(touch${IFS}" + marker + ").ts"
	report := filepath.Join(work, "review.md")
	body := "### a\n<!-- finding severity=major class=logic path=" + hostile + " line=2 side=RIGHT -->\n\n" +
		"### b\n<!-- finding severity=minor class=logic path=gone.go line=1 side=RIGHT -->\n\n" +
		"### c\n<!-- finding severity=minor class=logic path=- line=- side=RIGHT -->\n"
	if err := os.WriteFile(report, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gh := "#!/bin/sh\nprintf '%s\\n' 'other.go' '" + hostile + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	script := strings.NewReplacer("<review>", report, "<work>", work, "<N>", "7").Replace(strings.Join(recipe, "\n"))
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != hostile {
		t.Errorf("only the path in the diff should come out, as text: %q", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the file name was executed")
	}
}
