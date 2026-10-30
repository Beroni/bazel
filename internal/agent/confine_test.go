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
	guard := []string{"--setting-sources", "user", "--strict-mcp-config", "--plugin-dir", "/rt/x"}

	base := []string{"-p", "--output-format", "stream-json", "--verbose", "--allowedTools", "Read,Grep,Glob,Bash,Agent"}
	got := confineClaude(base, "/rt/x")
	if want := slices.Concat(guard, base); !slices.Equal(got, want) {
		t.Errorf("the flags go first:\n got %q\nwant %q", got, want)
	}
	if len(base) != 6 || base[0] != "-p" {
		t.Error("the step's own args must not be modified in place")
	}

	// The user's args already confine it: nothing is repeated.
	own := []string{"-p", "--model", "haiku", "--setting-sources=user", "--strict-mcp-config"}
	got = confineClaude(own, "/rt/y")
	if joined := strings.Join(got, " "); strings.Count(joined, "setting-sources") != 1 || strings.Count(joined, "--strict-mcp-config") != 1 {
		t.Errorf("flags already in the args were repeated: %q", got)
	}
	if !slices.Contains(got, "--plugin-dir") {
		t.Errorf("the plugin dir is always added: %q", got)
	}

	// Without a plugin dir (no embedded skill to load), the guard flags still go.
	got = confineClaude([]string{"-p"}, "")
	if !slices.Equal(got, []string{"--setting-sources", "user", "--strict-mcp-config", "-p"}) {
		t.Errorf("no plugin: %q", got)
	}
}

// Any source that reads the project is refused, in both spellings, wherever it
// is in the args.
func TestCheckSettingSources(t *testing.T) {
	for _, bad := range [][]string{
		{"-p", "--setting-sources", "user,project"},
		{"-p", "--setting-sources=project"},
		{"-p", "--setting-sources", "local"},
		{"-p", "--setting-sources"},
		{"-p", "--", "--setting-sources", "project"},
	} {
		if err := checkSettingSources(bad); err == nil || !strings.Contains(err.Error(), "--setting-sources user only") {
			t.Errorf("%q should be refused with a clear message, got %v", bad, err)
		}
	}
	for _, ok := range [][]string{{"-p"}, {"--setting-sources", "user"}, {"--setting-sources=user", "-p"}} {
		if err := checkSettingSources(ok); err != nil {
			t.Errorf("%q is fine: %v", ok, err)
		}
	}
}

// After a `--` everything is positional: the flags must come before it, and a
// flag found after it is text, not a flag already set.
func TestConfineClaudeDoubleDash(t *testing.T) {
	got := confineClaude([]string{"-p", "--", "some text"}, "/rt/x")
	if sep := slices.Index(got, "--"); sep < 0 || slices.Index(got, "--setting-sources") > sep || slices.Index(got, "--strict-mcp-config") > sep || slices.Index(got, "--plugin-dir") > sep {
		t.Errorf("every flag must sit before the --: %q", got)
	}

	got = confineClaude([]string{"-p", "--", "--strict-mcp-config", "--setting-sources=user"}, "")
	sep := slices.Index(got, "--")
	if !slices.Contains(got[:sep], "--strict-mcp-config") || !slices.Contains(got[:sep], "--setting-sources") {
		t.Errorf("a flag after the -- is text and does not count: %q", got)
	}
}

// A launcher takes its own options before the package: the flags have to reach
// Claude Code, so they go right after it.
func TestConfineClaudeLauncher(t *testing.T) {
	for _, args := range [][]string{
		{"-y", "@anthropic-ai/claude-code", "-p"},
		{"-y", "@anthropic-ai/claude-code@2.1.312", "-p"},
		{"--", "@anthropic-ai/claude-code", "-p"},
		{"exec", "--", "claude", "-p"},
	} {
		got := confineClaude(args, "/rt/x")
		pkg := claudePackageArg(got)
		if got[pkg+1] != "--setting-sources" || got[len(got)-1] != "-p" {
			t.Errorf("the flags belong right after the package: %q", got)
		}
	}
}

