package skills

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// catalog is what Bazel ships. A skill added to builtin/ without a line here
// fails the test on purpose: the starter config and the docs name these.
var catalog = []string{
	"bazel-exploit-digger",
	"bazel-history-pr",
	"bazel-lazy-senior-dev",
	"bazel-post-report",
	"bazel-review-fleet",
	"bazel-senior-code-reviewer",
}

func TestCatalogIsWhatShips(t *testing.T) {
	var got []string
	for _, s := range Builtin() {
		got = append(got, s.Name)
	}
	if strings.Join(got, ",") != strings.Join(catalog, ",") {
		t.Errorf("built-in skills = %v, want %v", got, catalog)
	}
}

// frontmatter returns the raw YAML block of a SKILL.md or agent file.
func frontmatter(t *testing.T, content string) string {
	t.Helper()
	rest, ok := strings.CutPrefix(content, "---\n")
	if !ok {
		t.Fatal("no frontmatter")
	}
	block, _, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		t.Fatal("frontmatter not closed")
	}
	return block
}

func TestCatalogFrontmatter(t *testing.T) {
	for _, s := range Builtin() {
		data, err := builtinFS.ReadFile(s.Dir + "/SKILL.md")
		if err != nil {
			t.Fatal(err)
		}
		fm := frontmatter(t, string(data))
		if name := filepath.Base(s.Dir); s.Name != name {
			t.Errorf("%s: frontmatter name %q differs from its folder", name, s.Name)
		}
		if !strings.HasPrefix(s.Name, "bazel-") {
			t.Errorf("%s: a built-in skill needs the bazel- prefix — a personal skill of the same name would shadow it", s.Name)
		}
		if len(s.Description) < 80 {
			t.Errorf("%s: the description is what the page and the model read; too short: %q", s.Name, s.Description)
		}
		tools := regexp.MustCompile(`(?m)^allowed-tools: (.+)$`).FindStringSubmatch(fm)
		if tools == nil {
			t.Errorf("%s: no allowed-tools — without it `claude -p` denies Write silently", s.Name)
			continue
		}
		// Nobody answers a headless run: a skill that can ask hangs.
		if strings.Contains(tools[1], "AskUserQuestion") {
			t.Errorf("%s: a built-in skill runs headless and must not ask", s.Name)
		}
	}
}

// Every review skill defers to the headless contract.
func TestCatalogFollowsHeadless(t *testing.T) {
	for _, s := range Builtin() {
		data, _ := builtinFS.ReadFile(s.Dir + "/SKILL.md")
		if !strings.Contains(string(data), ".claude/skills/bazel-review-fleet/references/headless.md") {
			t.Errorf("%s does not point at headless.md", s.Name)
		}
	}
}

// The skills name files by their path in the clone. Every one of those paths
// has to exist in what Materialize writes, or the agent reads nothing and
// improvises.
func TestCatalogPathsExist(t *testing.T) {
	ref := regexp.MustCompile(`\.claude/(skills|agents)/[A-Za-z0-9_./<>-]*[A-Za-z0-9_>]`)
	dir := t.TempDir()
	if err := Materialize(dir); err != nil {
		t.Fatal(err)
	}
	err := fs.WalkDir(builtinFS, builtinRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, _ := builtinFS.ReadFile(p)
		for _, m := range ref.FindAllString(string(data), -1) {
			if strings.Contains(m, "<") || m == ".claude/skills" || m == ".claude/agents" {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, m)); err != nil {
				t.Errorf("%s mentions %s, which Materialize does not write", p, m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Each lens agent is named after its file, which is the name the fleet passes
// as subagent_type.
func TestCatalogAgents(t *testing.T) {
	n := 0
	err := fs.WalkDir(builtinFS, builtinRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(builtinRoot, p)
		file, ok := agentFile(rel)
		if !ok {
			return nil
		}
		n++
		data, _ := builtinFS.ReadFile(p)
		want := "name: " + strings.TrimSuffix(file, ".md")
		if !strings.Contains(frontmatter(t, string(data)), want) {
			t.Errorf("%s: frontmatter should carry %q", p, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("expected the three lens agents, found %d", n)
	}
}

func TestMaterializeWritesAgents(t *testing.T) {
	dir := t.TempDir()
	if err := Materialize(dir); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"bazel-senior-code-reviewer", "bazel-exploit-digger", "bazel-lazy-senior-dev"} {
		if _, err := os.Stat(filepath.Join(dir, AgentsSubdir, a+".md")); err != nil {
			t.Errorf("the lens %s should be in %s: %v", a, AgentsSubdir, err)
		}
	}
	info, err := os.Stat(filepath.Join(dir, SkillsSubdir, "bazel-review-fleet", "references", "validate-report.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("validate-report.sh should be executable, got %v", info.Mode())
	}
}

func TestInline(t *testing.T) {
	got := Inline("/bazel-review-fleet 26")
	if strings.HasPrefix(got, "/") {
		t.Fatal("a built-in task should be expanded, not left as a slash command")
	}
	for _, want := range []string{"`bazel-review-fleet`", "Arguments: 26", "# Bazel Review Fleet", ".claude/agents/"} {
		if !strings.Contains(got, want) {
			t.Errorf("inlined task lacks %q", want)
		}
	}
	if strings.Contains(got, "allowed-tools:") {
		t.Error("the frontmatter is for Claude Code, not for the prompt")
	}
	if got := Inline("/bazel-history-pr"); !strings.Contains(got, "Arguments: none") {
		t.Errorf("no arguments should say so: %q", got[:120])
	}
	for _, task := range []string{"/review-fleet 26", "Review this PR.", ""} {
		if got := Inline(task); got != task {
			t.Errorf("Inline(%q) should leave a non-built-in task alone, got %q", task, got)
		}
	}
}
