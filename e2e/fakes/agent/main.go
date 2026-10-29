// Command fake-agent stands in for claude/codex/grok/pi in the e2e harness. It
// reads the prompt on stdin like a real agent, records the call, and replays a
// recorded scenario to stdout.
//
// Environment:
//
//	FAKE_AGENT_SCENARIOS  directory with the scenario files
//	FAKE_AGENT_SCENARIO   which one to play (file name, extension optional)
//	FAKE_AGENT_STATE      writable directory: calls.jsonl and the release files
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
		case "gh-review":
			publish(prompt)
		default:
			fail(2, "fake-agent: unknown directive "+line)
		}
	}
}

func scenarioPath() string {
	for _, ext := range []string{"", ".jsonl", ".txt", ".md"} {
		p := filepath.Join(dir, name+ext)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
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

// record appends the call to calls.jsonl. builtin_skill says whether Bazel
// materialized its bazel-post-report skill in the working directory.
func record(prompt string) {
	cwd, _ := os.Getwd()
	_, skillErr := os.Stat(filepath.Join(cwd, ".claude", "skills", "bazel-post-report", "SKILL.md"))
	line, _ := json.Marshal(map[string]any{
		"scenario":      name,
		"argv":          os.Args[1:],
		"cwd":           cwd,
		"prompt":        prompt,
		"builtin_skill": skillErr == nil,
		"at":            time.Now().UTC().Format(time.RFC3339Nano),
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
