package store

import (
	"os"
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
