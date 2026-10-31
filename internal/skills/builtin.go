package skills

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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
	_, ok := builtinNamed(name)
	return ok
}

// builtinNamed finds an embedded skill by name, with or without the slash and
// ignoring case — one normalization for every caller, so IsBuiltin and Inline
// can never disagree about whether a task calls a built-in.
func builtinNamed(name string) (Skill, bool) {
	name = Bare(strings.TrimPrefix(strings.TrimSpace(name), "/"))
	for _, s := range Builtin() {
		if strings.EqualFold(s.Name, name) {
			return s, true
		}
	}
	return Skill{}, false
}

// Canonical rewrites a task that calls a built-in skill so the slash command
// carries the skill's exact name: `/BAZEL-POST-REPORT x` becomes
// `/bazel-post-report x`. Bazel matches names ignoring case; the agent CLI may
// not, and a publishing step that silently runs without its skill would still
// have the permission to post. Any other task comes back as it was.
func Canonical(task string) string {
	name := TaskSkill(task)
	s, ok := builtinNamed(name)
	if !ok {
		return task
	}
	want := s.Name
	if Bare(name) != name {
		want = pluginPrefix + s.Name
	}
	if name == want {
		return task
	}
	trimmed := strings.TrimSpace(task)
	return "/" + want + trimmed[1+len(name):]
}

// pluginPrefix is how Claude Code names a skill of Bazel's plugin. The bare name
// goes to any skill of that name the user or the project has; this one does not.
const pluginPrefix = "bazel:"

// Bare is a skill's name without the plugin's prefix.
func Bare(name string) string {
	if len(name) > len(pluginPrefix) && strings.EqualFold(name[:len(pluginPrefix)], pluginPrefix) {
		return name[len(pluginPrefix):]
	}
	return name
}

// PostTask points a task at the plugin's own publishing skill, whatever a saved
// config calls it: a homonym would publish with none of the script's checks. Any
// other task comes back as it was.
func PostTask(task string) string {
	if !IsPost(task) {
		return task
	}
	name := TaskSkill(task)
	trimmed := strings.TrimSpace(task)
	return "/" + pluginPrefix + postSkill + trimmed[1+len(name):]
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

// postSkill is the built-in skill that writes to the PR.
const postSkill = "bazel-post-report"

// IsPost says whether a task runs the built-in publishing skill.
func IsPost(task string) bool {
	return strings.EqualFold(Bare(TaskSkill(task)), postSkill)
}

// PublishTarget leaves in the plugin folder the PR a publishing step is for. The
// script checks the review against it: everything else it knows comes from the
// review file, which cannot vouch for itself.
func PublishTarget(plugin, repo string, number int) error {
	return os.WriteFile(filepath.Join(plugin, "publish-target"), fmt.Appendf(nil, "%s#%d\n", repo, number), 0o644)
}

// PublishFailure reads what bazel-post-report's script left in the plugin folder
// and returns "" only if it says the review reached the PR. An agent's exit code
// and its words say nothing about that: the script refusing is a successful run
// for the model, and a model can stop before it ever calls the script. Anything
// but a proof of publishing is a failure.
func PublishFailure(plugin string) string {
	data, err := os.ReadFile(filepath.Join(plugin, "publish-outcome"))
	status, msg, _ := strings.Cut(string(data), "\n")
	msg = strings.TrimSpace(msg)
	switch {
	case err == nil && status == "published":
		return ""
	case err == nil && msg != "" && slices.Contains([]string{"refused", "failed", "nothing"}, status):
		return msg
	}
	return "Not published: the publishing script never finished, so nothing is known to have reached the PR. Check the PR, then publish again."
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
	s, ok := builtinNamed(name)
	if !ok {
		return task
	}
	data, err := builtinFS.ReadFile(s.Dir + "/SKILL.md")
	body := withoutFrontmatter(string(data))
	if err != nil || body == "" {
		return task
	}
	args := strings.TrimSpace(strings.TrimSpace(task)[1+len(name):])
	if args == "" {
		args = "none"
	}
	return fmt.Sprintf("Run the `%s` skill below. This agent does not load skills by itself, so its "+
		"instructions are inlined here. `<plugin>` in them is %s: the skill's files are in "+
		"%s/skills/%s/, the agents it names in %s/agents/.\n\nArguments: %s\n\n%s",
		s.Name, plugin, plugin, s.Name, plugin, args, body)
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
