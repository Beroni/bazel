//go:build !windows

package config

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
)

// A failed write leaves the old file whole. The child process runs with a file
// size limit, the way a full disk fails.
func TestSaveFalhaNaoTrunca(t *testing.T) {
	if os.Getenv("BAZEL_SAVE_LIMIT") == "1" {
		signal.Ignore(syscall.SIGXFSZ)
		lim := syscall.Rlimit{Cur: 32, Max: 32}
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
			t.Fatal(err)
		}
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		c.AddRepo("acme/web")
		if err := c.Save(); err == nil {
			t.Fatal("expected the limit to fail the save")
		}
		return
	}
	home := t.TempDir()
	path := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(path, []byte(handWritten), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSaveFalhaNaoTrunca$")
	cmd.Env = append(os.Environ(), "BAZEL_SAVE_LIMIT=1", "BAZEL_HOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if data, _ := os.ReadFile(path); string(data) != handWritten {
		t.Errorf("the file changed:\n%s", data)
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 1 {
		t.Errorf("left behind: %v", entries)
	}
}
