// Package skills discovers the Claude Code skills installed on the machine.
//
// It is what lets the page show the agents that are actually available — the
// ones in ~/.claude/skills — instead of a hand-written list that may point to
// a skill you never installed.
package skills

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Skill is an installed skill.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Dir is its folder, useful when the directory name and the frontmatter one
	// don't match.
	Dir string `json:"dir"`
	// Builtin marks the skill that came inside the Bazel binary. It is not
	// anywhere on disk until a review runs, and it is always available.
	Builtin bool `json:"builtin,omitempty"`
}

// DefaultDir is where Claude Code keeps the user's skills.
func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "skills")
}

// List returns the skills in a directory, in alphabetical order. A missing
// directory is an empty list: not every machine has skills installed, and
// that is not an error.
func List(dir string) []Skill {
	if strings.TrimSpace(dir) == "" {
		dir = DefaultDir()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Skill
	for _, e := range entries {
		// No IsDir filter: skills are often symlinks to the repository
		// where you version them, and to os.ReadDir a symlink is not a
		// directory. What decides is the SKILL.md inside.
		s, ok := read(filepath.Join(dir, e.Name()))
		if !ok {
			continue
		}
		if s.Name == "" {
			s.Name = e.Name()
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// read reads a SKILL.md's frontmatter. A folder without SKILL.md is not a skill.
func read(dir string) (Skill, bool) {
	f, err := os.Open(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return Skill{}, false
	}
	defer f.Close()

	s := Skill{Dir: dir}
	s.Name, s.Description = parseFrontmatter(f)
	return s, true
}

// frontmatterOf reads the frontmatter of a SKILL.md already in memory — it is
// how the skills embedded in the binary describe themselves, without going
// through the disk. With no `name` in the frontmatter, the folder name is used.
func frontmatterOf(content, fallback string) (name, description string) {
	name, description = parseFrontmatter(strings.NewReader(content))
	if name == "" {
		name = fallback
	}
	return name, description
}

// parseFrontmatter reads the YAML block between the two `---` at the top. Only
// name and description matter: they are what the page shows.
func parseFrontmatter(r io.Reader) (name, description string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return "", ""
	}

	key := ""
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		// An indented line continues the previous value: long descriptions come
		// broken across several lines in the YAML.
		if key != "" && (strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t")) {
			if key == "description" {
				description = strings.TrimSpace(description + " " + strings.TrimSpace(line))
			}
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			key = ""
			continue
		}
		key = strings.TrimSpace(name)
		value = strings.TrimSpace(strings.Trim(strings.TrimSpace(value), `"'`))
		switch key {
		case "name":
			name = value
		case "description":
			description = value
		}
	}
	return name, description
}

// TaskSkill returns the skill a task invokes — the "/review-fleet" of
// "/review-fleet {{number}} --post". A task that doesn't start with a slash
// calls no skill.
func TaskSkill(task string) string {
	task = strings.TrimSpace(task)
	if !strings.HasPrefix(task, "/") {
		return ""
	}
	name := strings.TrimPrefix(task, "/")
	if i := strings.IndexAny(name, " \t\n"); i >= 0 {
		name = name[:i]
	}
	return name
}

// Has says whether a skill is among the installed ones.
func Has(list []Skill, name string) bool {
	for _, s := range list {
		if strings.EqualFold(s.Name, name) {
			return true
		}
	}
	return false
}
