package server

import (
	"os"
	"path/filepath"
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
	// No test reaches the real gh, with the user's login: a gh that refuses
	// comes first on PATH, and a test that wants answers puts its own before it.
	fence := filepath.Join(home, "fence")
	if err := os.MkdirAll(fence, 0o700); err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(fence, "gh"), []byte("#!/bin/sh\necho 'gh: the tests never reach the real gh' >&2\nexit 1\n"), 0o755); err != nil {
		panic(err)
	}
	os.Setenv("PATH", fence+string(os.PathListSeparator)+os.Getenv("PATH"))
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
