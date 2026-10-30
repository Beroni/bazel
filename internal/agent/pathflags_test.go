package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beroni/bazel/internal/config"
)

// Every flag that reads a file as configuration, in both spellings: a relative
// path and a path inside the clone are refused, a path outside is fine.
func TestCheckPathFlags(t *testing.T) {
	clone := t.TempDir()
	outside := filepath.Join(t.TempDir(), "mine.json")

	for _, flag := range pathFlags {
		for name, value := range map[string]string{
			"relative":       ".claude/settings.json",
			"dot":            "./.mcp.json",
			"parent":         "../x.json",
			"inside":         filepath.Join(clone, ".mcp.json"),
			"the clone":      clone,
			"inline JSON":    `{"hooks":{}}`,
			"tilde":          "~/mine.json",
			"climbs back in": filepath.Join(clone, "a", "..", "b.json"),
		} {
			for _, args := range [][]string{{"-p", flag, value}, {"-p", flag + "=" + value}} {
				err := checkPathFlags(args, clone)
				if err == nil || !strings.Contains(err.Error(), flag) || !strings.Contains(err.Error(), "clone") {
					t.Errorf("%s (%s) %q should be refused clearly, got %v", flag, name, args, err)
				}
			}
		}
		for _, args := range [][]string{{"-p", flag, outside}, {flag + "=" + outside, "-p"}} {
			if err := checkPathFlags(args, clone); err != nil {
				t.Errorf("%q is outside the clone: %v", args, err)
			}
		}
		if err := checkPathFlags([]string{"-p", flag}, clone); err == nil {
			t.Errorf("%s with no value should be refused", flag)
		}
	}

	if err := checkPathFlags([]string{"-p", "--model", "haiku", "--allowedTools", "Read"}, clone); err != nil {
		t.Errorf("no path flag, nothing to refuse: %v", err)
	}
	// A step without a clone still refuses a relative path.
	if err := checkPathFlags([]string{"--settings", "x.json"}, ""); err == nil {
		t.Error("a relative path is refused with no clone too")
	}
}

// Only the flags that take several values read past the first one: the next
// word after --settings is not its value.
func TestCheckPathFlagsSingleValue(t *testing.T) {
	clone := t.TempDir()
	ok := filepath.Join(t.TempDir(), "s.json")
	if err := checkPathFlags([]string{"--settings", ok, "review this"}, clone); err != nil {
		t.Errorf("the prompt after --settings is not its value: %v", err)
	}
	if err := checkPathFlags([]string{"--mcp-config", ok, "-", "rel.json"}, clone); err == nil {
		t.Error("a lone - does not end the values of --mcp-config")
	}
	for _, v := range []string{"/proc/self/cwd/.mcp.json", "/dev/fd/3"} {
		if err := checkPathFlags([]string{"--mcp-config", v}, clone); err == nil {
			t.Errorf("%s leads into the clone and should be refused", v)
		}
	}
}

// --mcp-config and --add-dir take several values: one bad value is enough.
func TestCheckPathFlagsSeveralValues(t *testing.T) {
	clone := t.TempDir()
	ok := filepath.Join(t.TempDir(), "a.json")
	if err := checkPathFlags([]string{"--mcp-config", ok, ".mcp.json", "-p"}, clone); err == nil {
		t.Error("the second value is relative and should be refused")
	}
	if err := checkPathFlags([]string{"--add-dir", ok, filepath.Join(clone, "src"), "-p"}, clone); err == nil {
		t.Error("the second value is inside the clone and should be refused")
	}
	if err := checkPathFlags([]string{"--add-dir", ok, ok, "-p"}, clone); err != nil {
		t.Errorf("both are outside: %v", err)
	}
}

// A path outside that links back into the clone is inside it.
func TestCheckPathFlagsSymlinkIntoClone(t *testing.T) {
	clone := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(clone, link); err != nil {
		t.Fatal(err)
	}
	if err := checkPathFlags([]string{"--settings", link}, clone); err == nil {
		t.Error("a link into the clone should be refused")
	}
}

// A step with a bad flag stops the review before the agent starts, whatever
// the CLI is.
func TestReviewRefusesPathFlagsBeforeRunning(t *testing.T) {
	for _, command := range []string{"wrapper", "claude", "codex"} {
		state := hostileClone(t, command)
		cfg := config.Default()
		cfg.Agent.Checkout = true
		cfg.Agent.Prompt = "{{task}}"
		cfg.Agent.TimeoutSeconds = 30
		cfg.Agents = []config.AgentDef{{Name: "lens", Task: "x", Command: command, Format: "plain",
			Args: []string{"-p", "--settings", ".claude/settings.json"}}}
		choice, err := cfg.ChoiceByName("lens")
		if err != nil {
			t.Fatal(err)
		}
		pr := testPR()
		pr.BaseRefName = ""
		_, err = New(cfg).Review(context.Background(), pr, choice, nil)
		if err == nil || !strings.Contains(err.Error(), "--settings") {
			t.Errorf("%s: the review should be refused, got %v", command, err)
		}
		if _, err := os.Stat(filepath.Join(state, "ran")); err == nil {
			t.Errorf("%s: the agent must not start", command)
		}
	}
}