func TestRunsClaude(t *testing.T) {
	bin := t.TempDir()
	real := filepath.Join(bin, "claude")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(bin, "ai")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "unrelated"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cases := []struct {
		step config.ResolvedAgent
		want bool
	}{
		{config.ResolvedAgent{Command: "claude", Args: []string{"-p"}}, true},
		{config.ResolvedAgent{Command: "/opt/bin/claude", Format: "plain"}, true},
		{config.ResolvedAgent{Command: "ai", Format: "plain"}, true}, // a link to claude
		{config.ResolvedAgent{Command: "my-claude", Format: "claude-stream"}, true},
		{config.ResolvedAgent{Command: "wrapper", Args: []string{"--output-format", "stream-json"}}, true},
		{config.ResolvedAgent{Command: "wrapper", Args: []string{"--output-format=stream-json"}, Format: "plain"}, true},
		{config.ResolvedAgent{Command: "npx", Args: []string{"-y", "@anthropic-ai/claude-code", "-p"}, Format: "plain"}, true},
		{config.ResolvedAgent{Command: "bunx", Args: []string{"@anthropic-ai/claude-code@latest"}, Format: "plain"}, true},
		{config.ResolvedAgent{Command: "env", Args: []string{"FOO=1", "claude", "-p"}, Format: "plain"}, true},
		{config.ResolvedAgent{Command: "mise", Args: []string{"exec", "--", "claude", "-p"}, Format: "plain"}, true},
		// A shell takes no flags for what it runs: Neutralize protects this one.
		{config.ResolvedAgent{Command: "sh", Args: []string{"-c", "claude -p"}, Format: "claude-stream"}, false},
		{config.ResolvedAgent{Command: "claude.exe", Format: "plain"}, true},
		{config.ResolvedAgent{Command: `C:\x\claude.cmd`, Format: "plain"}, true},
		{config.ResolvedAgent{Command: "Claude", Format: "plain"}, true},
		{config.ResolvedAgent{Command: "claude-wrapper", Format: "plain"}, true},
		{config.ResolvedAgent{Command: "node", Args: []string{"/x/cli.js", "--output-format", "stream-json"}, Format: "plain"}, true},
		{config.ResolvedAgent{Command: "codex", Format: "codex-json"}, false},
		{config.ResolvedAgent{Command: "grok", Format: "grok-stream"}, false},
		{config.ResolvedAgent{Command: "echo", Args: []string{"report"}}, false},
		{config.ResolvedAgent{Command: "unrelated", Format: "plain"}, false},
		// Nothing shows these are Claude Code: Neutralize protects them.
		{config.ResolvedAgent{Command: "cc", Args: []string{"-p"}, Format: "plain"}, false},
		{config.ResolvedAgent{Command: "node", Args: []string{"/x/cli.js", "-p"}, Format: "plain"}, false},
		{config.ResolvedAgent{Command: "pi", Args: []string{"--model", "claude-haiku"}, Format: "plain"}, false},
		// A shell takes no flags, whatever it says it speaks.
		{config.ResolvedAgent{Command: "bash", Args: []string{"-c", "claude -p --output-format stream-json"}}, false},
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
	for _, want := range []string{"ARG --model", "ARG --setting-sources\nARG user\nARG --strict-mcp-config\nARG --plugin-dir", "ARG --strict-mcp-config", "HAS_MANIFEST", "HAS_POST_SKILL"} {
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

// One bad step refuses the whole job before the clone: the good step does not
// run, and nothing comes out that could be published.
func TestPipelineRefusedBeforeAnything(t *testing.T) {
	state := hostileClone(t, "wrapper")
	cfg := config.Default()
	cfg.Agent.Checkout = true
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{
		{Name: "good", Task: "x", Command: "wrapper", Format: "plain"},
		{Name: "bad", Task: "x", Command: "wrapper", Format: "plain", Args: []string{"--setting-sources", "user,project"}},
	}
	cfg.Pipelines = []config.Pipeline{{Name: "both", Steps: []string{"good", "bad"}}}
	choice, err := cfg.ChoiceByName("both")
	if err != nil {
		t.Fatal(err)
	}
	cloned := false
	res, err := New(cfg).Review(context.Background(), testPR(), choice, func(e Event) {
		cloned = cloned || e.Kind == EventClone
	})
	if err == nil || !strings.Contains(err.Error(), "--setting-sources user only") || !strings.Contains(err.Error(), "`bad`") {
		t.Fatalf("the job should be refused naming the step, got %v (%+v)", err, res)
	}
	if cloned {
		t.Error("the refusal should come before the clone")
	}
	if _, err := os.Stat(filepath.Join(state, "ran")); err == nil {
		t.Error("the good step must not run")
	}
}

// Cancelling a review with the agent running kills it and takes the plugin
// folder along.
func TestReviewCleansPluginOnCancel(t *testing.T) {
	bin, state := t.TempDir(), t.TempDir()
	marker := filepath.Join(state, "running")
	script := "#!/bin/sh\ntouch " + marker + "\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg, choice := claudeChoice(t, []string{"-p"})
	home, _ := config.Dir()
	runtime := filepath.Join(home, "runtime")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	foldersWhileRunning := make(chan int, 1)
	go func() {
		for i := 0; i < 500; i++ {
			if _, err := os.Stat(marker); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		entries, _ := os.ReadDir(runtime)
		foldersWhileRunning <- len(entries)
		cancel()
	}()
	start := time.Now()
	_, err := New(cfg).Review(ctx, testPR(), choice, nil)
	if err == nil {
		t.Error("a cancelled review should return an error")
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatal("the agent never started: this test would prove nothing")
	}
	if n := <-foldersWhileRunning; n != 1 {
		t.Errorf("the plugin folder should exist while the agent runs, found %d", n)
	}
	if time.Since(start) > 20*time.Second {
		t.Error("the agent was not killed")
	}
	entries, _ := os.ReadDir(runtime)
	if len(entries) != 0 {
		t.Errorf("a cancelled review left %d runtime folders behind", len(entries))
	}
}

// --plugin-dir is read with the clone as working directory, so it is absolute
// even when BAZEL_HOME is not.
func TestRuntimeIsAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("BAZEL_HOME", "relative-home")
	dir, err := newRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if !filepath.IsAbs(dir) {
		t.Errorf("runtime %q should be absolute", dir)
	}
}

// Publishing, with a clone: the post agent is confined like a review, gets
// the review's path, and never sees the PR's configuration.
func TestPublishIsConfined(t *testing.T) {
	state := hostileClone(t, "claude")
	cfg := config.Default()
	cfg.Agent.Checkout = true
	cfg.Agent.TimeoutSeconds = 30
	cfg.PostAgent.Format = "plain"
	cfg.PostAgent.Args = []string{"-p"}
	cfg.PostAgent.Command = "claude"
	choice := cfg.PostChoice()
	pr := testPR()
	pr.BaseRefName = ""
	res, err := New(cfg).Publish(context.Background(), pr, choice, "/reviews/publish/copy.md", "body", nil)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	for _, want := range []string{"--setting-sources user --strict-mcp-config --plugin-dir"} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("the post agent should get %q:\n%s", want, res.Body)
		}
	}
	prompt, _ := os.ReadFile(filepath.Join(state, "prompt"))
	if !strings.Contains(string(prompt), "/bazel-post-report /reviews/publish/copy.md") {
		t.Errorf("the post agent should get the trimmed copy:\n%s", prompt)
	}
	if _, err := os.Stat(filepath.Join(state, "hook-ran")); err == nil {
		t.Error("the post agent found the PR's configuration in the clone")
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
