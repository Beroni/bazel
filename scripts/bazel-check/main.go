// bazel-check runs a built-in skill over a real PR with Bazel's own Runner and
// checks what came out. It costs tokens: a smoke test before shipping a skill
// change, not a unit test. Because it is the Runner, it cannot drift from what
// Bazel does: clone, plugin, flags, neutralizing, prompt.
//
//	go run ./scripts/bazel-check <owner/repo> <pr> <skill> [model]
//	go run ./scripts/bazel-check beroni/bazel 25 bazel-review-fleet sonnet
//
// It never publishes: the prompt is Bazel's default one, which forbids writing
// to GitHub, and bazel-post-report is refused. The PR needs a diff against its
// base, so a merged one will not do.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/beroni/bazel/internal/agent"
	"github.com/beroni/bazel/internal/config"
	"github.com/beroni/bazel/internal/gh"
	"github.com/beroni/bazel/internal/skills"
	"github.com/beroni/bazel/internal/store"
)

func main() {
	if len(os.Args) < 4 {
		fatal("usage: bazel-check <owner/repo> <pr> <skill> [model]")
	}
	repo := os.Args[1]
	skill, ok := readOnly(os.Args[3])
	if !ok {
		fatal("not a built-in skill that only reads: " + os.Args[3])
	}
	number, err := strconv.Atoi(os.Args[2])
	if err != nil {
		fatal("the PR is a number")
	}

	ctx := context.Background()
	pr, err := gh.Get(ctx, repo, number)
	if err != nil {
		fatal(err.Error())
	}
	home, err := os.MkdirTemp("", "bazel-check-")
	if err != nil {
		fatal(err.Error())
	}
	defer os.RemoveAll(home)
	os.Setenv("BAZEL_HOME", home)

	cfg := config.Default()
	if len(os.Args) > 4 {
		cfg.Agent.Args = append(cfg.Agent.Args, "--model", os.Args[4])
	}
	cfg.Agents = []config.AgentDef{{Name: skill, Task: "/" + skill + " {{number}}"}}
	choice, err := cfg.ChoiceByName(skill)
	if err != nil {
		fatal(err.Error())
	}

	asked := false
	res, err := agent.New(cfg).Review(ctx, pr, choice, func(e agent.Event) {
		if e.Kind == agent.EventLog {
			fmt.Println(e.Text)
			asked = asked || strings.Contains(e.Text, "AskUserQuestion")
		}
	})
	if err != nil {
		fatal(err.Error())
	}
	report := filepath.Join(home, "report.md")
	if err := os.WriteFile(report, []byte(res.Body), 0o600); err != nil {
		fatal(err.Error())
	}
	keep := filepath.Join(os.TempDir(), fmt.Sprintf("bazel-check-%s-%d.md", skill, number))
	if err := os.WriteFile(keep, []byte(res.Body), 0o600); err != nil {
		fatal(err.Error())
	}

	failed := false
	check := func(name string, ok bool) {
		mark := "ok  "
		if !ok {
			mark, failed = "FAIL", true
		}
		fmt.Printf("  %s %s\n", mark, name)
	}
	fmt.Printf("\n%s, %s, %d tokens\n", skill, res.Duration.Round(1e9), res.Usage.Total())
	check("report is not empty", strings.TrimSpace(res.Body) != "")
	check("skill never asked a question", !asked)
	check("report starts with its header", strings.HasPrefix(res.Body, "<!-- review-fleet:v1") || strings.HasPrefix(res.Body, "<!-- history-pr:v1"))
	check("no file path after the report", !strings.Contains(lastLines(res.Body, 3), ".reviews/"))
	if strings.HasPrefix(res.Body, "<!-- review-fleet:v1") {
		plugin := filepath.Join(home, "plugin")
		if err := skills.Materialize(plugin); err != nil {
			fatal(err.Error())
		}
		out, err := exec.Command("bash", filepath.Join(plugin, "skills", "bazel-review-fleet", "references", "validate-report.sh"), report).CombinedOutput()
		check("validate-report.sh passes", err == nil)
		if err != nil {
			fmt.Print(string(out))
		}
	}
	if strings.HasPrefix(res.Body, "<!-- history-pr:v1") {
		h := store.ParseHistory(res.Body)
		check("the History tab reads its header", h.Meta["repo"] != "" && h.Meta["pr"] != "")
		check("the History tab reads what it does", strings.TrimSpace(h.WhatItDoes) != "")
		check("the History tab reads the decisions", len(h.Decisions) > 0)
		check("commits read match the header", strconv.Itoa(h.CommitsRead) == h.Meta["commits"])
		fmt.Printf("  %d decision(s), %d still open\n", len(h.Decisions), len(h.StillOpen))
	}
	fmt.Printf("  %d finding(s) on the page\n  report: %s\n", store.Findings(res.Body), keep)
	if failed {
		os.Exit(1)
	}
}

// readOnly normalizes a skill's name, once, as the Runner will, and says whether
// it is one this tool may run: the task it builds must never be the publisher.
func readOnly(name string) (string, bool) {
	skill := skills.TaskSkill(skills.Canonical("/" + strings.TrimPrefix(strings.TrimSpace(name), "/")))
	return skill, skills.IsBuiltin(skill) && !skills.IsPost("/"+skill)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "bazel-check:", msg)
	os.Exit(2)
}
