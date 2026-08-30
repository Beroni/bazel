package gh

import "testing"

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
