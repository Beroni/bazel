package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func escreve(t *testing.T, path, conteudo string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(conteudo), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A skill is a folder with SKILL.md; the name and description come from the
// frontmatter, with long descriptions rejoined from several lines.
func TestListReadsFrontmatter(t *testing.T) {
	dir := t.TempDir()
	escreve(t, filepath.Join(dir, "review-fleet", "SKILL.md"), `---
name: review-fleet
description: Roda uma frota de lentes de code review sobre UM mesmo diff —
  precisão, recall adversarial e over-engineering, deduplicando os achados
  entre as lentes.
allowed-tools: Read Grep Bash
---

# corpo que não interessa
`)
	escreve(t, filepath.Join(dir, "sem-frontmatter", "SKILL.md"), "só o corpo\n")
	escreve(t, filepath.Join(dir, "solto.md"), "não é skill\n")
	if err := os.MkdirAll(filepath.Join(dir, "pasta-vazia"), 0o755); err != nil {
		t.Fatal(err)
	}

	list := List(dir)
	if len(list) != 2 {
		t.Fatalf("esperava 2 skills, vieram %d: %+v", len(list), list)
	}
	fleet := list[0]
	if fleet.Name != "review-fleet" {
		t.Errorf("nome: %q", fleet.Name)
	}
	if want := "Roda uma frota de lentes de code review sobre UM mesmo diff — precisão, recall adversarial e over-engineering, deduplicando os achados entre as lentes."; fleet.Description != want {
		t.Errorf("descrição remontada errada:\n%q", fleet.Description)
	}
	// Without frontmatter, the folder name does.
	if list[1].Name != "sem-frontmatter" {
		t.Errorf("skill sem frontmatter devia usar o nome da pasta, veio %q", list[1].Name)
	}
}

// Skills are often symlinks to the repository where you version them —
// and to os.ReadDir a symlink is not a directory.
func TestListFollowsSymlinks(t *testing.T) {
	real := t.TempDir()
	escreve(t, filepath.Join(real, "exploit-digger", "SKILL.md"), "---\nname: exploit-digger\ndescription: caça brecha\n---\n")

	links := t.TempDir()
	if err := os.Symlink(filepath.Join(real, "exploit-digger"), filepath.Join(links, "exploit-digger")); err != nil {
		t.Skipf("sem symlink neste sistema: %v", err)
	}

	list := List(links)
	if len(list) != 1 || list[0].Name != "exploit-digger" {
		t.Fatalf("symlink devia contar como skill, veio %+v", list)
	}
}

// A directory that doesn't exist is an empty list, not an error.
func TestListMissingDir(t *testing.T) {
	if got := List(filepath.Join(t.TempDir(), "nao-existe")); got != nil {
		t.Errorf("esperava nada, veio %+v", got)
	}
}

func TestTaskSkill(t *testing.T) {
	casos := map[string]string{
		"/review-fleet {{number}} --post": "review-fleet",
		"/post-report":                    "post-report",
		"  /exploit-digger 482":           "exploit-digger",
		"revise o PR":                     "",
		"":                                "",
	}
	for task, want := range casos {
		if got := TaskSkill(task); got != want {
			t.Errorf("TaskSkill(%q) = %q, esperava %q", task, got, want)
		}
	}
}
