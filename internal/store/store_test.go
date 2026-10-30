package store

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/beroni/bazel/internal/agent"
)

// The name has second resolution: two reviews of one PR that end in the same
// second used to share a path, and the second wiped out the first (BUG-QA-02).
// Each write now gets its own file, and nothing already on disk is replaced.
func TestWriteNewNaoSobrescreve(t *testing.T) {
	dir := t.TempDir()
	base := "acme-api-core-482-20261029-120000"
	var paths []string
	for _, body := range []string{"first", "second", "third"} {
		p, err := writeNew(dir, base, []byte(body))
		if err != nil {
			t.Fatalf("writeNew: %v", err)
		}
		paths = append(paths, p)
	}
	want := []string{base + ".md", base + "-2.md", base + "-3.md"}
	for i, p := range paths {
		if filepath.Base(p) != want[i] {
			t.Errorf("write %d went to %s, want %s", i, filepath.Base(p), want[i])
		}
	}
	for i, body := range []string{"first", "second", "third"} {
		data, err := os.ReadFile(paths[i])
		if err != nil || string(data) != body {
			t.Errorf("%s holds %q (%v), want %q", paths[i], data, err, body)
		}
	}
}

// Two jobs saving the same PR at once — the --jobs 2 case — end up with two
// paths and two files, each with its own body, and List shows both with the PR
// parsed from the heading as before.
func TestSaveDoMesmoPRAoMesmoTempoGuardaOsDois(t *testing.T) {
	dir := t.TempDir()
	pr := prFor("abc123")
	bodies := []string{"E2E-FINDING-ONE", "E2E-PLAIN-REPORT"}
	paths := make([]string, len(bodies))
	var wg sync.WaitGroup
	for i, body := range bodies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := Save(dir, agent.Result{PR: pr, Agent: "lens", Body: body}, nil)
			if err != nil {
				t.Errorf("Save: %v", err)
			}
			paths[i] = p
		}()
	}
	wg.Wait()
	if paths[0] == paths[1] {
		t.Fatalf("both reviews were saved to %s", paths[0])
	}
	for i, body := range bodies {
		data, err := os.ReadFile(paths[i])
		if err != nil || !strings.Contains(string(data), body) {
			t.Errorf("%s lost its review (%v)", paths[i], err)
		}
	}
	entries, err := List(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("List: %d entries (%v), want 2", len(entries), err)
	}
	for _, e := range entries {
		if e.Repo != "acme/api-core" || e.Number != 482 {
			t.Errorf("the PR still comes from the heading: %+v", e)
		}
	}
}

// The saved file says what ran, per section in a pipeline, and it is read back
// for the Saved tab. The report's own text does not change: that goes to the PR.
func TestSaveDizOQueRodou(t *testing.T) {
	for _, c := range []struct {
		steps []agent.StepResult
		want  string
	}{
		{[]agent.StepResult{{Name: "lens", Ran: "claude · opus-5"}}, "claude · opus-5"},
		{[]agent.StepResult{{Name: "a", Ran: "claude · opus-5"}, {Name: "b", Ran: "codex"}}, "a: claude · opus-5, b: codex"},
	} {
		dir := t.TempDir()
		res := agent.Result{Agent: "fleet", Body: "## Report\n\nBody.", Steps: c.steps}
		res.PR.Repo, res.PR.Number = "acme/api", 7
		path, err := Save(dir, res, nil)
		if err != nil {
			t.Fatal(err)
		}
		entries, err := List(dir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("List: %v %v", entries, err)
		}
		if entries[0].Ran != c.want {
			t.Errorf("Ran = %q, want %q", entries[0].Ran, c.want)
		}
		data, _ := os.ReadFile(path)
		head, body, _ := strings.Cut(string(data), "\n---\n")
		if !strings.Contains(head, "- Ran on: "+c.want) || strings.Contains(body, "Ran on") {
			t.Errorf("header %q, report %q", head, body)
		}
	}
}
