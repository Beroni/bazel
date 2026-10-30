package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/beroni/bazel/internal/config"
)

func TestConfineClaude(t *testing.T) {
	base := []string{"-p", "--output-format", "stream-json", "--verbose", "--allowedTools", "Read,Grep,Glob,Bash,Agent"}
	got, err := confineClaude(base, "/rt/x")
	if err != nil {
		t.Fatal(err)
	}
	want := append(slices.Clone(base), "--setting-sources", "user", "--strict-mcp-config", "--plugin-dir", "/rt/x")
	if !slices.Equal(got, want) {
		t.Errorf("default args:\n got %q\nwant %q", got, want)
	}
	if len(base) != 6 {
		t.Error("the step's own args must not be modified in place")
	}

	// The user's args already confine it: nothing is repeated.
	own := []string{"-p", "--model", "haiku", "--setting-sources=user", "--strict-mcp-config"}
	got, err = confineClaude(own, "/rt/y")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(strings.Join(got, " "), "setting-sources") != 1 || strings.Count(strings.Join(got, " "), "--strict-mcp-config") != 1 {
		t.Errorf("flags already in the args were repeated: %q", got)
	}
	if !slices.Contains(got, "--plugin-dir") {
		t.Errorf("the plugin dir is always added: %q", got)
	}

	// Without a plugin dir (no embedded skill to load), the guard flags still go.
	got, _ = confineClaude([]string{"-p"}, "")
	if !slices.Equal(got, []string{"-p", "--setting-sources", "user", "--strict-mcp-config"}) {
		t.Errorf("no plugin: %q", got)
	}

	// Any source that reads the project is refused, in both spellings.
	for _, bad := range [][]string{
		{"-p", "--setting-sources", "user,project"},
		{"-p", "--setting-sources=project"},
		{"-p", "--setting-sources", "local"},
		{"-p", "--setting-sources"},
	} {
		if _, err := confineClaude(bad, "/rt"); err == nil || !strings.Contains(err.Error(), "--setting-sources user only") {
			t.Errorf("%q should be refused with a clear message, got %v", bad, err)
		}
	}
}

func TestRunsClaude(t *testing.T) {
	cases := []struct {
		step config.ResolvedAgent
		want bool
	}{
		{config.ResolvedAgent{Command: "claude", Args: []string{"-p"}}, true},
		{config.ResolvedAgent{Command: "/opt/bin/claude", Format: "plain"}, true},
		{config.ResolvedAgent{Command: "my-claude", Format: "claude-stream"}, true},
		{config.ResolvedAgent{Command: "wrapper", Args: []string{"--output-format", "stream-json"}}, true},
		{config.ResolvedAgent{Command: "codex", Format: "codex-json"}, false},
		{config.ResolvedAgent{Command: "grok", Format: "grok-stream"}, false},
		{config.ResolvedAgent{Command: "echo", Args: []string{"report"}}, false},
	}
	for _, c := range cases {
		if got := runsClaude(c.step); got != c.want {
			t.Errorf("runsClaude(%+v) = %v, want %v", c.step, got, c.want)
		}
	}
}

// fakeClaude puts a `claude` on PATH that prints its args and the plugin it
// was handed, then exits with code.
func fakeClaude(t *testing.T, code int) {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
for a in "$@"; do printf 'ARG %s\n' "$a"; done
while [ $# -gt 0 ]; do
  if [ "$1" = "--plugin-dir" ]; then
    printf 'PLUGIN %s\n' "$2"
    [ -f "$2/.claude-plugin/plugin.json" ] && echo HAS_MANIFEST
    [ -f "$2/skills/bazel-post-report/SKILL.md" ] && echo HAS_POST_SKILL
  fi
  shift
done
exit ` + string(rune('0'+code)) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func claudeChoice(t *testing.T, args []string) (*config.Config, config.Choice) {
	t.Helper()
	cfg := config.Default()
	cfg.Agent.Checkout = false
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{{Name: "lens", Command: "claude", Args: args, Format: "plain", Task: "/review-fleet {{number}}"}}
	choice, err := cfg.ChoiceByName("lens")
	if err != nil {
		t.Fatal(err)
	}
	return cfg, choice
}

// The guard flags reach the process even when the user wrote their own args,
// the embedded skills are there as a plugin while it runs, and the plugin
// folder — Bazel's, under BAZEL_HOME — is gone when the review ends.
func TestReviewConfinesClaude(t *testing.T) {
	fakeClaude(t, 0)
	cfg, choice := claudeChoice(t, []string{"-p", "--model", "haiku"})
	res, err := New(cfg).Review(context.Background(), testPR(), choice, nil)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	for _, want := range []string{"ARG --model", "ARG --setting-sources\nARG user", "ARG --strict-mcp-config", "HAS_MANIFEST", "HAS_POST_SKILL"} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("the agent did not get %q:\n%s", want, res.Body)
		}
	}
	plugin := pluginFrom(t, res.Body)
	home, _ := config.Dir()
	if !strings.HasPrefix(plugin, filepath.Join(home, "runtime")+string(os.PathSeparator)) {
		t.Errorf("the plugin should live under BAZEL_HOME/runtime, got %s", plugin)
	}
	if _, err := os.Stat(plugin); !os.IsNotExist(err) {
		t.Errorf("the plugin folder should be gone after the review: %v", err)
	}
}

// A failing agent cleans up the same way.
func TestReviewCleansPluginOnError(t *testing.T) {
	fakeClaude(t, 3)
	cfg, choice := claudeChoice(t, []string{"-p"})
	res, _ := New(cfg).Review(context.Background(), testPR(), choice, nil)
	home, _ := config.Dir()
	entries, _ := os.ReadDir(filepath.Join(home, "runtime"))
	if len(entries) != 0 {
		t.Errorf("a failed review left %d runtime folders behind (%v)", len(entries), res.Steps)
	}
}

// A contradicting flag stops the review before anything runs.
func TestReviewRefusesProjectSettings(t *testing.T) {
	fakeClaude(t, 0)
	cfg, choice := claudeChoice(t, []string{"-p", "--setting-sources", "user,project"})
	_, err := New(cfg).Review(context.Background(), testPR(), choice, nil)
	if err == nil || !strings.Contains(err.Error(), "--setting-sources user only") {
		t.Errorf("a --setting-sources that reads the project must be refused clearly, got %v", err)
	}
}

// A cancelled review cleans up too: the plugin goes with the review, however
// it ends.
func TestReviewCleansPluginOnCancel(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nsleep 10\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg, choice := claudeChoice(t, []string{"-p"})
	ctx, cancel := context.WithCancel(context.Background())
	home, _ := config.Dir()
	go func() {
		// Cancel once the plugin folder exists, i.e. while the agent runs.
		for i := 0; i < 200; i++ {
			if entries, _ := os.ReadDir(filepath.Join(home, "runtime")); len(entries) > 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	New(cfg).Review(ctx, testPR(), choice, nil)
	entries, _ := os.ReadDir(filepath.Join(home, "runtime"))
	if len(entries) != 0 {
		t.Errorf("a cancelled review left %d runtime folders behind", len(entries))
	}
}

func pluginFrom(t *testing.T, body string) string {
	t.Helper()
	for _, l := range strings.Split(body, "\n") {
		if p, ok := strings.CutPrefix(l, "PLUGIN "); ok {
			return p
		}
	}
	t.Fatalf("no --plugin-dir reached the agent:\n%s", body)
	return ""
}
