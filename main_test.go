//go:build !windows

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Each signal cancels the context instead of killing the process: that cancel is
// what stops the jobs, and with them the agents in their own process groups. The
// test runs in a child of itself, the way a terminal would signal Bazel.
func TestShutdownSignalsCancel(t *testing.T) {
	if os.Getenv("BAZEL_TEST_SIGNAL_CHILD") == "1" {
		ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals...)
		defer stop()
		fmt.Println("ready")
		select {
		case <-ctx.Done():
			fmt.Println("cancelled")
		case <-time.After(20 * time.Second):
			fmt.Println("timed out")
		}
		return
	}
	for name, sig := range map[string]syscall.Signal{"SIGHUP": syscall.SIGHUP, "SIGTERM": syscall.SIGTERM, "SIGINT": syscall.SIGINT} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestShutdownSignalsCancel$")
			cmd.Env = append(os.Environ(), "BAZEL_TEST_SIGNAL_CHILD=1")
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			lines := bufio.NewScanner(out)
			if !lines.Scan() || lines.Text() != "ready" {
				t.Fatalf("the child did not start: %q", lines.Text())
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			var rest strings.Builder
			for lines.Scan() {
				rest.WriteString(lines.Text() + "\n")
			}
			if err := cmd.Wait(); err != nil {
				t.Errorf("%s killed the process instead of cancelling: %v", name, err)
			}
			if !strings.Contains(rest.String(), "cancelled") {
				t.Errorf("%s did not cancel the context: %q", name, rest.String())
			}
		})
	}
}