// A link that leads nowhere could be made to lead into the clone, and a folder
// that links in holds the clone's files even if this one does not exist yet.
func TestCheckPathFlagsUnresolvedLinks(t *testing.T) {
	clone := t.TempDir()
	outside := t.TempDir()

	dangling := filepath.Join(outside, "dangling.json")
	if err := os.Symlink(filepath.Join(clone, "not-yet.json"), dangling); err != nil {
		t.Fatal(err)
	}
	if err := checkPathFlags([]string{"--settings", dangling}, clone); err == nil {
		t.Error("a link that leads nowhere should be refused")
	}

	folder := filepath.Join(outside, "folder")
	if err := os.Symlink(clone, folder); err != nil {
		t.Fatal(err)
	}
	if err := checkPathFlags([]string{"--settings", filepath.Join(folder, "not-yet.json")}, clone); err == nil {
		t.Error("a missing file under a folder that links into the clone should be refused")
	}
}

// A path that was safe when the job started does not stay safe: an earlier step
// can turn it into a link into the clone. The check runs again right before each
// agent, and the job stops there.
func TestStepSwappingAPathIntoTheClone(t *testing.T) {
	state := hostileClone(t, "wrapper")
	allowed := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(allowed, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	swapper := "#!/bin/sh\necho '{\"hooks\":{}}' > evil.json\nln -sf \"$PWD/evil.json\" \"$BAZEL_TEST_ALLOWED\"\necho swapped\n"
	bin := filepath.SplitList(os.Getenv("PATH"))[0]
	if err := os.WriteFile(filepath.Join(bin, "swapper"), []byte(swapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BAZEL_TEST_ALLOWED", allowed)

	cfg := config.Default()
	cfg.Agent.Checkout = true
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{
		{Name: "first", Task: "x", Command: "swapper", Format: "plain"},
		{Name: "second", Task: "x", Command: "wrapper", Format: "plain", Args: []string{"--settings", allowed}},
	}
	cfg.Pipelines = []config.Pipeline{{Name: "both", Steps: []string{"first", "second"}}}
	choice, err := cfg.ChoiceByName("both")
	if err != nil {
		t.Fatal(err)
	}
	pr := testPR()
	pr.BaseRefName = ""
	res, err := New(cfg).Review(context.Background(), pr, choice, nil)
	if err == nil || !strings.Contains(err.Error(), "inside the PR's clone") || !strings.Contains(err.Error(), "`second`") {
		t.Fatalf("the second step should be refused, got %v", err)
	}
	if res.Body != "" {
		t.Errorf("a refused job leaves nothing to publish, got %q", res.Body)
	}
	if _, err := os.Stat(filepath.Join(state, "ran")); err == nil {
		t.Error("the second agent must not start")
	}
}

// CLAUDE_CONFIG_DIR is where --setting-sources user reads: relative or in the
// clone, it would be the PR's settings. Inherited counts as much as set.
func TestCheckClaudeConfigDir(t *testing.T) {
	clone := t.TempDir()
	outside := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	for _, bad := range []string{".x", "./.x", "~/.claude", filepath.Join(clone, ".x")} {
		err := checkClaudeConfigDir(config.ResolvedAgent{Env: map[string]string{"CLAUDE_CONFIG_DIR": bad}}, clone)
		if err == nil || !strings.Contains(err.Error(), "CLAUDE_CONFIG_DIR") {
			t.Errorf("%q should be refused, got %v", bad, err)
		}
	}
	if err := checkClaudeConfigDir(config.ResolvedAgent{Env: map[string]string{"CLAUDE_CONFIG_DIR": outside}}, clone); err != nil {
		t.Errorf("an absolute path outside the clone is fine: %v", err)
	}
	if err := checkClaudeConfigDir(config.ResolvedAgent{}, clone); err != nil {
		t.Errorf("not set, nothing to refuse: %v", err)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", ".inherited")
	if err := checkClaudeConfigDir(config.ResolvedAgent{}, clone); err == nil {
		t.Error("a relative value inherited from Bazel's environment should be refused")
	}
	if err := checkClaudeConfigDir(config.ResolvedAgent{Env: map[string]string{"CLAUDE_CONFIG_DIR": outside}}, clone); err != nil {
		t.Errorf("the step's value wins over the inherited one: %v", err)
	}
}

// It stops the job before the clone, like the other bad steps.
func TestRelativeConfigDirRefusedBeforeTheClone(t *testing.T) {
	state := hostileClone(t, "wrapper")
	cfg := config.Default()
	cfg.Agent.Checkout = true
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{{Name: "lens", Task: "x", Command: "wrapper", Format: "plain",
		Env: map[string]string{"CLAUDE_CONFIG_DIR": ".x"}}}
	choice, err := cfg.ChoiceByName("lens")
	if err != nil {
		t.Fatal(err)
	}
	cloned := false
	_, err = New(cfg).Review(context.Background(), testPR(), choice, func(e Event) {
		cloned = cloned || e.Kind == EventClone
	})
	if err == nil || !strings.Contains(err.Error(), "CLAUDE_CONFIG_DIR") {
		t.Fatalf("the review should be refused, got %v", err)
	}
	if cloned {
		t.Error("the refusal should come before the clone")
	}
	if _, err := os.Stat(filepath.Join(state, "ran")); err == nil {
		t.Error("the agent must not start")
	}
}
