package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"unicode"
)

// untrustedConfig are the names that make an agent CLI run something: Claude
// Code's settings hooks, MCP servers, commands, and skills and agents that can
// carry hooks of their own; the same for Codex, Grok, Gemini and Cursor, and for
// `.agents`, the shared skills folder several of them read. Whole folders go,
// not a list of files inside them: what a CLI executes from its folder changes
// between versions, and a list only has to miss one.
var untrustedConfig = []string{".claude", ".codex", ".grok", ".gemini", ".cursor", ".agents", ".mcp.json"}

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
//
// It keeps names relative to the clone and removes through an os.Root: a PR can
// hold a path git accepts but that, with the temporary folder in front, no
// longer fits in PATH_MAX, and that must not stop a review of it.
func Neutralize(dir string) ([]string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("removing agent configuration from the clone: %w", err)
	}
	defer root.Close()
	n := &neutralizer{dir: dir, root: root, slots: make(chan struct{}, runtime.GOMAXPROCS(0))}
	n.sweep(".")
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
	dir   string
	root  *os.Root
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
	entries, err := n.list(path)
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
		case e.IsDir() && entry != ".git":
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
	if _, err := n.root.Lstat(path); err != nil {
		if !os.IsNotExist(err) {
			n.fail(err)
		}
		return
	}
	if err := n.root.RemoveAll(path); err != nil {
		n.fail(err)
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.removed = append(n.removed, path)
}

// list reads one folder of the clone. The whole path first: going through the
// Root walks every folder on the way, several times slower on a big tree, and
// this runs over all of it before each step. So only a path too long for that
// takes the Root.
func (n *neutralizer) list(path string) ([]fs.DirEntry, error) {
	entries, err := os.ReadDir(filepath.Join(n.dir, path))
	if !errors.Is(err, syscall.ENAMETOOLONG) {
		return entries, err
	}
	f, err := n.root.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

// mayFold says whether a disk could fold name into one of untrustedConfig: they
// all start with a dot, and only a name outside ASCII can hide that.
func mayFold(name string) bool {
	return strings.HasPrefix(name, ".") || strings.IndexFunc(name, func(r rune) bool { return r > unicode.MaxASCII }) >= 0
}
