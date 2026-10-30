package store

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

const historyMD = `Commits read: 4

## What it does
Lets agents backed by Grok stream into bazel's live log exactly like Claude ones.

## Decisions
### 1. Parse grok's stream instead of wrapping the CLI output
- Commit: a1f3c9e · description
Grok already emits JSON events; reading them keeps tool calls structured.
- Rejected: Plain-text adapter (like plain.go) — loses tool calls and token counts.

### 2. GROK_HOME comes from the step env, not a global
- Commit: ` + "`7be20d4`" + ` — thread with mrc
Lets two pipelines point at different Grok accounts.
- Rejected: A single GROK_HOME in config.yaml.
- Rejected: An env var read at startup.

### Read usage from the stream, not the billing API
No extra credentials and the number is live while the job runs.

## Still open
- What happens when two concurrent jobs share the same GROK_HOME?

## Coverage
Read grok.go and env.go.
`

// The history skill's markdown becomes the History tab's sections: what it
// does, numbered decisions with sha · source, body and rejected alternatives,
// and what is still open. What the contract doesn't name stays in Rest.
func TestParseHistory(t *testing.T) {
	h := ParseHistory(historyMD)
	if h.CommitsRead != 4 || h.WhatItDoes == "" {
		t.Fatalf("header: %+v", h)
	}
	if len(h.Decisions) != 3 {
		t.Fatalf("decisions: %+v", h.Decisions)
	}
	d := h.Decisions[0]
	if d.N != 1 || d.Title != "Parse grok's stream instead of wrapping the CLI output" || d.Sha != "a1f3c9e" ||
		d.Source != "description" || d.Body != "Grok already emits JSON events; reading them keeps tool calls structured." ||
		len(d.Rejected) != 1 {
		t.Errorf("decision 1: %+v", d)
	}
	if d := h.Decisions[1]; d.Sha != "7be20d4" || d.Source != "thread with mrc" || len(d.Rejected) != 2 {
		t.Errorf("decision 2: %+v", d)
	}
	if d := h.Decisions[2]; d.N != 3 || d.Sha != "" || d.Title != "Read usage from the stream, not the billing API" {
		t.Errorf("an unnumbered decision without a commit: %+v", d)
	}
	if len(h.StillOpen) != 1 || h.StillOpen[0] != "What happens when two concurrent jobs share the same GROK_HOME?" {
		t.Errorf("still open: %v", h.StillOpen)
	}
	if h.Rest != "## Coverage\nRead grok.go and env.go." {
		t.Errorf("rest: %q", h.Rest)
	}
	// Another shape entirely: nothing parsed, everything kept.
	if h := ParseHistory("# Something else\n\ntext"); len(h.Decisions) != 0 || h.Rest == "" {
		t.Errorf("unknown shape: %+v", h)
	}
}

