// Command fake-agent stands in for claude/codex/grok/pi in the e2e harness. It
// reads the prompt on stdin like a real agent, records the call, and replays a
// recorded scenario to stdout.
//
// Environment:
//
//	FAKE_AGENT_SCENARIOS  directories with the scenario files, ':'-separated
//	FAKE_AGENT_SCENARIO   which one to play (file name, extension optional)
//	FAKE_AGENT_STATE      writable directory: calls.jsonl and the release files
//	FAKE_AGENT_GATE       optional: wait for FAKE_AGENT_STATE/<gate> before playing
//
// A scenario is replayed line by line. Lines are copied to stdout verbatim
// (JSON events for claude-stream/codex-json/grok-stream, Markdown for plain),
// except the directives, which start with "#!":
//
//	#!sleep <ms>         pause
//	#!stderr <text>      write a line to stderr
//	#!exit <code>        stop here with that exit code
//	#!wait <name>        block until FAKE_AGENT_STATE/<name> exists (max 120s)
//	#!block              block until killed (max 120s)
//	#!repeat <n> <text>  print text n times; {i} becomes 1..n
//	#!gh-review          publish the review file named in the prompt with gh
//	#!# <comment>        ignored
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	dir      = os.Getenv("FAKE_AGENT_SCENARIOS")
	name     = os.Getenv("FAKE_AGENT_SCENARIO")
	stateDir = os.Getenv("FAKE_AGENT_STATE")
)

func main() {
	if dir == "" || name == "" || stateDir == "" {
		fail(2, "fake-agent: FAKE_AGENT_SCENARIOS, FAKE_AGENT_SCENARIO and FAKE_AGENT_STATE must be set")
	}
	raw, _ := io.ReadAll(os.Stdin)
	prompt := string(raw)
	record(prompt)
	if gate := os.Getenv("FAKE_AGENT_GATE"); gate != "" {
		waitFor(filepath.Join(stateDir, gate))
	}

	path := scenarioPath()
	f, err := os.Open(path)
	if err != nil {
		fail(2, "fake-agent: scenario: "+err.Error())
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "#!") {
			fmt.Println(line)
			continue
		}
		verb, arg, _ := strings.Cut(strings.TrimPrefix(line, "#!"), " ")
		switch verb {
		case "#":
		case "sleep":
			ms, _ := strconv.Atoi(arg)
			time.Sleep(time.Duration(ms) * time.Millisecond)
		case "stderr":
			fmt.Fprintln(os.Stderr, arg)
		case "exit":
			code, _ := strconv.Atoi(arg)
			os.Exit(code)
		case "wait":
			waitFor(filepath.Join(stateDir, arg))
		case "block":
			time.Sleep(120 * time.Second)
		case "repeat":
			nStr, text, _ := strings.Cut(arg, " ")
			n, _ := strconv.Atoi(nStr)
			w := bufio.NewWriter(os.Stdout)
			for i := 1; i <= n; i++ {
				fmt.Fprintln(w, strings.ReplaceAll(text, "{i}", strconv.Itoa(i)))
			}
			w.Flush()
		case "gh-review":
			publish(prompt)
		default:
			fail(2, "fake-agent: unknown directive "+line)
		}
	}
}

