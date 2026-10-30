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

// PluginName is the Claude Code plugin the embedded skills are packaged as.
// Claude Code also finds a plugin's skills by their bare name, so a task keeps
// calling `/bazel-post-report`.
const PluginName = "bazel"

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

// Materialize writes the embedded skills into dir as a Claude Code plugin —
// dir/.claude-plugin/plugin.json, dir/skills/<name>/…, and any
// dir/agents/<name>.md — for the agent to load with --plugin-dir.
//
// dir is a fresh folder Bazel owns, outside the PR's clone. Nothing is
// written to the clone: it is the PR author's tree, where a directory can be
// a symlink pointing anywhere, and where Claude Code would also read the PR's
// own .claude/ (settings, hooks, skills) next to Bazel's. The agent runs with
// --setting-sources user, which ignores that project configuration, and finds
// the embedded skills through the plugin instead.
func Materialize(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("no directory to materialize the built-in skills into")
	}
	manifest := fmt.Sprintf("{\n  \"name\": %q,\n  \"description\": \"Skills that ship inside Bazel\"\n}\n", PluginName)
	if err := writeFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), []byte(manifest)); err != nil {
		return fmt.Errorf("writing the built-in plugin into %s: %w", dir, err)
	}
	err := fs.WalkDir(builtinFS, builtinRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(builtinRoot, p)
		if err != nil {
			return err
		}
		data, err := builtinFS.ReadFile(p)
		if err != nil {
			return err
		}
		if err := writeFile(filepath.Join(dir, "skills", rel), data); err != nil {
			return err
		}
		// A subagent is found by name, not by the skill that uses it: the
		// plugin's agents/ is where Claude Code looks.
		if agent, ok := agentFile(rel); ok {
			return writeFile(filepath.Join(dir, "agents", agent), data)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("writing the built-in skills into %s: %w", dir, err)
	}
	return nil
}

// writeFile writes data to path, creating the folders on the way.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// agentFile says whether rel (relative to the embed root) is a subagent
// definition — `<skill>/agents/<name>.md` — and returns its file name.
func agentFile(rel string) (string, bool) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 || parts[1] != "agents" || !strings.HasSuffix(parts[2], ".md") {
		return "", false
	}
	return parts[2], true
}
