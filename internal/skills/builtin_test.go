package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The publishing skill travels in the binary: it is what makes the "publish"
// button work on a machine that never installed any skill.
func TestBuiltinTrazAPostReport(t *testing.T) {
	list := Builtin()
	if len(list) == 0 {
		t.Fatal("o binário devia trazer pelo menos uma skill")
	}
	var achou bool
	for _, s := range list {
		if s.Name == "bazel-post-report" {
			achou = true
			if !s.Builtin {
				t.Error("skill embarcada devia estar marcada como tal")
			}
			if s.Description == "" {
				t.Error("a descrição sai do frontmatter e é o que a página mostra")
			}
		}
	}
	if !achou {
		t.Errorf("bazel-post-report devia estar embarcada: %+v", list)
	}

	if !IsBuiltin("bazel-post-report") || !IsBuiltin("/bazel-post-report") {
		t.Error("IsBuiltin devia reconhecer com e sem a barra")
	}
	if IsBuiltin("post-report") {
		t.Error("a post-report do usuário não é embarcada — são skills diferentes, de propósito")
	}
}

// Materialize writes a Claude Code plugin into the folder it is given, and
// nowhere else: that folder is Bazel's, outside the PR's clone.
func TestMaterializeWritesAPlugin(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "runtime-x")
	if err := Materialize(dir); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	manifest, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatalf("plugin.json: %v", err)
	}
	if !strings.Contains(string(manifest), `"name": "bazel"`) {
		t.Errorf("the plugin is named %q: %s", PluginName, manifest)
	}
	data, err := os.ReadFile(filepath.Join(dir, "skills", "bazel-post-report", "SKILL.md"))
	if err != nil {
		t.Fatalf("the skill should be in the plugin's skills/: %v", err)
	}
	if !strings.Contains(string(data), "name: bazel-post-report") {
		t.Error("the SKILL.md written should be the embedded one")
	}

	// Nothing outside dir: no .claude/, no .git/info/exclude, nothing next to it.
	entries, _ := os.ReadDir(parent)
	if len(entries) != 1 || entries[0].Name() != "runtime-x" {
		t.Errorf("Materialize wrote outside its folder: %v", entries)
	}
	if err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && (d.Name() == ".claude" || d.Name() == ".git") {
			t.Errorf("Materialize wrote %s, which is a project's, not a plugin's", p)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Running again over the same folder is harmless.
	if err := Materialize(dir); err != nil {
		t.Fatalf("Materialize twice: %v", err)
	}
}

// Without a folder there is nowhere to write, and the current directory —
// maybe someone's project — is not a fallback.
func TestMaterializeRefusesNoDir(t *testing.T) {
	if err := Materialize("  "); err == nil {
		t.Error("Materialize with no folder should fail")
	}
}
