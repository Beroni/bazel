//go:build !windows

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/beroni/bazel/internal/config"
)

// shellStep is a choice that runs script in sh, outside any clone.
func shellStep(t *testing.T, script string) (*config.Config, config.Choice) {
	t.Helper()
	cfg := config.Default()
	cfg.Agent.Checkout = false
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{{Name: "lens", Task: "x", Command: "sh", Args: []string{"-c", script}, Format: "plain"}}
	choice, err := cfg.ChoiceByName("lens")
	if err != nil {
		t.Fatal(err)
	}
	return cfg, choice
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// waitDead gives the kernel a moment to reap a killed process.
func waitDead(t *testing.T, pid int) {
	t.Helper()
	for i := 0; i < 100 && alive(pid); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if alive(pid) {
		t.Errorf("process %d survived the step", pid)
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

// A background process does not outlive its step, whether the step passes or
// fails. The control proves the same script leaves it running on its own.
func TestBackgroundProcessDiesWithTheStep(t *testing.T) {
	for name, exit := range map[string]string{"success": "0", "failure": "3"} {
		t.Run(name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			script := "sleep 30 >/dev/null 2>&1 &\necho $! > " + pidFile + "\necho report\nexit " + exit + "\n"

			control := exec.Command("sh", "-c", script)
			control.Run()
			data, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			if !alive(pid) {
				t.Fatal("the control should leave the process running")
			}
			syscall.Kill(pid, syscall.SIGKILL)

			cfg, choice := shellStep(t, script)
			New(cfg).Review(context.Background(), testPR(), choice, nil)
			data, err = os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			waitDead(t, pid)
		})
	}
}

// Something that keeps the agent's output open does not keep the review waiting.
func TestChildHoldingOutputDoesNotHang(t *testing.T) {
	cfg, choice := shellStep(t, "sleep 30 &\necho report\n")
	start := time.Now()
	res, err := New(cfg).Review(context.Background(), testPR(), choice, nil)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if res.Body != "report" {
		t.Errorf("body = %q", res.Body)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("the review waited %s for a background process", took)
	}
}

// Cancelling kills what the agent started, not only the agent.
func TestCancelKillsTheGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	cfg, choice := shellStep(t, "sleep 30 >/dev/null 2>&1 &\necho $! > "+pidFile+".tmp\nmv "+pidFile+".tmp "+pidFile+"\nwait\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for i := 0; i < 500; i++ {
			if _, err := os.Stat(pidFile); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	if _, err := New(cfg).Review(ctx, testPR(), choice, nil); err == nil {
		t.Error("a cancelled review should return an error")
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("cancelling took %s", took)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal("the agent never started")
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	waitDead(t, pid)
}

// A process left behind by one step cannot put the PR's configuration back
// under the next one.
func TestBackgroundLoopDoesNotReachTheNextStep(t *testing.T) {
	state := hostileClone(t, "wrapper")
	loop := "#!/bin/sh\n(while :; do mkdir -p .claude; echo '{}' > .claude/settings.json; sleep 0.02; done) >/dev/null 2>&1 &\nsleep 0.2\necho looping\n"
	bin := filepath.SplitList(os.Getenv("PATH"))[0]
	if err := os.WriteFile(filepath.Join(bin, "looper"), []byte(loop), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Agent.Checkout = true
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{
		{Name: "first", Task: "x", Command: "looper", Format: "plain"},
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
		t.Error("the first step's background loop reached the second one")
	}
}