// A history is kept per PR and head: the current head's when there is one,
// else the newest of another head, marked stale.
func TestSaveLoadHistory(t *testing.T) {
	dir := t.TempDir()
	if _, _, found := LoadHistory(dir, "acme/api", 7, "aaa"); found {
		t.Fatal("nothing generated yet")
	}
	if _, err := SaveHistory(dir, "acme/api", 7, "aaa111", "old"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := SaveHistory(dir, "acme/api", 7, "bbb222", "new"); err != nil {
		t.Fatal(err)
	}
	if md, stale, found := LoadHistory(dir, "acme/api", 7, "aaa111"); !found || stale || md != "old" {
		t.Errorf("same head: %q stale=%v found=%v", md, stale, found)
	}
	if md, stale, found := LoadHistory(dir, "acme/api", 7, "ccc333"); !found || !stale || md != "new" {
		t.Errorf("other head: %q stale=%v found=%v, want the newest, stale", md, stale, found)
	}
	if _, err := SaveHistory(dir, "acme/api", 7, "aaa111", "regenerated"); err != nil {
		t.Fatal(err)
	}
	if md, _, _ := LoadHistory(dir, "acme/api", 7, "aaa111"); md != "regenerated" {
		t.Errorf("regenerate replaces: %q", md)
	}
	// Histories are not reviews: the Saved tab never lists them.
	if entries, _ := List(dir); len(entries) != 0 {
		t.Errorf("histories leaked into List: %v", entries)
	}
	if _, err := os.Stat(dir + "/history"); err != nil {
		t.Error(err)
	}
}

// A real bazel-history-pr output (PR #22), from before the skill settled on
// one sha per Commit line and a flat Still open list — so it doubles as the
// edge case: the header block is read and dropped, a list of shas keeps the
// first as Sha, nested still-open lines stay with their item, the prose above
// the list is its note, and the extra sections land in Rest.
func TestParseHistoryRealOutput(t *testing.T) {
	data, err := os.ReadFile("testdata/history-pr-22.md")
	if err != nil {
		t.Fatal(err)
	}
	h := ParseHistory(string(data))
	if h.Meta["head_sha"] != "6fe788fee32d4fe1d7099e1478164921e5bd83fe" || h.Meta["commits"] != "11" || h.Meta["pr"] != "22" {
		t.Errorf("meta: %v", h.Meta)
	}
	if h.CommitsRead != 11 || h.WhatItDoes == "" || len(h.Decisions) != 7 {
		t.Fatalf("commits %d, what %d chars, %d decisions", h.CommitsRead, len(h.WhatItDoes), len(h.Decisions))
	}
	if d := h.Decisions[5]; d.Sha != "4a2a946" || len(d.Shas) != 5 || d.Source != "commit message" {
		t.Errorf("a list of shas: %+v", d)
	}
	if d := h.Decisions[4]; d.Sha != "fdec568" || d.Source != "thread with @Duukaa (T1–T12)" {
		t.Errorf("thread source: %+v", d)
	}
	if !strings.HasPrefix(h.StillOpenNote, "All 12 threads were open") {
		t.Errorf("still-open note: %q", h.StillOpenNote)
	}
	if len(h.StillOpen) != 15 {
		t.Errorf("still open: %d top-level items, want 15", len(h.StillOpen))
	}
	if it := h.StillOpen[12]; !strings.Contains(it, "Manager.keep") {
		t.Errorf("nested lines must stay with their item: %q", it)
	}
	for _, bad := range []string{"<!--", "history-pr:v1", "# History — PR #22"} {
		if strings.Contains(h.Rest, bad) {
			t.Errorf("rest still has %q", bad)
		}
	}
	for _, want := range []string{"## Review ↔ fix loop", "## Timeline", "## Stats", "## Gaps"} {
		if !strings.Contains(h.Rest, want) {
			t.Errorf("rest lost %q", want)
		}
	}
}

// One PR's history never shows up as another's: owner, name and number are
// separate path segments, matched exactly (review F2).
func TestHistoryNaoVazaEntrePRs(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		repo string
		n    int
	}{{"acme/web-2", 10}, {"a-b/c", 7}} {
		if _, err := SaveHistory(dir, c.repo, c.n, "aaa111", "story of "+c.repo); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		repo string
		n    int
	}{{"acme/web", 2}, {"acme/web", 210}, {"a/b-c", 7}} {
		if md, _, found := LoadHistory(dir, c.repo, c.n, "aaa111"); found {
			t.Errorf("%s#%d got another PR's history: %q", c.repo, c.n, md)
		}
	}
	if md, stale, found := LoadHistory(dir, "ACME/Web-2", 10, "AAA111"); !found || stale || md != "story of acme/web-2" {
		t.Errorf("same PR in another case: %q stale=%v found=%v", md, stale, found)
	}
	if _, err := SaveHistory(dir, "../x", 1, "aaa", "no"); err == nil {
		t.Error("a path segment like .. must be refused")
	}
}

// The file comes from an agent, whose text a PR can steer, and a history that
// panics would break the tab on every later head too (LoadHistory falls back
// to the newest file). Shas of any case come out lower case.
func TestParseHistoryEntradasEstranhas(t *testing.T) {
	head := "## Decisions\n### 1. T\n"
	cases := []struct {
		name, line, sha string
		shas            []string
	}{
		{"upper case", "- Commit: ABCDEF1", "abcdef1", nil},
		{"a hex word", "- Commit: ADDED · x", "added", nil},
		{"mixed", "- Commit: AbC1234 — y", "abc1234", nil},
		{"several, upper case", "- Commit: `ABCD`, `EF12`", "abcd", []string{"abcd", "ef12"}},
		{"not a sha", "- Commit: nope", "", nil},
		{"empty", "- Commit:", "", nil},
		{"too short", "- Commit: abc", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := ParseHistory(head + c.line + "\n")
			if len(h.Decisions) != 1 {
				t.Fatalf("decisions: %+v", h.Decisions)
			}
			d := h.Decisions[0]
			if d.Sha != c.sha || !slices.Equal(d.Shas, c.shas) {
				t.Errorf("sha %q shas %q, want %q %q", d.Sha, d.Shas, c.sha, c.shas)
			}
		})
	}
	for _, md := range []string{
		"", "##", "## ", "## Decisions", "## Decisions\n###", "## Decisions\n### 1.", "## Decisions\n- Commit: ADDED",
		"## Still open\n  indented first", "## Still open\n- \n  x", "<!-- history-pr:v1", "<!-- history-pr:v1 -->\n",
		"Commits read: x", "\x00\xff\xfe", strings.Repeat("## Decisions\n### 1. a\n- Commit: F00D\n", 50),
	} {
		ParseHistory(md)
	}
}

func FuzzParseHistory(f *testing.F) {
	f.Add("## Decisions\n### 1. T\n- Commit: ADDED · x\n- Rejected: y\n## Still open\n- q\n  more\n")
	f.Add("<!-- history-pr:v1\ncommits: 2\n-->\n# H1\n## What it does\nx\n")
	f.Fuzz(func(t *testing.T, md string) {
		ParseHistory(md)
	})
}
