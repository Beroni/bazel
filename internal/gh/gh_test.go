package gh

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseRef(t *testing.T) {
	cases := []struct {
		in     string
		repo   string
		number int
		bad    bool
	}{
		{in: "cli/cli#14131", repo: "cli/cli", number: 14131},
		{in: "  cli/cli #14131 ", repo: "cli/cli", number: 14131},
		{in: "https://github.com/charmbracelet/bubbletea/pull/1755", repo: "charmbracelet/bubbletea", number: 1755},
		{in: "https://github.com/charmbracelet/bubbletea/pull/1755/files", repo: "charmbracelet/bubbletea", number: 1755},
		{in: "cli/cli", bad: true},
		{in: "cli/cli#abc", bad: true},
		{in: "https://github.com/cli/cli/issues/1", bad: true},
		{in: "#12", bad: true},
	}
	for _, tc := range cases {
		repo, number, err := ParseRef(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("ParseRef(%q) devia falhar, devolveu %s#%d", tc.in, repo, number)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRef(%q) falhou: %v", tc.in, err)
			continue
		}
		if repo != tc.repo || number != tc.number {
			t.Errorf("ParseRef(%q) = %s#%d, esperado %s#%d", tc.in, repo, number, tc.repo, tc.number)
		}
	}
}

func TestPRHelpers(t *testing.T) {
	pr := PR{Repo: "acme/api-core", Number: 42}
	if got := pr.Key(); got != "acme/api-core#42" {
		t.Errorf("Key() = %q", got)
	}
	if got := pr.Slug(); got != "api-core" {
		t.Errorf("Slug() = %q", got)
	}
	if got := (PR{Repo: "semdono"}).Slug(); got != "semdono" {
		t.Errorf("Slug() sem owner = %q", got)
	}
}

func TestLowerSet(t *testing.T) {
	got := lowerSet([]string{"@Maria", " joao ", ""})
	if len(got) != 2 || !got["maria"] || !got["joao"] {
		t.Errorf("lowerSet = %v", got)
	}
	if lowerSet(nil) != nil {
		t.Error("lowerSet(nil) devia ser nil para significar 'sem filtro'")
	}
}

// fakeGH puts a gh on PATH that logs its args and runs script.
func fakeGH(t *testing.T, script string) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// gh search does not know half the fields: the list is one gh pr list per repo.
func TestListPRsPorRepositorio(t *testing.T) {
	log := fakeGH(t, `echo '[{"number":1,"author":{"login":"maria"}}]'`)
	prs, errs := ListPRs(context.Background(), []string{"a/b", "c/d"}, ListOptions{})
	if len(prs) != 2 || len(errs) != 0 {
		t.Fatalf("%+v %+v", prs, errs)
	}
	data, _ := os.ReadFile(log)
	if strings.Contains(string(data), "search") || strings.Count(string(data), "pr list") != 2 {
		t.Errorf("calls:\n%s", data)
	}
	repos := map[string]bool{prs[0].Repo: true, prs[1].Repo: true}
	if !repos["a/b"] || !repos["c/d"] {
		t.Errorf("repos: %v", repos)
	}
}

// The author filter goes to gh, or the limit would cut before it.
func TestListPRsAutorVaiAoGh(t *testing.T) {
	log := fakeGH(t, `echo '[{"number":1,"author":{"login":"maria"}},{"number":2,"author":{"login":"joao"}}]'`)
	prs, _ := ListPRs(context.Background(), []string{"a/b"}, ListOptions{Authors: []string{"@Maria"}})
	if len(prs) != 1 || prs[0].Number != 1 {
		t.Fatalf("%+v", prs)
	}
	if data, _ := os.ReadFile(log); !strings.Contains(string(data), "--author maria") {
		t.Errorf("calls:\n%s", data)
	}
}

// Stopping at the limit is said, not silent, and the list stays.
func TestListPRsAvisaNoLimite(t *testing.T) {
	fakeGH(t, `echo '[{"number":1},{"number":2}]'`)
	prs, errs := ListPRs(context.Background(), []string{"a/b"}, ListOptions{Limit: 2})
	if len(prs) != 2 || len(errs) != 1 || !errs[0].Truncated || !strings.Contains(errs[0].Err.Error(), "more than 2") {
		t.Errorf("%+v %+v", prs, errs)
	}
}

// A stuck gh does not hold the caller past the timeout.
func TestGhTemTimeout(t *testing.T) {
	fakeGH(t, "exec sleep 30")
	old := ghTimeout
	ghTimeout = 100 * time.Millisecond
	t.Cleanup(func() { ghTimeout = old })
	start := time.Now()
	if _, err := run(context.Background(), "gh", "x"); err == nil {
		t.Error("expected the timeout")
	}
	if err := Comment(context.Background(), "a/b", 1, "x"); err == nil {
		t.Error("expected the timeout")
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("took %v", took)
	}
}

// A reference names a repository on GitHub and nothing else: gh --repo also
// takes a host, and it would connect there.
func TestParseRefRecusaOutroHost(t *testing.T) {
	for _, ref := range []string{
		"evil.invalid/a/b#1", "https://evil.invalid/a/b#1", "a/b/c#1", "../x#1", "a/..#1", "a/.git#1",
		"-x/y#1", "a/b#0", "a/b#-1", "https://github.com/a/b%2fc/pull/1", "a b/c#1",
	} {
		if repo, n, err := ParseRef(ref); err == nil {
			t.Errorf("%q gave %s #%d", ref, repo, n)
		}
	}
}
