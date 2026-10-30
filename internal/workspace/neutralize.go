package workspace

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// untrustedConfig are the names that make an agent CLI run something: Claude
// Code's settings hooks, MCP servers, commands, and skills and agents that can
// carry hooks of their own; the same for Codex and Grok. Whole folders go, not
// a list of files inside them: what a CLI executes from its folder changes
// between versions, and a list only has to miss one.
var untrustedConfig = []string{".claude", ".codex", ".grok", ".mcp.json"}

// isUntrusted says whether name is one of untrustedConfig, ignoring case: on a
// case-insensitive disk, the default on macOS, a PR's `.Claude/` is the folder
// a CLI opens as `.claude/`.
func isUntrusted(name string) bool {
	return slices.ContainsFunc(untrustedConfig, func(c string) bool { return strings.EqualFold(c, name) })
}

// Neutralize removes untrustedConfig from the clone at dir, at any depth, before
// an agent runs there, and returns what it removed, relative to dir. It is what
// protects a run Bazel cannot confine with flags — a Claude Code behind an
// unknown wrapper, or any other CLI: the file is simply not there to be read.
// Any depth, because Claude Code also discovers `<sub>/.claude/skills` when it
// reads a file in that folder.
//
// In each folder it asks the disk first, by the name a CLI opens: `.CLAUDE`,
// `.Claude` or any other spelling a filesystem folds into `.claude` (case on
// APFS, Unicode forms, a trailing dot on NTFS) answers to that Lstat, and there
// is no list of variants to get wrong. The walk then catches what the disk
// keeps apart, in any case, so a clone looks the same on every machine.
//
// The clone is disposable, and the review loses nothing: the files are still
// in git, in the diff and in `git show HEAD:<path>`. Only the working tree
// changes. Neither the walk nor os.RemoveAll follow a symlink: a `.claude` that
// links elsewhere loses the link, and what it pointed at is not touched.
func Neutralize(dir string) ([]string, error) {
	var removed []string
	remove := func(path string) error {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		removed = append(removed, rel)
		return nil
	}
	gitDir := filepath.Join(dir, ".git")
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == gitDir {
			return filepath.SkipDir
		}
		if d.IsDir() {
			for _, name := range untrustedConfig {
				entry := filepath.Join(path, name)
				if _, err := os.Lstat(entry); err == nil {
					if err := remove(entry); err != nil {
						return err
					}
				} else if !os.IsNotExist(err) {
					return err
				}
			}
			if path == dir {
				return nil
			}
		}
		if !isUntrusted(d.Name()) {
			return nil
		}
		if err := remove(path); err != nil {
			return err
		}
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