func scenarioPath() string {
	for _, d := range filepath.SplitList(dir) {
		for _, ext := range []string{"", ".jsonl", ".txt", ".md"} {
			p := filepath.Join(d, name+ext)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
	}
	fail(2, "fake-agent: no scenario named "+name+" in "+dir)
	return ""
}

func waitFor(path string) {
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	fail(3, "fake-agent: gave up waiting for "+path)
}

// record appends the call to calls.jsonl: what the agent was given (argv,
// prompt, cwd), the confinement flags, the skills it could load, what the clone
// looked like, and the GROK_* env Bazel set.
//
// `skills` are the ones a real Claude Code would load from Bazel: those of
// --plugin-dir, or of <cwd>/.claude/skills on a base that still writes them in
// the clone. `builtin_skill_source` says which of the two served
// bazel-post-report, and `clone_skills` lists what is in the clone either way:
// with a hostile PR that is the PR's own.
func record(prompt string) {
	cwd, _ := os.Getwd()
	plugin := argValue("--plugin-dir")
	cloneSkills := skillsIn(filepath.Join(cwd, ".claude", "skills"))
	skills, source := skillsIn(filepath.Join(plugin, "skills")), "plugin"
	if plugin == "" {
		skills, source = cloneSkills, "clone"
	}
	if !slices.Contains(skills, "bazel-post-report") {
		source = ""
	}
	pluginName := ""
	if raw, err := os.ReadFile(filepath.Join(plugin, ".claude-plugin", "plugin.json")); plugin != "" && err == nil {
		var manifest struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(raw, &manifest)
		pluginName = manifest.Name
	}
	grok := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GROK_") {
			grok[k] = v
		}
	}
	_, grokCfgErr := os.Stat(filepath.Join(os.Getenv("GROK_HOME"), "config.toml"))
	gitStatus, _ := exec.Command("git", "status", "--porcelain").Output()
	gitExclude, _ := os.ReadFile(filepath.Join(cwd, ".git", "info", "exclude"))
	line, _ := json.Marshal(map[string]any{
		"scenario":             name,
		"argv":                 os.Args[1:],
		"cwd":                  cwd,
		"prompt":               prompt,
		"builtin_skill":        source != "",
		"builtin_skill_source": source,
		"skills":               skills,
		"clone_skills":         cloneSkills,
		"plugin_dir":           plugin,
		"plugin_name":          pluginName,
		"setting_sources_user": argValue("--setting-sources") == "user",
		"strict_mcp_config":    slices.Contains(os.Args[1:], "--strict-mcp-config"),
		"git_status":           string(gitStatus),
		"git_exclude":          string(gitExclude),
		"grok_env":             grok,
		"grok_home_config":     os.Getenv("GROK_HOME") != "" && grokCfgErr == nil,
		"at":                   time.Now().UTC().Format(time.RFC3339Nano),
	})
	_ = os.MkdirAll(stateDir, 0o755)
	f, err := os.OpenFile(filepath.Join(stateDir, "calls.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fail(2, "fake-agent: cannot record call: "+err.Error())
	}
	defer f.Close()
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	f.Write(append(line, '\n'))
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// skillsIn lists the skills under dir: <dir>/<name>/SKILL.md.
func skillsIn(dir string) []string {
	out := []string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "SKILL.md")); err == nil {
			out = append(out, e.Name())
		}
	}
	return out
}

// argValue reads a flag from argv, as `--flag value` or `--flag=value`.
func argValue(flag string) string {
	args := os.Args[1:]
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			return v
		}
	}
	return ""
}

var (
	reviewFileRe = regexp.MustCompile(`/bazel-post-report\s+(\S+)`)
	repoRe       = regexp.MustCompile(`The repository ([\w.-]+/[\w.-]+) is cloned`)
	numberRe     = regexp.MustCompile(`PR\s+#(\d+)`)
)

// publish is what the post skill does, minus the intelligence: it sends the
// review file it was handed to the PR as one review, through gh. With the
// fake gh on PATH that lands in the gh call log, which is what the tests read.
func publish(prompt string) {
	file := reviewFileRe.FindStringSubmatch(prompt)
	repo := repoRe.FindStringSubmatch(prompt)
	number := numberRe.FindStringSubmatch(prompt)
	if file == nil || repo == nil || number == nil {
		fail(4, "fake-agent: #!gh-review needs the review file and the PR in the prompt")
	}
	body, err := os.ReadFile(file[1])
	if err != nil {
		fail(4, "fake-agent: reading the review: "+err.Error())
	}
	payload, _ := json.Marshal(map[string]any{"event": "COMMENT", "body": string(body)})
	cmd := exec.Command("gh", "api", "--method", "POST",
		fmt.Sprintf("repos/%s/pulls/%s/reviews", repo[1], number[1]), "--input", "-")
	cmd.Stdin = strings.NewReader(string(payload))
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fail(5, "fake-agent: gh api failed: "+err.Error())
	}
}

func fail(code int, msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(code)
}
