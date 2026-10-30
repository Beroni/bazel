package workspace

import (
	"fmt"
	"os"
	"path/filepath"
)

// untrustedConfig are the paths at the root of a PR's working tree that make
// an agent CLI run something: Claude Code's settings hooks and MCP servers,
// skills and agents that can carry hooks of their own, and the same for Codex
// and Grok. Whole folders go, not a list of files inside them: what a CLI
// executes from its folder changes between versions, and a list only has to
// miss one.
var untrustedConfig = []string{".claude", ".codex", ".grok", ".mcp.json"}

// Neutralize removes untrustedConfig from the clone at dir before an agent
// runs there, and returns what it removed. It is what protects a run Bazel
// cannot confine with flags — a Claude Code behind an unknown wrapper, or any
// other CLI: the file is simply not there to be read.
//
// The clone is disposable, and the review loses nothing: the files are still
// in git, in the diff and in `git show HEAD:<path>`. Only the working tree
// changes. os.RemoveAll never follows a symlink: a `.claude` that links
// elsewhere loses the link, and what it pointed at is not touched.
func Neutralize(dir string) ([]string, error) {
	var removed []string
	for _, rel := range untrustedConfig {
		path := filepath.Join(dir, rel)
		if _, err := os.Lstat(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, fmt.Errorf("checking %s in the clone: %w", rel, err)
		}
		if err := os.RemoveAll(path); err != nil {
			return removed, fmt.Errorf("removing %s from the clone: %w", rel, err)
		}
		removed = append(removed, rel)
	}
	return removed, nil
}
