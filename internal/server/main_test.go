package server

import (
	"os"
	"testing"
)

// TestMain points BAZEL_HOME at a throwaway folder: a review that runs a
// Claude Code step writes the embedded skills under <BAZEL_HOME>/runtime, and
// a test run must never touch the user's ~/.bazel.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "bazel-server-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("BAZEL_HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
