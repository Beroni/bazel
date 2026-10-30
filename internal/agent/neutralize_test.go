package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/beroni/bazel/internal/config"
)

// hostileAgent is a fake agent CLI that does what a real one does with a PR's
// configuration: finding it in its working directory counts as running it.
const hostileAgent = `#!/bin/sh
cat > "$BAZEL_TEST_DIR/prompt"
touch "$BAZEL_TEST_DIR/ran"
for f in .claude/settings.json .claude/settings.local.json .claude/hooks .mcp.json .codex/config.toml; do
  [ -e "$f" ] && touch "$BAZEL_TEST_DIR/hook-ran"
done
printf 'ARGS %s\n' "$*"
echo report
`

// hostileClone puts on PATH a gh whose `repo clone` makes a repository that
// versions everything an agent CLI would execute, and the named fake agents.
func hostileClone(t *testing.T, agents ...string) string {
	t.Helper()
	bin, state := t.TempDir(), t.TempDir()
	gh := `#!/bin/sh
if [ "$1 $2" = "repo clone" ]; then
  git init -q "$4" && cd "$4" || exit 1
  mkdir -p .claude/hooks .codex
  echo '{"hooks":{"SessionStart":[]}}' > .claude/settings.json
  echo '{}' > .claude/settings.local.json
  echo 'evil' > .claude/hooks/run.sh
  echo '{"mcpServers":{}}' > .mcp.json
  echo 'x' > .codex/config.toml
  echo 'code' > main.go
  git add -A
  git -c user.name=t -c user.email=t@t -c commit.gpgsign=false commit -qm init
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if err := os.WriteFile(filepath.Join(bin, a), []byte(hostileAgent), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BAZEL_TEST_DIR", state)
	return state
}

// The fake agent really would run the PR's configuration: this is what the
// other tests rely on when they find no trace of it.
func TestHostileAgentDetectsConfig(t *testing.T) {
	state := hostileClone(t, "wrapper")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("wrapper")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(state, "hook-ran")); err != nil {
		t.Fatal("the control failed: the fake agent should have found the settings")
	}
}

// Whatever runs in the clone, recognized as Claude Code or not, the PR's
// configuration is gone before it starts — and the agent is told where to read
// it, because it is still part of the review.
func TestNoAgentSeesThePRConfig(t *testing.T) {
	cases := []struct {
		name    string
		agent   config.AgentDef
		confine bool
	}{
		{"unknown wrapper, plain", config.AgentDef{Command: "wrapper", Args: []string{"-p"}, Format: "plain"}, false},
		{"claude, plain", config.AgentDef{Command: "claude", Args: []string{"-p"}, Format: "plain"}, true},
		{"codex", config.AgentDef{Command: "codex", Args: []string{"exec", "--json", "-"}, Format: "codex-json"}, false},
		{"grok", config.AgentDef{Command: "grok", Args: []string{"--prompt-file", "/dev/stdin"}, Format: "grok-stream"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state := hostileClone(t, c.agent.Command)
			cfg := config.Default()
			cfg.Agent.Checkout = true
			cfg.Agent.Prompt = "{{task}}"
			cfg.Agent.TimeoutSeconds = 30
			c.agent.Name, c.agent.Task = "lens", "Review it."
			cfg.Agents = []config.AgentDef{c.agent}
			choice, err := cfg.ChoiceByName("lens")
			if err != nil {
				t.Fatal(err)
			}
			pr := testPR()
			pr.BaseRefName = "" // the fake clone has no remote to fetch a base from

			var log []string
			res, _ := New(cfg).Review(context.Background(), pr, choice, func(e Event) {
				if e.Kind == EventLog {
					log = append(log, e.Text)
				}
			})
			if _, err := os.Stat(filepath.Join(state, "ran")); err != nil {
				t.Fatalf("the agent never ran: %v", err)
			}
			if _, err := os.Stat(filepath.Join(state, "hook-ran")); err == nil {
				t.Fatal("the agent found the PR's configuration in the clone")
			}
			if !strings.Contains(strings.Join(log, "\n"), "removed from the clone, so that no agent runs them: .claude, .codex, .mcp.json") {
				t.Errorf("the job's log should say what was neutralized:\n%s", strings.Join(log, "\n"))
			}
			prompt, _ := os.ReadFile(filepath.Join(state, "prompt"))
			if !strings.Contains(string(prompt), "git show HEAD:<path>") {
				t.Errorf("the agent should be told the files are still in git:\n%s", prompt)
			}
			if c.confine && !strings.Contains(res.Body, "--strict-mcp-config") {
				t.Errorf("this one is Claude Code and also gets the flags:\n%s", res.Body)
			}
		})
	}
}

// A pipeline step can bring the files back with a git checkout: the next step
// still runs without them.
func TestNeutralizeRunsBeforeEveryStep(t *testing.T) {
	state := hostileClone(t, "wrapper", "restorer")
	script := "#!/bin/sh\ngit checkout -q -- . ; echo restored\n"
	if err := os.WriteFile(filepath.Join(filepath.SplitList(os.Getenv("PATH"))[0], "restorer"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Agent.Checkout = true
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{
		{Name: "first", Task: "x", Command: "restorer", Format: "plain"},
		{Name: "second", Task: "x", Command: "wrapper", Format: "plain"},
	}
	cfg.Pipelines = []config.Pipeline{{Name: "both", Steps: []string{"first", "second"}}}
	choice, err := cfg.ChoiceByName("both")
	if err != nil {
		t.Fatal(err)
	}
	pr := testPR()
	pr.BaseRefName = ""
	if _, err := New(cfg).Review(context.Background(), pr, choice, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "ran")); err != nil {
		t.Fatal("the second step never ran")
	}
	if _, err := os.Stat(filepath.Join(state, "hook-ran")); err == nil {
		t.Error("the second step found the files the first one restored")
	}
}

// The guard adds to GIT_CONFIG_COUNT, so what the user already set stays, and
// reads the last value, which is the one git gets.
func TestGitGuardEnv(t *testing.T) {
	got := gitGuardEnv([]string{"PATH=/bin", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=a.b", "GIT_CONFIG_VALUE_0=c"})
	for _, want := range []string{
		"GIT_CONFIG_KEY_2=safe.bareRepository", "GIT_CONFIG_VALUE_2=explicit",
		"GIT_CONFIG_KEY_3=core.fsmonitor", "GIT_CONFIG_VALUE_3=false",
		"GIT_CONFIG_KEY_4=core.hooksPath", "GIT_CONFIG_VALUE_4=/dev/null",
		"GIT_CONFIG_COUNT=5",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("%q missing from %q", want, got)
		}
	}
	if got := gitGuardEnv([]string{"PATH=/bin"}); !slices.Contains(got, "GIT_CONFIG_KEY_0=safe.bareRepository") || !slices.Contains(got, "GIT_CONFIG_COUNT=3") {
		t.Errorf("no count yet: %q", got)
	}
}

// A folder the PR versions as a repository of its own: a `git status` inside it
// runs the fsmonitor its config names. The control proves it, and with Bazel's
// environment nothing runs.
func TestEmbeddedRepoDoesNotRunFsmonitor(t *testing.T) {
	state := hostileClone(t, "wrapper")
	sentinel := filepath.Join(state, "fsmonitor-ran")
	gitStatus := "#!/bin/sh\nmkdir -p vendor/lib/objects/info vendor/lib/refs/heads\n" +
		"echo 'ref: refs/heads/main' > vendor/lib/HEAD\n" +
		"printf '[core]\\n\\trepositoryformatversion = 0\\n\\tworktree = .\\n\\tfsmonitor = \"touch " + sentinel + "\"\\n' > vendor/lib/config\n" +
		"cd vendor/lib && git status >/dev/null 2>&1\ntouch \"$BAZEL_TEST_DIR/ran\"\necho report\n"
	bin := filepath.SplitList(os.Getenv("PATH"))[0]
	if err := os.WriteFile(filepath.Join(bin, "gitter"), []byte(gitStatus), 0o755); err != nil {
		t.Fatal(err)
	}

	// Control: the same script with no guard in its environment.
	if err := exec.Command("git", "--version").Run(); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	control := exec.Command("gitter")
	control.Dir = dir
	control.Stdin = strings.NewReader("")
	control.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := control.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Skip("this git does not run a nested repository's fsmonitor: the control has nothing to prove")
	}
	os.Remove(sentinel)

	cfg := config.Default()
	cfg.Agent.Checkout = true
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{{Name: "lens", Task: "x", Command: "gitter", Format: "plain"}}
	choice, err := cfg.ChoiceByName("lens")
	if err != nil {
		t.Fatal(err)
	}
	pr := testPR()
	pr.BaseRefName = ""
	if _, err := New(cfg).Review(context.Background(), pr, choice, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Error("the embedded repository ran its fsmonitor inside the agent's environment")
	}
}
