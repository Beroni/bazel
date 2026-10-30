//go:build !windows

package server

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A signal at the terminal does not reach an agent in its own process group: it
// dies when its job sees the cancel, so shutting down has to wait for the job.
func TestShutdownLeavesNoAgentBehind(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	cfg := cfgFor(t, "sh")
	cfg.Agent.Args = []string{"-c", "sleep 30 >/dev/null 2>&1 &\necho $! > " + pidFile + ".tmp\nmv " + pidFile + ".tmp " + pidFile + "\nwait\n"}
	cfg.Agent.Format = "plain"
	cfg.Agent.Prompt = "{{task}}"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := NewManager(ctx, cfg, dir, 1, false, NewHub())
	if _, err := m.Enqueue(testPR(482), false, cfg.DefaultChoice()); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	for i := 0; i < 500; i++ {
		if _, err := os.Stat(pidFile); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal("the agent never started")
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))

	cancel()
	m.Wait(10 * time.Second)
	if m.running.Load() != 0 {
		t.Error("Wait returned with a job still running")
	}
	if err := syscall.Kill(pid, 0); err == nil || err == syscall.EPERM {
		// A killed child may need a moment to be reaped by init.
		time.Sleep(200 * time.Millisecond)
		if err := syscall.Kill(pid, 0); err == nil || err == syscall.EPERM {
			t.Errorf("process %d outlived the shutdown", pid)
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}
