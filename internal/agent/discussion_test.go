package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beroni/bazel/internal/config"
	"github.com/beroni/bazel/internal/gh"
)

// discussionReview runs one Claude step whose agent keeps what it found in the
// plugin's discussion.json, with discussion as the Runner's reader.
func discussionReview(t *testing.T, discussion func(context.Context, gh.PR) ([]byte, error), publish bool) (string, []string, error) {
	t.Helper()
	hostileClone(t)
	bin, seen := t.TempDir(), filepath.Join(t.TempDir(), "seen")
	script := "#!/bin/sh\nwhile [ $# -gt 0 ]; do [ \"$1\" = --plugin-dir ] && dir=$2; shift; done\ncat >/dev/null\n" +
		"if [ -f \"$dir/discussion.json\" ]; then cat \"$dir/discussion.json\" > " + seen + "; ls -l \"$dir/discussion.json\" | cut -c1-10 >> " + seen + "; else echo none > " + seen + "; fi\n" +
		"echo '# Verdict'\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := config.Default()
	cfg.Agent.Checkout = true
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{{Name: "fleet", Command: "claude", Format: "plain", Args: []string{"-p"}, Task: "/bazel-review-fleet {{number}}"}}
	choice, err := cfg.ChoiceByName("fleet")
	if err != nil {
		t.Fatal(err)
	}
	r := New(cfg)
	r.Discussion = discussion
	var log []string
	on := func(e Event) {
		if e.Kind == EventLog {
			log = append(log, e.Text)
		}
	}
	pr := testPR()
	pr.BaseRefName = ""
	if publish {
		_, err = r.Publish(context.Background(), pr, choice, "/r/x.md", "body", on)
	} else {
		_, err = r.Review(context.Background(), pr, choice, on)
	}
	data, _ := os.ReadFile(seen)
	return string(data), log, err
}

// The fleet finds what the PR discussed in the plugin, written before it runs.
func TestReviewGanhaDiscussion(t *testing.T) {
	var asked gh.PR
	got, _, err := discussionReview(t, func(_ context.Context, pr gh.PR) ([]byte, error) {
		asked = pr
		return []byte(`{"author":{"login":"maria"},"complete":true,"threads":[],"comments":[],"reviews":[]}`), nil
	}, false)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if !strings.Contains(got, `"author":{"login":"maria"}`) {
		t.Errorf("the agent saw: %q", got)
	}
	if !strings.Contains(got, "-rw-------") {
		t.Errorf("the file is not the user's alone: %q", got)
	}
	if asked.Number != testPR().Number {
		t.Errorf("read for %+v", asked)
	}
}

// A read that fails writes no file, says so in the log, and the review goes.
func TestDiscussionQueFalhaNaoParaOReview(t *testing.T) {
	got, log, err := discussionReview(t, func(context.Context, gh.PR) ([]byte, error) {
		return nil, errors.New("HTTP 502: Bad Gateway")
	}, false)
	if err != nil {
		t.Fatalf("the review should go: %v", err)
	}
	if strings.TrimSpace(got) != "none" {
		t.Errorf("a file was written: %q", got)
	}
	if !strings.Contains(strings.Join(log, "\n"), "the PR's discussion could not be read, so the review goes without it: HTTP 502") {
		t.Errorf("log: %q", log)
	}
}

// Publishing a review that is done reads no discussion.
func TestPublicarNaoLeDiscussion(t *testing.T) {
	read := false
	got, _, _ := discussionReview(t, func(context.Context, gh.PR) ([]byte, error) {
		read = true
		return []byte(`{}`), nil
	}, true)
	if read || strings.TrimSpace(got) != "none" {
		t.Errorf("publishing read the discussion: %v %q", read, got)
	}
}
