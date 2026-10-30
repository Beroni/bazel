package skills

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// builtinFS are the skills that travel inside the Bazel binary.
//
// Publishing needed a skill installed in ~/.claude/skills to work, and that
// made the "publish" button lie on every machine other than the one belonging
// to whoever wrote the skill: the agent ran, with declared permission to write
// to the PR, and with no instructions at all. Embedding is what makes the
// factory default true everywhere.
//
//go:embed builtin
var builtinFS embed.FS

// builtinRoot is the folder inside the embed.
const builtinRoot = "builtin"

// SkillsSubdir is where Claude Code looks for a project's skills, relative
// to the agent's working directory. One level only: `.claude/skills/<name>/`
// is discovered, `.claude/skills/<group>/<name>/` is not.
const SkillsSubdir = ".claude/skills"

// Builtin lists the embedded skills, in alphabetical order.
func Builtin() []Skill {
	entries, err := builtinFS.ReadDir(builtinRoot)
	if err != nil {
		return nil
	}
	out := make([]Skill, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s := Skill{Name: e.Name(), Dir: builtinRoot + "/" + e.Name(), Builtin: true}
		if data, err := builtinFS.ReadFile(s.Dir + "/SKILL.md"); err == nil {
			s.Name, s.Description = frontmatterOf(string(data), e.Name())
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// IsBuiltin says whether a name is an embedded skill. It is what lets the
// page show these as installed without looking for them on disk: they are in
// the binary, and there is nothing to install.
func IsBuiltin(name string) bool {
	name = strings.TrimPrefix(strings.TrimSpace(name), "/")
	for _, s := range Builtin() {
		if strings.EqualFold(s.Name, name) {
			return true
		}
	}
	return false
}

// Materialize writes the embedded skills to <dir>/.claude/skills, which is
// where Claude Code finds them when it runs with dir as its working directory.
//
// dir is the PR's temporary clone: the skill is born and dies with it, and the
// machine running Bazel gains nothing in ~/.claude/skills that the person did
// not ask for. The files also go into the clone's exclude, so they don't show
// up in the `git status` of an agent that looks at it.
func Materialize(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("no working directory to materialize the built-in skills into")
	}
	err := fs.WalkDir(builtinFS, builtinRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(builtinRoot, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		dest := filepath.Join(dir, SkillsSubdir, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		data, err := builtinFS.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0o644)
	})
	if err != nil {
		return fmt.Errorf("writing the built-in skills into %s: %w", dir, err)
	}
	excludeClaude(dir)
	return nil
}

// excludeClaude adds .claude/ to the clone's exclude. It is best effort: the
// clone is disposable, and an agent that runs `git status` has no reason to
// see the skill Bazel just wrote there. Without a repository, there is nothing to do.
func excludeClaude(dir string) {
	info := filepath.Join(dir, ".git", "info")
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return
	}
	if err := os.MkdirAll(info, 0o755); err != nil {
		return
	}
	path := filepath.Join(info, "exclude")
	atual, _ := os.ReadFile(path)
	if strings.Contains(string(atual), "\n.claude/") || strings.HasPrefix(string(atual), ".claude/") {
		return
	}
	linha := ".claude/\n"
	if len(atual) > 0 && !strings.HasSuffix(string(atual), "\n") {
		linha = "\n" + linha
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(linha)
}
