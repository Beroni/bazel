package workspace

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// untrustedConfig are the names that make an agent CLI run something: Claude
// Code's settings hooks, MCP servers, commands, and skills and agents that can
// carry hooks of their own; the same for Codex and Grok. Whole folders go, not
// a list of files inside them: what a CLI executes from its folder changes
// between versions, and a list only has to miss one.
var untrustedConfig = map[string]bool{".claude": true, ".codex": true, ".grok": true, ".mcp.json": true}

// Neutralize removes untrustedConfig from the clone at dir, at any depth, before
// an agent runs there, and returns what it removed, relative to dir. It is what
// protects a run Bazel cannot confine with flags — a Claude Code behind an
// unknown wrapper, or any other CLI: the file is simply not there to be read.
// Any depth, because Claude Code also discovers `<sub>/.claude/skills` when it
// reads a file in that folder.
//
// The clone is disposable, and the review loses nothing: the files are still
// in git, in the diff and in `git show HEAD:<path>`. Only the working tree
// changes. Neither the walk nor os.RemoveAll follow a symlink: a `.claude` that
// links elsewhere loses the link, and what it pointed at is not touched.
func Neutralize(dir string) ([]string, error) {
	var removed []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !untrustedConfig[d.Name()] {
			return nil
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		removed = append(removed, rel)
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return removed, fmt.Errorf("removing agent configuration from the clone: %w", err)
	}
	return removed, nil
}
