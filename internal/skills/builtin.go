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

// AgentsSubdir is where Claude Code looks for a project's subagents. A
// built-in skill that fires lenses keeps their definitions in its own
// `agents/` folder, and they are copied here as well: a subagent is found by
// name, not by the skill that uses it. A project agent wins over one of the
// same name in ~/.claude/agents, so the lens that runs is the one Bazel ships.
const AgentsSubdir = ".claude/agents"

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
	if err := writeFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), []byte(manifest), 0o644); err != nil {
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
		// The skills call their scripts with `bash <path>`, but an executable
		// bit costs nothing and spares an agent that runs one directly.
		mode := fs.FileMode(0o644)
		if strings.HasSuffix(p, ".sh") {
			mode = 0o755
		}
		if err := writeFile(filepath.Join(dir, "skills", rel), data, mode); err != nil {
			return err
		}
		// A subagent is found by name, not by the skill that uses it: the
		// plugin's agents/ is where Claude Code looks.
		if agent, ok := agentFile(rel); ok {
			return writeFile(filepath.Join(dir, "agents", agent), data, 0o644)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("writing the built-in skills into %s: %w", dir, err)
	}
	return nil
}

// writeFile writes data to path, creating the folders on the way.
func writeFile(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, mode)
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

// Inline turns a task that calls a built-in skill into that skill's
// instructions, for agent CLIs that do not load Claude Code skills — Codex
// and Grok read "/bazel-review-fleet 123" as plain text. The SKILL.md body
// goes into the prompt with the task's arguments, and plugin is the folder
// Materialize wrote: the files the skill mentions are there. A task that does
// not call a built-in skill comes back as it was.
func Inline(task, plugin string) string {
	name := TaskSkill(task)
	if name == "" || !IsBuiltin(name) {
		return task
	}
	var body string
	for _, s := range Builtin() {
		if strings.EqualFold(s.Name, name) {
			data, err := builtinFS.ReadFile(s.Dir + "/SKILL.md")
			if err != nil {
				return task
			}
			body = withoutFrontmatter(string(data))
			break
		}
	}
	args := strings.TrimSpace(strings.TrimSpace(task)[1+len(name):])
	if args == "" {
		args = "none"
	}
	return fmt.Sprintf("Run the `%s` skill below. This agent does not load skills by itself, so its "+
		"instructions are inlined here. Its files are in %s/skills/%s/ and the agents it names in "+
		"%s/agents/.\n\nArguments: %s\n\n%s", name, plugin, name, plugin, args, body)
}

// withoutFrontmatter drops the YAML block between the two `---` at the top.
func withoutFrontmatter(content string) string {
	rest, ok := strings.CutPrefix(content, "---\n")
	if !ok {
		return strings.TrimSpace(content)
	}
	if _, body, ok := strings.Cut(rest, "\n---\n"); ok {
		return strings.TrimSpace(body)
	}
	return strings.TrimSpace(content)
}
