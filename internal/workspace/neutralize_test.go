package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func touch(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// What makes a CLI execute something leaves the working tree; the code under
// review stays, and git still has every removed file for the review to read.
func TestNeutralizeKeepsTheFilesInGit(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	touch(t, filepath.Join(dir, ".claude", "settings.json"), `{"hooks":{}}`)
	touch(t, filepath.Join(dir, ".claude", "settings.local.json"), `{}`)
	touch(t, filepath.Join(dir, ".claude", "hooks", "run.sh"), "evil")
	touch(t, filepath.Join(dir, ".mcp.json"), `{}`)
	touch(t, filepath.Join(dir, ".codex", "config.toml"), "x")
	touch(t, filepath.Join(dir, ".grok", "config.toml"), "x")
	touch(t, filepath.Join(dir, "src", "main.go"), "package main")
	touch(t, filepath.Join(dir, "CLAUDE.md"), "instructions are text, not code")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "init")

	removed, err := Neutralize(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{".claude", ".codex", ".grok", ".mcp.json"}; !slices.Equal(removed, want) {
		t.Errorf("removed %v, want %v", removed, want)
	}
	for _, gone := range []string{".claude", ".codex", ".grok", ".mcp.json"} {
		if _, err := os.Lstat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s should be gone from the working tree: %v", gone, err)
		}
	}
	for _, kept := range []string{"src/main.go", "CLAUDE.md"} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Errorf("%s is not agent config and must stay: %v", kept, err)
		}
	}
	if got := gitIn(t, dir, "show", "HEAD:.claude/settings.json"); !strings.Contains(got, "hooks") {
		t.Errorf("the review must still read it through git, got %q", got)
	}

	// A second pass over a clean tree has nothing to report.
	if removed, err := Neutralize(dir); err != nil || len(removed) != 0 {
		t.Errorf("second pass: %v, %v", removed, err)
	}
}

// A link goes, and what it points at — outside the clone — is not touched.
func TestNeutralizeNeverFollowsSymlinks(t *testing.T) {
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "precious")
	touch(t, filepath.Join(sentinel, "settings.json"), "keep me")

	for name, link := range map[string]string{
		"folder":      ".claude",
		"mcp file":    ".mcp.json",
		"codex":       ".codex",
		"grok folder": ".grok",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Symlink(sentinel, filepath.Join(dir, link)); err != nil {
				t.Fatal(err)
			}
			if _, err := Neutralize(dir); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(dir, link)); !os.IsNotExist(err) {
				t.Errorf("the link should be gone: %v", err)
			}
			if data, err := os.ReadFile(filepath.Join(sentinel, "settings.json")); err != nil || string(data) != "keep me" {
				t.Fatalf("the target outside the clone was touched: %q, %v", data, err)
			}
		})
	}

	// settings.json itself as a link, inside a real .claude folder.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(sentinel, "settings.json")
	if err := os.Symlink(target, filepath.Join(dir, ".claude", "settings.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Neutralize(dir); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "keep me" {
		t.Errorf("a linked settings.json must not take its target along: %q, %v", data, err)
	}
}

// Claude Code discovers a nested .claude/skills when it reads a file there, so
// the sweep goes all the way down, and a link in the way is not walked through.
func TestNeutralizeSweepsSubfolders(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "pkg", "a", ".claude", "skills", "x", "SKILL.md"), "evil")
	touch(t, filepath.Join(dir, "pkg", ".mcp.json"), "{}")
	touch(t, filepath.Join(dir, "pkg", "keep.go"), "package pkg")
	touch(t, filepath.Join(dir, ".git", "config"), "kept")
	touch(t, filepath.Join(dir, ".git", ".claude", "x"), "git's own, not walked")

	outside := t.TempDir()
	touch(t, filepath.Join(outside, ".claude", "settings.json"), "keep me")
	if err := os.Symlink(outside, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}

	removed, err := Neutralize(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"pkg/.mcp.json", "pkg/a/.claude"}; !slices.Equal(removed, want) {
		t.Errorf("removed %v, want %v", removed, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "pkg", "keep.go")); err != nil {
		t.Error("code stays")
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "config")); err != nil {
		t.Error(".git is not touched")
	}
	if data, err := os.ReadFile(filepath.Join(outside, ".claude", "settings.json")); err != nil || string(data) != "keep me" {
		t.Errorf("the walk went through a link: %q, %v", data, err)
	}
}

func TestNeutralizeCleanTree(t *testing.T) {
	removed, err := Neutralize(t.TempDir())
	if err != nil || len(removed) != 0 {
		t.Errorf("nothing to neutralize: %v, %v", removed, err)
	}
}
