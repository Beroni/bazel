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
