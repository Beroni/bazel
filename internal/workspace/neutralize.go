package workspace

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"unicode"
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
// In each folder that has a name it could be, it asks the disk first, by the name
// a CLI opens: `.CLAUDE`, `.Claude` or any other spelling a filesystem folds
// into `.claude` (case on APFS, Unicode forms, a trailing dot on NTFS) answers to
// that Lstat, and there is no list of variants to get wrong. Then it compares
// the names it listed, in any case, so a clone looks the same on every machine.
// No folder is skipped, node_modules included: a `.claude` is one wherever it is.
//
// The clone is disposable, and the review loses nothing: the files are still
// in git, in the diff and in `git show HEAD:<path>`. Only the working tree
// changes. Neither the walk nor os.RemoveAll follow a symlink: a `.claude` that
// links elsewhere loses the link, and what it pointed at is not touched.
func Neutralize(dir string) ([]string, error) {
	n := &neutralizer{root: dir, slots: make(chan struct{}, runtime.GOMAXPROCS(0))}
	n.sweep(dir)
	n.wg.Wait()
	slices.Sort(n.removed)
	if n.err != nil {
		return n.removed, fmt.Errorf("removing agent configuration from the clone: %w", n.err)
	}
	return n.removed, nil
}

// neutralizer sweeps one clone. Folders are independent of each other and this
// runs before each step over the whole tree, so a folder goes to another
// goroutine when there is a free slot: listing folders is what takes the time.
type neutralizer struct {
	root  string
	slots chan struct{}
	wg    sync.WaitGroup

	mu      sync.Mutex
	removed []string
	err     error
}

func (n *neutralizer) fail(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.err == nil {
		n.err = err
	}
}

func (n *neutralizer) failed() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.err != nil
}

// sweep clears one folder and goes down its subfolders. It does one listing per
// folder and, in the usual folder with no dotfile, no other system call.
func (n *neutralizer) sweep(path string) {
	if n.failed() {
		return
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		n.fail(err)
		return
	}
	if slices.ContainsFunc(entries, func(e fs.DirEntry) bool { return mayFold(e.Name()) }) {
		for _, name := range untrustedConfig {
			n.removeIfThere(filepath.Join(path, name))
		}
	}
	for _, e := range entries {
		entry := filepath.Join(path, e.Name())
		switch {
		case isUntrusted(e.Name()):
			n.removeIfThere(entry)
		case e.IsDir() && entry != filepath.Join(n.root, ".git"):
			select {
			case n.slots <- struct{}{}:
				n.wg.Add(1)
				go func() {
					defer n.wg.Done()
					defer func() { <-n.slots }()
					n.sweep(entry)
				}()
			default:
				n.sweep(entry)
			}
		}
	}
}

// removeIfThere removes path, unless an earlier spelling of it took it along.
func (n *neutralizer) removeIfThere(path string) {
	if _, err := os.Lstat(path); err != nil {
		if !os.IsNotExist(err) {
			n.fail(err)
		}
		return
	}
	if err := os.RemoveAll(path); err != nil {
		n.fail(err)
		return
	}
	rel, err := filepath.Rel(n.root, path)
	if err != nil {
		n.fail(err)
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.removed = append(n.removed, rel)
}

// mayFold says whether a disk could fold name into one of untrustedConfig: they
// all start with a dot, and only a name outside ASCII can hide that.
func mayFold(name string) bool {
	return strings.HasPrefix(name, ".") || strings.IndexFunc(name, func(r rune) bool { return r > unicode.MaxASCII }) >= 0
}
