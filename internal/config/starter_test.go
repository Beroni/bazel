package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beroni/bazel/internal/skills"
)

// A brand-new config comes with agents built from the skills that ship inside
// Bazel, so the first review works right after install.
func TestLoadOrInitSeedsStarterAgents(t *testing.T) {
	t.Setenv("BAZEL_HOME", t.TempDir())
	cfg, created, err := LoadOrInit()
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("the config should have been created")
	}
	if cfg.Default != "bazel-review-fleet" {
		t.Errorf("default = %q, want bazel-review-fleet", cfg.Default)
	}
	for _, a := range cfg.Agents {
		skill := strings.Fields(strings.TrimPrefix(a.Task, "/"))[0]
		if !skills.IsBuiltin(skill) {
			t.Errorf("starter agent %s calls %s, which does not ship inside Bazel", a.Name, skill)
		}
		if a.Name != skill {
			t.Errorf("starter agent %s should be named after its skill %s, as the page names agents", a.Name, skill)
		}
		if a.Posts {
			t.Errorf("%s publishes on its own; publishing without reading stays your choice", a.Name)
		}
	}
	history, err := cfg.ChoiceByName("bazel-history-pr")
	if err != nil {
		t.Fatal(err)
	}
	if history.Publishable {
		t.Error("a PR history is not a review and must not be publishable")
	}
	for _, name := range []string{"serial-fleet", "review-then-publish"} {
		ch, err := cfg.ChoiceByName(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !ch.Pipeline || len(ch.Steps) < 2 {
			t.Errorf("%s should resolve to a pipeline, got %+v", name, ch)
		}
	}

	// What was written is what loads back.
	again, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Agents) != len(cfg.Agents) || len(again.Pipelines) != len(cfg.Pipelines) {
		t.Errorf("reloaded %d agents / %d pipelines, want %d / %d",
			len(again.Agents), len(again.Pipelines), len(cfg.Agents), len(cfg.Pipelines))
	}
}

// A config that already exists is never seeded, not even an empty one: empty
// may be what you chose.
func TestLoadOrInitLeavesExistingConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BAZEL_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("repos: [acme/api]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, created, err := LoadOrInit()
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("an existing config is not created again")
	}
	if len(cfg.Agents) != 0 || len(cfg.Pipelines) != 0 || cfg.Default != "" {
		t.Errorf("an existing config must not be seeded: %+v / %+v / %q", cfg.Agents, cfg.Pipelines, cfg.Default)
	}
}
