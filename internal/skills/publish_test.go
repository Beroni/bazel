package skills_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/beroni/bazel/internal/agent"
	"github.com/beroni/bazel/internal/gh"
	"github.com/beroni/bazel/internal/skills"
	"github.com/beroni/bazel/internal/store"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"golang.org/x/net/html"
)

// The publishing script is the part of the post skill that can do harm, so it is
// tested as a program: a report, a fake gh that keeps what it is sent, and the
// payload GitHub would have received. No model and no network.

const reviewedSha = "a1b2c3d4e5f67890abcdef1234567890abcdef12"

// fakeGH answers from files in $FAKE_DIR and records every write.
const fakeGH = `#!/bin/sh
d=$FAKE_DIR
if [ -f "$d/fail" ] && echo "$*" | grep -qE "$(cat "$d/fail")"; then echo 'HTTP 500: server error' >&2; exit 1; fi
case "$1 $2" in
"pr view")
  case "$*" in
  *headRefOid*) cat "$d/head-json" 2>/dev/null || printf '{"headRefOid":"%s"}\n' "$(cat "$d/head")" ;;
  *) cat "$d/pr" 2>/dev/null || printf '{"author":{"login":"pr-author"},"url":"https://github.invalid/o/r/pull/7"}\n' ;;
  esac ;;
"pr diff") cat "$d/diff" ;;
"api user") echo me ;;
"api -X")
  echo "$*" >> "$d/writes"
  case "$*" in
  *reactions*) ;;
  *) n=$(ls "$d" | grep -c '^payload') ; cat > "$d/payload$n.json"
     if [ -f "$d/reject" ] && grep -q '"path"' "$d/payload$n.json"; then echo 'HTTP 422: line must be part of the diff' >&2; exit 1; fi ;;
  esac ;;
"api graphql") cat "$d/threads" ;;
api\ repos/*/pulls/*/comments) cat "$d/comments" ;;
api\ repos/*/issues/*/comments) cat "$d/issue" ;;
api\ repos/*/pulls/*/reviews) cat "$d/reviews" ;;
*) echo "fake gh: unsupported: $*" >&2; exit 9 ;;
esac
`

type world struct {
	t      *testing.T
	dir    string
	clone  string
	script string
	plugin string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	for _, tool := range []string{"jq", "bash", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	w := &world{t: t, dir: t.TempDir(), clone: t.TempDir()}
	plugin := t.TempDir()
	if err := skills.Materialize(plugin); err != nil {
		t.Fatal(err)
	}
	w.plugin = plugin
	if err := skills.PublishTarget(plugin, "o/r", 7); err != nil {
		t.Fatal(err)
	}
	w.script = filepath.Join(plugin, "skills", "bazel-post-report", "scripts", "publish.sh")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGH), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DIR", w.dir)
	for _, args := range []string{"init -q", "remote add origin https://github.com/o/r.git"} {
		cmd := exec.Command("git", strings.Fields(args)...)
		cmd.Dir = w.clone
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", args, err, out)
		}
	}
	w.write("head", reviewedSha+"\n")
	w.write("diff", "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,2 +1,3 @@\n one\n+two\n three\n")
	w.write("comments", "[]\n")
	w.write("issue", "[]\n")
	w.write("reviews", "[]\n")
	w.write("threads", `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]}}}}}`+"\n")
	return w
}

func (w *world) write(name, content string) {
	w.t.Helper()
	if err := os.WriteFile(filepath.Join(w.dir, name), []byte(content), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) run(args ...string) (string, int) {
	w.t.Helper()
	cmd := exec.Command("bash", append([]string{w.script}, args...)...)
	cmd.Dir = w.clone
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		w.t.Fatal(err)
	}
	return string(out), code
}

// prepare runs the first half and returns the work folder.
func (w *world) prepare(report string) string {
	w.t.Helper()
	file := filepath.Join(w.dir, "review.md")
	w.write("review.md", report)
	out, code := w.run("prepare", file)
	if code != 0 {
		w.t.Fatalf("prepare: exit %d\n%s", code, out)
	}
	var plan struct {
		Work string `json:"work_folder"`
	}
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		w.t.Fatalf("the plan is not JSON: %v\n%s", err, out)
	}
	return plan.Work
}

type payload struct {
	Event    string `json:"event"`
	Body     string `json:"body"`
	CommitID string `json:"commit_id"`
	Comments []struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Body string `json:"body"`
	} `json:"comments"`
}

func (w *world) payload(n int) payload {
	w.t.Helper()
	data, err := os.ReadFile(filepath.Join(w.dir, fmt.Sprintf("payload%d.json", n)))
	if err != nil {
		w.t.Fatalf("nothing was posted: %v", err)
	}
	// Only these keys, whatever the report tried to add.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		w.t.Fatalf("the payload is not JSON: %v\n%s", err, data)
	}
	for k := range keys {
		if k != "event" && k != "body" && k != "comments" && k != "commit_id" {
			w.t.Errorf("unexpected key %q in the payload", k)
		}
	}
	var p payload
	if err := json.Unmarshal(data, &p); err != nil {
		w.t.Fatal(err)
	}
	if p.Event != "COMMENT" {
		w.t.Errorf("event = %q, always COMMENT", p.Event)
	}
	return p
}

func (w *world) wrote() bool {
	_, err := os.Stat(filepath.Join(w.dir, "writes"))
	return err == nil
}

// bazelFile wraps a report as store.Save does. The steps are the file's own
// lower-case `## name` titles; with none, it is the one step of a single agent.
func bazelFile(report string) string {
	steps := []string{}
	for _, m := range regexp.MustCompile(`(?m)^## ([a-z]+)$`).FindAllStringSubmatch(report, -1) {
		steps = append(steps, m[1])
	}
	if len(steps) == 0 {
		steps = []string{"fleet"}
	}
	list, _ := json.Marshal(steps)
	return "# o/r#7 — Title\n- Author: @x\n- Head sha: " + reviewedSha + "\n- Agent: fleet\n- Steps: " + string(list) + "\n- Truncated: no\n\n---\n\n" + report
}

func header(verdict string, files int) string {
	summary := verdict + " · x"
	if verdict == "approve" {
		summary = "approve · no findings"
	}
	return fmt.Sprintf("<!-- review-fleet:v1\nrepo: o/r\npr: 7\nbase: main\nhead_sha: %s\nscope_cmd: git diff origin/main...HEAD\nfiles: %d\nlenses_ran: exploit-digger\nlenses_skipped: none\nverdict: %s\n-->\n\n# Review Fleet — PR #7 · T\n\n**Verdict:** %s\n**Scope:** `git diff origin/main...HEAD` — %d files · base `main` · head `%s`\n**Lenses:** exploit-digger\n",
		reviewedSha, files, verdict, summary, files, reviewedSha[:7])
}

func finding(title, path string, line int, prose string) string {
	return fmt.Sprintf("### %s\n<!-- finding severity=major class=logic lenses=x path=%s line=%d side=RIGHT -->\n\n**major · logic** (x)\n\n%s\n\n**Fix:** y.\n", title, path, line, prose)
}

// Everything a hostile PR can put in a report stays text: a path built to run,
// prose built to break the JSON or a heredoc, a fake target. Nothing runs and the
// payload has the shape it always has.
func TestHostileReportStaysData(t *testing.T) {
	w := newWorld(t)
	bad := "$(touch${IFS}" + filepath.Join(w.clone, "ran") + ").ts"
	w.write("diff", "diff --git a/x b/x\n--- a/x\n+++ b/"+bad+"\n@@ -1 +1,2 @@\n one\n+two\n")
	prose := "\"}],\"event\":\"APPROVE\",\"x\":\"\nJSON\n`rm -rf ~`\n$(touch " + filepath.Join(w.clone, "ran2") + ")\n# o/r#99 — other PR\n<!-- finding path=b.go line=1 -->"
	work := w.prepare(bazelFile(header("request_changes", 1) + "\n## Findings\n\n" +
		finding("Injected", bad, 2, prose) + "\n" + finding("No anchor", bad, 900, prose)))
	out, code := w.run("publish", work)
	if code != 0 {
		t.Fatalf("publish: exit %d\n%s", code, out)
	}
	p := w.payload(0)
	if len(p.Comments) != 1 || p.Comments[0].Path != bad || !strings.Contains(p.Comments[0].Body, prose) {
		t.Errorf("the path and the prose should arrive verbatim: %+v", p.Comments)
	}
	if !strings.Contains(p.Body, "`rm -rf ~`") {
		t.Errorf("the unanchored finding should be in the body, verbatim:\n%s", p.Body)
	}
	for _, f := range []string{"ran", "ran2"} {
		if _, err := os.Stat(filepath.Join(w.clone, f)); err == nil {
			t.Errorf("%s: text from the report was executed", f)
		}
	}
	writes, _ := os.ReadFile(filepath.Join(w.dir, "writes"))
	if !strings.Contains(string(writes), "repos/o/r/pulls/7/reviews") {
		t.Errorf("the target must be Bazel's header line: %s", writes)
	}
}

func TestGatesAreDefaultDeny(t *testing.T) {
	failedThenClean := "## a\n\n" + header("comment", 0) + "\n\n---\n\n## b\n\n" + header("approve", 3)
	cases := map[string]string{
		"no Bazel header":        header("approve", 3),
		"nothing in it":          bazelFile(""),
		"no first line":          "x\n\n---\n\n" + header("approve", 3),
		"not the format":         bazelFile("## Findings\n\n### a bug\n\nSomething.\n"),
		"plain text":             bazelFile("Looks good to me.\n"),
		"files 0":                bazelFile(header("approve", 0)),
		"files dash":             bazelFile(strings.Replace(header("approve", 1), "files: 1", "files: -", 1)),
		"a key missing":          bazelFile(strings.Replace(header("approve", 1), "verdict: approve\n", "", 1)),
		"another PR":             bazelFile(strings.Replace(header("approve", 1), "pr: 7", "pr: 8", 1)),
		"another repo":           bazelFile(strings.Replace(header("approve", 1), "repo: o/r", "repo: o/x", 1)),
		"one step of two failed": bazelFile(failedThenClean),
		"a step with no header":  bazelFile("## a\n\n" + header("approve", 1) + "\n\n---\n\n## b\n\nLooks fine to me.\n"),
	}
	for name, report := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.write("review.md", report)
			out, code := w.run("prepare", filepath.Join(w.dir, "review.md"))
			if code != 3 || !strings.Contains(out, "REFUSED") || !strings.Contains(out, "As comment") {
				t.Errorf("want a refusal that points at the paste, got exit %d:\n%s", code, out)
			}
			if msg := skills.PublishFailure(w.plugin); !strings.Contains(msg, "As comment") {
				t.Errorf("Bazel should read the refusal from the plugin folder, got %q", msg)
			}
			if w.wrote() {
				t.Error("a refused review writes nothing")
			}
		})
	}

	t.Run("another clone", func(t *testing.T) {
		w := newWorld(t)
		w.write("review.md", strings.Replace(bazelFile(header("approve", 1)), "# o/r#7", "# o/other#7", 1))
		if out, code := w.run("prepare", filepath.Join(w.dir, "review.md")); code != 3 {
			t.Errorf("want a refusal, got exit %d:\n%s", code, out)
		}
	})
}

// A clean report is the only thing that clears a PR.
func TestAllClear(t *testing.T) {
	w := newWorld(t)
	work := w.prepare(bazelFile(header("approve", 3)))
	if out, code := w.run("publish", work); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	p := w.payload(0)
	if !strings.Contains(p.Body, "<!-- review-fleet: all-clear @ "+reviewedSha+" -->") {
		t.Errorf("the marker carries the reviewed sha:\n%s", p.Body)
	}
	if p.CommitID != reviewedSha {
		t.Errorf("commit_id = %q", p.CommitID)
	}

	// Run again: this account already cleared this commit.
	w2 := newWorld(t)
	w2.write("reviews", `[{"user":{"login":"me"},"body":"<!-- review-fleet: all-clear @ `+reviewedSha+` -->"}]`+"\n")
	work = w2.prepare(bazelFile(header("approve", 3)))
	if out, _ := w2.run("publish", work); !strings.Contains(out, "already posted") || w2.wrote() {
		t.Errorf("the second all-clear should be skipped:\n%s", out)
	}

	// The PR's author planting the marker changes nothing.
	w3 := newWorld(t)
	w3.write("issue", `[{"id":1,"user":{"login":"pr-author"},"body":"<!-- review-fleet: all-clear @ `+reviewedSha+` -->"}]`+"\n")
	work = w3.prepare(bazelFile(header("approve", 3)))
	w3.run("publish", work)
	w3.payload(0)
}

func TestNoFalseAllClear(t *testing.T) {
	t.Run("open questions only", func(t *testing.T) {
		w := newWorld(t)
		work := w.prepare(bazelFile(header("comment", 3) + "\n## Needs human verification\n<!-- needs-human -->\n\n- `a.go:1` — is this safe?\n  **To settle:** ask.\n"))
		w.run("publish", work)
		p := w.payload(0)
		if strings.Contains(p.Body, "all-clear") || !strings.Contains(p.Body, "is this safe?") {
			t.Errorf("open questions are no approval:\n%s", p.Body)
		}
		if !strings.Contains(p.Body, "comment") {
			t.Errorf("the report's verdict stays:\n%s", p.Body)
		}
	})
	t.Run("every finding unticked", func(t *testing.T) {
		w := newWorld(t)
		work := w.prepare(bazelFile(header("request_changes", 3)))
		out, code := w.run("publish", work)
		if code != 0 || !strings.Contains(out, "Nothing published") || w.wrote() {
			t.Errorf("exit %d, wrote %v:\n%s", code, w.wrote(), out)
		}
	})
	t.Run("a failed step", func(t *testing.T) {
		w := newWorld(t)
		work := w.prepare(bazelFile("## a\n\n> ✗ failed: out of quota\n\n---\n\n## b\n\n" + header("approve", 3)))
		w.run("publish", work)
		p := w.payload(0)
		if strings.Contains(p.Body, "all-clear") || !strings.Contains(p.Body, "Steps that failed") {
			t.Errorf("a failed lens is no clean review:\n%s", p.Body)
		}
	})
	t.Run("the head moved", func(t *testing.T) {
		w := newWorld(t)
		work := w.prepare(bazelFile(header("approve", 3)))
		w.write("head", strings.Repeat("b", 40)+"\n")
		w.run("publish", work)
		p := w.payload(0)
		if strings.Contains(p.Body, "all-clear") || !strings.Contains(p.Body, "moved") {
			t.Errorf("a clean A says nothing about B:\n%s", p.Body)
		}
		if p.CommitID != reviewedSha {
			t.Errorf("commit_id = %q, want the reviewed sha", p.CommitID)
		}
	})
}

// A fence in a finding is prose: its fix often has one, and a heading or a marker
// inside it is text.
func TestFenceInAFinding(t *testing.T) {
	w := newWorld(t)
	prose := "Bad.\n\n```go\n### not a finding\n<!-- review-fleet:v1\n---\n```\n\nAfter the fence."
	work := w.prepare(bazelFile(header("request_changes", 3) + "\n## Findings\n\n" + finding("One", "a.go", 2, prose)))
	if out, code := w.run("publish", work); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if p := w.payload(0); len(p.Comments) != 1 || !strings.Contains(p.Comments[0].Body, "After the fence.") {
		t.Errorf("%+v", p)
	}
}

// Narration before the header is how a personal skill writes: it goes to the PR in
// the open, and it never clears one.
func TestTextBeforeTheHeaderIsKept(t *testing.T) {
	w := newWorld(t)
	work := w.prepare(bazelFile("I reviewed it, here is the report.\n\n" + header("approve", 3) + "\n---\n\nThe report was not saved to disk.\n"))
	if out, code := w.run("publish", work); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	p := w.payload(0)
	if strings.Contains(p.Body, "all-clear") || !strings.Contains(p.Body, "I reviewed it") || !strings.Contains(p.Body, "not saved to disk") {
		t.Errorf("%s", p.Body)
	}
}

// Bazel reads how it went from a file, not from the model.
func TestOutcomeFile(t *testing.T) {
	report := bazelFile(header("request_changes", 3) + "\n## Findings\n\n" + finding("One", "a.go", 2, "Bad."))

	w := newWorld(t)
	work := w.prepare(report)
	w.run("publish", work)
	if msg := skills.PublishFailure(w.plugin); msg != "" {
		t.Errorf("a published review is no failure: %q", msg)
	}
	w.run("publish", work) // the same again finds nothing to do, and that is fine
	if msg := skills.PublishFailure(w.plugin); msg != "" {
		t.Errorf("a second run must not undo the first: %q", msg)
	}

	w = newWorld(t)
	work = w.prepare(bazelFile(header("request_changes", 3)))
	w.run("publish", work)
	if msg := skills.PublishFailure(w.plugin); !strings.Contains(msg, "Nothing published") {
		t.Errorf("nothing posted must not read as published: %q", msg)
	}

}

// A step joined with others still counts, and the strongest verdict wins.
func TestSeveralSteps(t *testing.T) {
	w := newWorld(t)
	work := w.prepare(bazelFile("## a\n\n" + header("request_changes", 3) + "\n## Findings\n\n" + finding("One", "a.go", 2, "Bad.") +
		"\n\n---\n\n## b\n\n" + header("approve", 3)))
	out, code := w.run("publish", work)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	p := w.payload(0)
	if len(p.Comments) != 1 || !strings.Contains(p.Body, "request_changes") {
		t.Errorf("%+v", p)
	}
	if strings.Contains(p.Comments[0].Body, "---") {
		t.Errorf("the step separator is not part of a finding: %q", p.Comments[0].Body)
	}
}

func TestSnapshotsAnchors(t *testing.T) {
	report := bazelFile(header("request_changes", 3) + "\n## Findings\n\n" + finding("One", "a.go", 2, "Bad."))

	t.Run("anchored at the reviewed commit", func(t *testing.T) {
		w := newWorld(t)
		work := w.prepare(report)
		w.run("publish", work)
		p := w.payload(0)
		if len(p.Comments) != 1 || p.CommitID != reviewedSha {
			t.Errorf("%+v", p)
		}
	})
	t.Run("moved after prepare: no inline", func(t *testing.T) {
		w := newWorld(t)
		work := w.prepare(report)
		w.write("head", strings.Repeat("b", 40)+"\n")
		w.run("publish", work)
		p := w.payload(0)
		if len(p.Comments) != 0 || !strings.Contains(p.Body, "Bad.") {
			t.Errorf("%+v", p)
		}
	})
	t.Run("no sha in Bazel's header: no inline", func(t *testing.T) {
		w := newWorld(t)
		work := w.prepare(strings.Replace(report, "- Head sha: "+reviewedSha+"\n", "", 1))
		w.run("publish", work)
		if p := w.payload(0); len(p.Comments) != 0 {
			t.Errorf("%+v", p)
		}
	})
	t.Run("a line outside the diff goes to the body", func(t *testing.T) {
		w := newWorld(t)
		work := w.prepare(bazelFile(header("request_changes", 3) + "\n## Findings\n\n" + finding("Far", "a.go", 50, "Far away.")))
		out, _ := w.run("publish", work)
		p := w.payload(0)
		if len(p.Comments) != 0 || !strings.Contains(p.Body, "Far away.") || !strings.Contains(out, "In the body") {
			t.Errorf("%+v\n%s", p, out)
		}
	})
	t.Run("a comment GitHub refuses does not disappear", func(t *testing.T) {
		w := newWorld(t)
		w.write("reject", "")
		work := w.prepare(report)
		out, code := w.run("publish", work)
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, out)
		}
		p := w.payload(1)
		if len(p.Comments) != 0 || !strings.Contains(p.Body, "Bad.") {
			t.Errorf("the finding should be in the retry's body: %+v", p)
		}
		if !strings.Contains(out, "GitHub refused") {
			t.Errorf("the summary should say so:\n%s", out)
		}
	})
}

func comment(id int, user, assoc string, line int, inReplyTo string) string {
	return fmt.Sprintf(`{"id":%d,"user":{"login":%q},"author_association":%q,"path":"a.go","line":%d,"body":"same bug","html_url":"https://github.invalid/c%d"%s}`,
		id, user, assoc, line, id, inReplyTo)
}

func TestExistingComments(t *testing.T) {
	report := bazelFile(header("request_changes", 3) + "\n## Findings\n\n" + finding("One", "a.go", 2, "Bad."))

	t.Run("a member's comment absorbs it", func(t *testing.T) {
		w := newWorld(t)
		w.write("comments", "["+comment(11, "alice", "MEMBER", 2, "")+"]\n")
		work := w.prepare(report)
		out, code := w.run("publish", work, "--same", "1=11")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, out)
		}
		p := w.payload(0)
		if len(p.Comments) != 0 || !strings.Contains(p.Body, "Already on the PR") {
			t.Errorf("%+v", p)
		}
		writes, _ := os.ReadFile(filepath.Join(w.dir, "writes"))
		if !strings.Contains(string(writes), "pulls/comments/11/reactions") {
			t.Errorf("a 👍 on the comment: %s", writes)
		}
	})
	t.Run("the PR's author and passers-by absorb nothing", func(t *testing.T) {
		for _, c := range []string{comment(11, "pr-author", "OWNER", 2, ""), comment(12, "drive-by", "NONE", 2, "")} {
			w := newWorld(t)
			w.write("comments", "["+c+"]\n")
			work := w.prepare(report)
			if out, code := w.run("publish", work, "--same", "1="+regexp.MustCompile(`\d+`).FindString(c)); code != 3 {
				t.Errorf("not a candidate, should be refused; exit %d\n%s", code, out)
			}
			if out, _ := w.run("publish", work); !strings.Contains(out, "1 inline") {
				t.Errorf("it is posted as if the comment were not there:\n%s", out)
			}
		}
	})
	t.Run("too far away is no candidate", func(t *testing.T) {
		w := newWorld(t)
		w.write("comments", "["+comment(11, "alice", "MEMBER", 40, "")+"]\n")
		work := w.prepare(report)
		if out, code := w.run("publish", work, "--same", "1=11"); code != 3 {
			t.Errorf("exit %d\n%s", code, out)
		}
	})
	t.Run("settled: listed, still in the verdict, never silent", func(t *testing.T) {
		w := newWorld(t)
		w.write("comments", "["+comment(11, "alice", "MEMBER", 2, "")+"]\n")
		w.write("threads", `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"isResolved":true,"resolvedBy":{"login":"bob"},"comments":{"nodes":[{"databaseId":11}]}}]}}}}}`+"\n")
		work := w.prepare(report)
		out, code := w.run("publish", work, "--same", "1=11")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, out)
		}
		p := w.payload(0)
		if len(p.Comments) != 0 || !strings.Contains(p.Body, "settled by @bob") || !strings.Contains(p.Body, "request_changes") {
			t.Errorf("%+v", p)
		}
		if !strings.Contains(p.Body, "#### One") || !strings.Contains(p.Body, "```text\n**major · logic** (x)\n\nBad.") {
			t.Errorf("what was approved is still there, in case the thread goes away:\n%s", p.Body)
		}
		if !strings.Contains(out, "already settled on the PR (by @bob)") && !strings.Contains(out, "Already settled on the PR (by @bob)") {
			t.Errorf("the summary should list it:\n%s", out)
		}
		writes, _ := os.ReadFile(filepath.Join(w.dir, "writes"))
		if strings.Contains(string(writes), "reactions") {
			t.Errorf("no 👍 on a settled thread: %s", writes)
		}
	})
	t.Run("a reply settles it, from someone who speaks for the repo", func(t *testing.T) {
		w := newWorld(t)
		w.write("comments", "["+comment(11, "alice", "MEMBER", 2, "")+","+comment(12, "carol", "COLLABORATOR", 2, `,"in_reply_to_id":11`)+"]\n")
		work := w.prepare(report)
		w.run("publish", work, "--same", "1=11")
		if p := w.payload(0); !strings.Contains(p.Body, "settled by @carol") {
			t.Errorf("%+v", p)
		}
	})
	t.Run("the PR's author and passers-by settle nothing", func(t *testing.T) {
		resolved := `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"isResolved":true,"resolvedBy":{"login":"pr-author"},"comments":{"nodes":[{"databaseId":11}]}}]}}}}}` + "\n"
		for name, c := range map[string]struct{ reply, threads string }{
			"a reply by the author":  {comment(12, "pr-author", "OWNER", 2, `,"in_reply_to_id":11`), ""},
			"a reply by a passer-by": {comment(12, "drive-by", "NONE", 2, `,"in_reply_to_id":11`), ""},
			"resolved by the author": {"", resolved},
		} {
			t.Run(name, func(t *testing.T) {
				w := newWorld(t)
				list := comment(11, "alice", "MEMBER", 2, "")
				if c.reply != "" {
					list += "," + c.reply
				}
				w.write("comments", "["+list+"]\n")
				if c.threads != "" {
					w.write("threads", c.threads)
				}
				w.run("publish", w.prepare(report), "--same", "1=11")
				if p := w.payload(0); strings.Contains(p.Body, "settled by") || !strings.Contains(p.Body, "#### One") {
					t.Errorf("it stays a live finding, with its title:\n%s", p.Body)
				}
			})
		}
	})
	t.Run("the same review twice", func(t *testing.T) {
		w := newWorld(t)
		work := w.prepare(report)
		w.run("publish", work)
		p := w.payload(0)
		w.write("reviews", mustJSON(t, []map[string]any{{"user": map[string]string{"login": "me"}, "body": p.Body}})+"\n")
		w.write("writes", "")
		os.Remove(filepath.Join(w.dir, "writes"))
		work = w.prepare(report)
		if out, _ := w.run("publish", work); !strings.Contains(out, "already posted") || w.wrote() {
			t.Errorf("it should not post the same review again:\n%s", out)
		}
	})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSameTakesOnlyNumbers(t *testing.T) {
	w := newWorld(t)
	work := w.prepare(bazelFile(header("request_changes", 3)))
	for _, bad := range []string{"1=$(touch x)", "a=b", "=", "1=", "1=2=3"} {
		if out, code := w.run("publish", work, "--same", bad); code != 3 {
			t.Errorf("%q: exit %d\n%s", bad, code, out)
		}
	}
}

// Two publishes at once never share a folder.
func TestWorkFoldersAreUnique(t *testing.T) {
	w := newWorld(t)
	report := bazelFile(header("approve", 3))
	if a, b := w.prepare(report), w.prepare(report); a == b {
		t.Errorf("same folder twice: %s", a)
	}
}

// A file that agrees with itself about another PR proves nothing: the PR is the
// job's, and it comes from Bazel.
func TestTheJobNamesThePR(t *testing.T) {
	report := bazelFile(header("request_changes", 3) + "\n## Findings\n\n" + finding("One", "a.go", 2, "Bad."))
	for name, target := range map[string]string{"another PR": "o/r#8\n", "another repository": "o/x#7\n", "none": ""} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.write("review.md", report)
			path := filepath.Join(w.plugin, "publish-target")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if target != "" {
				if err := os.WriteFile(path, []byte(target), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out, code := w.run("prepare", filepath.Join(w.dir, "review.md"))
			if code != 3 || !strings.Contains(out, "not for the PR this job publishes to") {
				t.Errorf("exit %d\n%s", code, out)
			}
			if w.wrote() {
				t.Error("nothing may be written")
			}
		})
	}
}

// An all-clear rests on proof, not on the absence of findings. Each of these reads
// as clean to a parser that only looks for what is wrong.
func TestAllClearNeedsProof(t *testing.T) {
	report := header("approve", 3)
	bare := report[:strings.Index(report, "# Review Fleet")]
	swap := func(from, to string) string { return strings.Replace(report, from, to, 1) }
	saved := func(body string) string { return bazelFile(body) }

	const refused, notClear = "refused", "not clear"
	cases := map[string]struct {
		file string
		want string
	}{
		"the header alone":             {saved(bare), notClear},
		"cut after the H1":             {saved(report[:strings.Index(report, "**Verdict")]), notClear},
		"cut after the verdict":        {saved(report[:strings.Index(report, "**Scope")]), notClear},
		"cut after the scope":          {saved(report[:strings.Index(report, "**Lenses")]), notClear},
		"a title and no report":        {saved("## Critical: SQL injection in login, do not merge\n"), refused},
		"a step that is a title":       {saved("## a\n\n" + report + "\n---\n\n## b\n\n## Critical: SQL injection\n"), refused},
		"a step with nothing":          {saved("## a\n\n" + report + "\n---\n\n## b\n"), refused},
		"a step's header missing":      {strings.Replace(saved(report), `["fleet"]`, `["fleet","lens"]`, 1), refused},
		"a step never shows up":        {strings.Replace(saved("## a\n\n"+report), `["a"]`, `["a","b"]`, 1), refused},
		"two headers for a step":       {saved(report + "\n" + report), refused},
		"another verdict in the block": {saved(swap("**Verdict:** approve · no findings", "**Verdict:** this has a critical bug")), refused},
		"a second verdict line":        {saved(report + "**Verdict:** this has a critical bug\n"), notClear},
		"a second title":               {saved(report + "# Do not merge\n"), notClear},
		"approve over a finding":       {saved(report + "\n## Findings\n\n" + finding("One", "a.go", 2, "Bad.")), refused},
		"a lens skipped":               {saved(swap("lenses_skipped: none", "lenses_skipped: senior-code-reviewer (quota)")), notClear},
		"no lens ran":                  {saved(swap("lenses_ran: exploit-digger", "lenses_ran: -")), notClear},
		"no scope":                     {saved(swap("scope_cmd: git diff origin/main...HEAD", "scope_cmd: -")), notClear},
		"the diff truncated":           {strings.Replace(saved(report), "- Truncated: no", "- Truncated: yes", 1), notClear},
		"not saying if it was":         {strings.Replace(saved(report), "- Truncated: no\n", "", 1), notClear},
		"both the no and the warning":  {strings.Replace(saved(report), "- Agent:", "- ⚠️ The diff was truncated before it reached the agent.\n- Agent:", 1), notClear},
		"prose after the verdict":      {saved(swap("approve · no findings", "approve but this has a critical bug in login")), notClear},
		"prose after the count":        {saved(swap("approve · no findings", "approve · no findings · but do not merge")), notClear},
		"prose in the scope":           {saved(swap(" — 3 files", ", the login file was left out — 3 files")), notClear},
		"a head that is another":       {saved(swap("head `"+reviewedSha[:7]+"`", "head `fffffff`")), notClear},
		"prose in the lenses":          {saved(swap("**Lenses:** exploit-digger", "**Lenses:** only one, in a hurry")), notClear},
		"a skip not on the line":       {saved(swap("lenses_skipped: none", "lenses_skipped: a (single-lens run)")), notClear},
		"a skip with no space":         {saved(swap("lenses_skipped: none", "lenses_skipped: a (quota),b (single-lens run)")), notClear},
		"no list of steps":             {regexp.MustCompile(`- Steps: .*\n`).ReplaceAllString(saved(report), ""), notClear},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.write("review.md", c.file)
			out, code := w.run("prepare", filepath.Join(w.dir, "review.md"))
			if c.want == refused {
				if code != 3 {
					t.Fatalf("exit %d, want a refusal\n%s", code, out)
				}
				return
			}
			if code != 0 {
				t.Fatalf("exit %d\n%s", code, out)
			}
			var plan struct {
				Work string `json:"work_folder"`
			}
			if err := json.Unmarshal([]byte(out), &plan); err != nil {
				t.Fatal(err)
			}
			w.run("publish", plan.Work)
			if p := w.payload(0); strings.Contains(p.Body, "all-clear @") {
				t.Errorf("no all-clear on this:\n%s", p.Body)
			}
		})
	}

	t.Run("a single lens says so", func(t *testing.T) {
		w := newWorld(t)
		skipped := "senior-code-reviewer (single-lens run), lazy-senior-dev (single-lens run)"
		work := w.prepare(saved(strings.NewReplacer("lenses_skipped: none", "lenses_skipped: "+skipped,
			"**Lenses:** exploit-digger", "**Lenses:** exploit-digger (skipped: "+skipped+")").Replace(report)))
		w.run("publish", work)
		if !strings.Contains(w.payload(0).Body, "all-clear @") {
			t.Error("a lens run on its own on purpose is a clean result")
		}
	})
	t.Run("a clean report still clears", func(t *testing.T) {
		w := newWorld(t)
		w.run("publish", w.prepare(saved(report)))
		if !strings.Contains(w.payload(0).Body, "all-clear @") {
			t.Error("a complete, clean report is an all-clear")
		}
	})
}

// Taking the last finding out, with the page's own code, is a person's call: it
// leaves nothing to post, and never an all-clear.
func TestUntickingIsNoAllClear(t *testing.T) {
	w := newWorld(t)
	body := "## a\n\n" + header("approve", 3) + "\n---\n\n## b\n\n" + header("comment", 3) + "\n## Findings\n\n" + finding("One", "a.go", 2, "Bad.")
	whole := filepath.Join(w.dir, "whole.md")
	if err := os.WriteFile(whole, []byte(bazelFile(body)), 0o644); err != nil {
		t.Fatal(err)
	}
	trimmed := store.DropFindings(body, []int{0})
	path, err := store.SavePublishCopy(w.dir, whole, trimmed, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	out, code := w.run("prepare", path)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	var plan struct {
		Work string `json:"work_folder"`
	}
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatal(err)
	}
	w.run("publish", plan.Work)
	if w.wrote() {
		t.Error("nothing is left to post, and the rest is not a clean review")
	}
}

// A rule the report's own prose draws is not Bazel's separator: the findings after
// it stay findings.
func TestARuleInTheReportIsNoStep(t *testing.T) {
	w := newWorld(t)
	report := header("request_changes", 3) + "\n## Findings\n\n" + finding("One", "a.go", 2, "Bad.") + "\n---\n\n" + finding("Two", "a.go", 2, "Worse.")
	w.run("publish", w.prepare(bazelFile(report)))
	if p := w.payload(0); len(p.Comments) != 2 {
		t.Errorf("the finding after the rule was lost: %+v", p)
	}
}

// rendered is the tree a browser builds from a body, with raw HTML let through, as
// GitHub's markdown does before it sanitizes.
func rendered(t *testing.T, body string) *html.Node {
	t.Helper()
	var out bytes.Buffer
	md := goldmark.New(goldmark.WithExtensions(extension.GFM), goldmark.WithRendererOptions(gmhtml.WithUnsafe()))
	if err := md.Convert([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(&out)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func walk(n *html.Node, visit func(*html.Node)) {
	visit(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, visit)
	}
}

func textOf(n *html.Node) string {
	var b strings.Builder
	walk(n, func(c *html.Node) {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	})
	return b.String()
}

// standsAlone checks the tree, not the text: a title inside a closed <details> is
// still in the string, and nobody sees it. Every part has to be a direct child of
// the page, the only elements are the ones the script writes, and the only comment
// is its marker at the very end.
func standsAlone(t *testing.T, body string, parts map[string]string) {
	t.Helper()
	doc := rendered(t, body)
	found := map[string]bool{}
	comments := 0
	walk(doc, func(n *html.Node) {
		switch n.Type {
		case html.CommentNode:
			comments++
			if !strings.HasPrefix(n.Data, " bazel-review: ") {
				t.Errorf("a comment came from the report: %q", n.Data)
			}
		case html.ElementNode:
			if !slices.Contains([]string{"html", "head", "body", "h2", "h3", "h4", "p", "pre", "code", "strong", "em", "a", "ul", "li", "br"}, n.Data) {
				t.Errorf("<%s> came from the report", n.Data)
			}
			if want, ok := parts[strings.TrimSpace(textOf(n))]; ok && want == n.Data {
				found[textOf(n)] = true
				if n.Parent == nil || n.Parent.Data != "body" {
					t.Errorf("%q is inside <%s>", textOf(n), n.Parent.Data)
				}
			}
		}
	})
	if comments != 1 {
		t.Errorf("%d comments, and only the marker should be one", comments)
	}
	for text := range parts {
		if !found[text] {
			t.Errorf("%q did not survive:\n%s", text, body)
		}
	}
}

// On screen every part stands alone; on the PR they are one document. What one
// part opens or closes must not reach the next, whatever the report says.
func TestOnePartCannotHideTheNext(t *testing.T) {
	const prose, last = true, false // an open fence in a finding's prose swallows the next one on screen too
	hostile := map[string]struct {
		text string
		mid  bool
	}{
		"a comment":        {"<!-- quoted from the PR, never closed", prose},
		"a details":        {"<details><summary>x</summary>", prose},
		"a summary":        {"<summary>x", prose},
		"out of the quote": {"</blockquote><details><summary>x</summary>", prose},
		"out of the code":  {"</code></pre><details open><summary>x</summary>", prose},
		"out of a heading": {"</h4><details>", prose},
		"a broken tag":     {"<details", prose},
		"a broken close":   {"</blockquote", prose},
		"an image":         {"<img src=x onerror=alert(1)>", prose},
		"a raw block":      {"<style>", prose},
		"a text area":      {"<textarea>", prose},
		"a long fence":     {"`````\nclosed\n`````", prose},
		"fence open":       {"```go\nnever closed", last},
		"long fence open":  {"``````\nnever closed", last},
		"tilde fence open": {"~~~\nnever closed", last},
	}
	parts := map[string]string{
		"First": "h4", "Second": "h4", "Findings that could not be anchored": "h3", "Open questions": "h3",
		"Unverified text that came with the report": "h3", "Reviewed at " + reviewedSha[:7] + ".": "p",
	}
	for name, c := range hostile {
		t.Run(name, func(t *testing.T) {
			first, second := "Plain.", "Plain."
			if c.mid {
				first = c.text
			}
			report := header("request_changes", 3) + "\n## Findings\n\n" + finding("First", "gone.go", 2, first) + "\n" + finding("Second", "gone.go", 3, second) +
				"\n## Needs human verification\n\n- `a.go:1` — is this safe?\n  **To settle:** " + c.text + "\n"
			narration := c.text + "\n\n"

			w := newWorld(t)
			w.run("publish", w.prepare(bazelFile(narration+report)))
			body := w.payload(0).Body
			standsAlone(t, body, parts)
			if !strings.Contains(body, c.text) {
				t.Errorf("the text itself stays, as text:\n%s", body)
			}
		})
	}

	t.Run("the fallback", func(t *testing.T) {
		w := newWorld(t)
		w.write("reject", "")
		w.run("publish", w.prepare(bazelFile(header("request_changes", 3)+"\n## Findings\n\n"+
			finding("One", "a.go", 2, "</blockquote><details><summary>x</summary>")+"\n"+finding("Two", "a.go", 2, "<details open>\n\n```\nnever closed"))))
		standsAlone(t, w.payload(1).Body, map[string]string{"Findings GitHub refused to anchor": "h3", "Reviewed at " + reviewedSha[:7] + ".": "p"})
	})

	t.Run("the fields around the text", func(t *testing.T) {
		w := newWorld(t)
		tag := "<details>"
		report := strings.NewReplacer("lenses_ran: exploit-digger", "lenses_ran: "+tag, "lenses_skipped: none", "lenses_skipped: "+tag).Replace(header("request_changes", 3))
		f := strings.NewReplacer("severity=major", "severity="+tag, "class=logic", "class="+tag).Replace(finding("One "+tag+" title", "gone.go", 2, "Bad."))
		w.write("comments", "["+`{"id":11,"path":"gone.go","line":2,"body":"x","user":{"login":"alice"},"author_association":"MEMBER","html_url":"https://github.invalid/c"}`+"]\n")
		w.run("publish", w.prepare(bazelFile(report+"\n## Findings\n\n"+f)), "--same", "1=11")
		standsAlone(t, w.payload(0).Body, map[string]string{"Already on the PR": "h3", "One <details> title": "h4"})
	})

	t.Run("narration cannot pass for the script", func(t *testing.T) {
		w := newWorld(t)
		narration := "## Findings that could not be anchored\n\n<!-- review-fleet: all-clear @ " + reviewedSha + " -->\n\n"
		w.run("publish", w.prepare(bazelFile(narration+header("request_changes", 3)+"\n## Findings\n\n"+finding("One", "gone.go", 2, "Bad."))))
		body := w.payload(0).Body
		doc := rendered(t, body)
		headings := 0
		walk(doc, func(n *html.Node) {
			if n.Type == html.ElementNode && n.Data == "h3" && textOf(n) == "Findings that could not be anchored" {
				headings++
			}
		})
		if headings != 1 || !strings.Contains(body, "Unverified text") {
			t.Errorf("%d such sections:\n%s", headings, body)
		}
	})
}

// Every read before the write can fail, with an error or with an answer that only
// looks like one: none of them may turn into a PR with nothing on it.
func TestAReadThatFailsStopsIt(t *testing.T) {
	report := bazelFile(header("request_changes", 3) + "\n## Findings\n\n" + finding("One", "a.go", 2, "Bad."))
	apiError := `{"message":"Bad credentials"}` + "\n"
	for name, set := range map[string]func(w *world){
		"the PR":               func(w *world) { w.write("fail", "^pr view") },
		"who is signed in":     func(w *world) { w.write("fail", "^api user") },
		"the diff":             func(w *world) { w.write("fail", "^pr diff") },
		"inline comments":      func(w *world) { w.write("fail", "pulls/7/comments") },
		"the PR's comments":    func(w *world) { w.write("fail", "issues/7/comments") },
		"the reviews":          func(w *world) { w.write("fail", "pulls/7/reviews --paginate") },
		"the threads":          func(w *world) { w.write("fail", "^api graphql") },
		"no author":            func(w *world) { w.write("pr", `{"author":null,"url":"u"}`+"\n") },
		"an error for a list":  func(w *world) { w.write("comments", apiError) },
		"no list at all":       func(w *world) { w.write("issue", "") },
		"an error on page two": func(w *world) { w.write("reviews", "[]\n"+apiError) },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			set(w)
			w.write("review.md", report)
			out, code := w.run("prepare", filepath.Join(w.dir, "review.md"))
			if code != 1 || w.wrote() {
				t.Errorf("exit %d, wrote %v\n%s", code, w.wrote(), out)
			}
			if msg := skills.PublishFailure(w.plugin); !strings.Contains(msg, "Publishing failed") {
				t.Errorf("outcome: %q", msg)
			}
		})
	}
	for name, set := range map[string]func(w *world){
		"the head fails":        func(w *world) { w.write("fail", "headRefOid") },
		"no head":               func(w *world) { w.write("head-json", "{}\n") },
		"an empty head":         func(w *world) { w.write("head-json", `{"headRefOid":""}`+"\n") },
		"a head that is no sha": func(w *world) { w.write("head-json", `{"headRefOid":"HEAD"}`+"\n") },
		"a short head":          func(w *world) { w.write("head-json", `{"headRefOid":"abc1234"}`+"\n") },
		"GitHub is down":        func(w *world) { w.write("fail", "reviews --input") },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			work := w.prepare(report)
			set(w)
			out, code := w.run("publish", work)
			if code != 1 {
				t.Errorf("exit %d\n%s", code, out)
			}
			if msg := skills.PublishFailure(w.plugin); !strings.Contains(msg, "Publishing failed") {
				t.Errorf("outcome: %q", msg)
			}
		})
	}
}

func TestPublishTakesOnlyItsOwnPlan(t *testing.T) {
	report := bazelFile(header("request_changes", 3) + "\n## Findings\n\n" + finding("One", "a.go", 2, "Bad."))
	w := newWorld(t)
	work := w.prepare(report)

	stray := t.TempDir()
	plan, err := os.ReadFile(filepath.Join(work, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stray, "plan.json"), plan, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := w.run("publish", stray); code != 3 || w.wrote() {
		t.Errorf("a folder prepare did not make: exit %d\n%s", code, out)
	}

	w.run("publish", work)
	w.run("publish", work)
	if _, err := os.Stat(filepath.Join(w.dir, "payload1.json")); err == nil {
		t.Error("the same plan posted twice")
	}
	if msg := skills.PublishFailure(w.plugin); msg != "" {
		t.Errorf("a second run must not undo the first: %q", msg)
	}
}

func TestReportFieldsAreChecked(t *testing.T) {
	t.Run("the clone is another repository", func(t *testing.T) {
		w := newWorld(t)
		cmd := exec.Command("git", "remote", "set-url", "origin", "https://github.com/o/other.git")
		cmd.Dir = w.clone
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		w.write("review.md", bazelFile(header("approve", 3)))
		out, code := w.run("prepare", filepath.Join(w.dir, "review.md"))
		if code != 3 || !strings.Contains(out, "another repository than this clone") {
			t.Errorf("exit %d\n%s", code, out)
		}
	})
	t.Run("a sha only Bazel's header has is no proof", func(t *testing.T) {
		w := newWorld(t)
		w.run("publish", w.prepare(bazelFile(strings.Replace(header("request_changes", 3), reviewedSha, strings.Repeat("c", 40), 1)+
			"\n## Findings\n\n"+finding("One", "a.go", 2, "Bad."))))
		if p := w.payload(0); len(p.Comments) != 0 {
			t.Errorf("nothing is anchored on an unproven commit: %+v", p.Comments)
		}
	})
	t.Run("a ref is a reference, not text", func(t *testing.T) {
		w := newWorld(t)
		w.run("publish", w.prepare(bazelFile(header("request_changes", 3)+"\n## Findings\n\n"+
			strings.Replace(finding("One", "a.go", 2, "Bad."), " -->", " ref=[x](http://evil) -->", 1))))
		if p := w.payload(0); strings.Contains(p.Comments[0].Body, "evil") {
			t.Errorf("%+v", p.Comments)
		}
	})
}

// The gates that read Bazel's header, against the header Bazel writes: the link
// between the two is a string, and a change on one side would turn a gate off in
// silence.
func TestTheSavedFileDrivesTheGates(t *testing.T) {
	save := func(t *testing.T, w *world, res agent.Result) string {
		t.Helper()
		res.PR = gh.PR{Repo: "o/r", Number: 7, HeadRefOid: reviewedSha}
		path, err := store.Save(w.dir, res, nil)
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
	publish := func(t *testing.T, w *world, path string) payload {
		t.Helper()
		out, code := w.run("prepare", path)
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, out)
		}
		var plan struct {
			Work string `json:"work_folder"`
		}
		if err := json.Unmarshal([]byte(out), &plan); err != nil {
			t.Fatal(err)
		}
		w.run("publish", plan.Work)
		return w.payload(0)
	}
	clean := header("approve", 3)
	one := []agent.StepResult{{Name: "fleet", Body: clean}}

	t.Run("a clean review clears", func(t *testing.T) {
		w := newWorld(t)
		if p := publish(t, w, save(t, w, agent.Result{Body: clean, Steps: one})); !strings.Contains(p.Body, "all-clear @") {
			t.Errorf("no all-clear:\n%s", p.Body)
		}
	})
	t.Run("a truncated diff does not", func(t *testing.T) {
		w := newWorld(t)
		p := publish(t, w, save(t, w, agent.Result{Body: clean, Steps: one, Truncated: true}))
		if strings.Contains(p.Body, "all-clear @") || !strings.Contains(p.Body, "truncated") {
			t.Errorf("a truncated diff says nothing about the rest:\n%s", p.Body)
		}
	})
	t.Run("a step left out of the copy is off the list", func(t *testing.T) {
		w := newWorld(t)
		whole := "## history\n\nstory\n\n---\n\n## fleet\n\n" + clean
		path := save(t, w, agent.Result{Body: whole, Steps: []agent.StepResult{{Name: "history"}, {Name: "fleet"}}})
		kept, err := store.SavePublishCopy(w.dir, path, clean, []string{"fleet"}, false)
		if err != nil {
			t.Fatal(err)
		}
		if p := publish(t, w, kept); !strings.Contains(p.Body, "all-clear @") {
			t.Errorf("the step that went is not owed:\n%s", p.Body)
		}
	})
}

// What the skills are told to print is what the publisher takes as clean: if the
// format and the parser part ways, every all-clear stops, on every PR.
func TestGoldenReportClears(t *testing.T) {
	w := newWorld(t)
	report := strings.NewReplacer("repo: beroni/bazel", "repo: o/r", "pr: 26", "pr: 7").Replace(readGolden(t, "single-lens-clean.md"))
	sha := regexp.MustCompile(`head_sha: (\S+)`).FindStringSubmatch(report)[1]
	w.write("head", sha+"\n")
	file := strings.Replace(bazelFile(report), reviewedSha, sha, 1)
	w.write("diff", "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n one\n+two\n")
	w.run("publish", w.prepare(file))
	if p := w.payload(0); !strings.Contains(p.Body, "all-clear @") {
		t.Errorf("no all-clear:\n%s", p.Body)
	}
}

// Files that store.Save wrote before it said whether the diff was whole, exactly
// as it wrote them: neither earns an all-clear, and the one that warned says so.
func TestLegacyFilesDoNotClear(t *testing.T) {
	for name, want := range map[string]string{
		"legacy-truncated.md": "the diff was truncated before the agent read it",
		"legacy-whole.md":     "the file does not say the diff was whole",
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.run("publish", w.prepare(readGolden(t, name)))
			if p := w.payload(0); strings.Contains(p.Body, "all-clear @") || !strings.Contains(p.Body, want) {
				t.Errorf("want %q and no all-clear:\n%s", want, p.Body)
			}
		})
	}
}

// A comment on a line is a document of its own, but what a person approved has to
// stay visible in it: on GitHub a tag is live, and a <details> folds the rest away.
func TestInlineCommentHidesNothing(t *testing.T) {
	hidden := []string{"details", "summary"}
	for name, prose := range map[string]string{
		"a details":        "<details><summary>Looks harmless</summary>\n\nAPPROVED_TEXT",
		"a closing tag":    "</details>\n\nAPPROVED_TEXT",
		"a comment":        "<!-- never closed\n\nAPPROVED_TEXT",
		"a broken tag":     "<details\n\nAPPROVED_TEXT",
		"a tag in a fence": "```go\nx := Vec<u8>{}\n```\n\n<details>\n\nAPPROVED_TEXT",
		"an open fence":    "<details>\n\n```\nAPPROVED_TEXT",
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.run("publish", w.prepare(bazelFile(header("request_changes", 3)+"\n## Findings\n\n"+finding("One", "a.go", 2, prose))))
			p := w.payload(0)
			if len(p.Comments) != 1 {
				t.Fatalf("%+v", p)
			}
			body := p.Comments[0].Body
			doc := rendered(t, body)
			seen := false
			walk(doc, func(n *html.Node) {
				if n.Type == html.CommentNode {
					t.Errorf("a comment is live: %q", n.Data)
				}
				if n.Type == html.ElementNode && slices.Contains(hidden, n.Data) {
					t.Errorf("<%s> is live:\n%s", n.Data, body)
				}
				if n.Type == html.TextNode && strings.Contains(n.Data, "APPROVED_TEXT") {
					seen = true
				}
			})
			if !seen || !strings.Contains(textOf(doc), "**Fix:**") {
				t.Errorf("the text must stay, the fix with it:\n%s", body)
			}
		})
	}

	t.Run("a tag in code is not broken", func(t *testing.T) {
		w := newWorld(t)
		w.run("publish", w.prepare(bazelFile(header("request_changes", 3)+"\n## Findings\n\n"+finding("One", "a.go", 2, "Use `Vec<u8>` here."))))
		if text := textOf(rendered(t, w.payload(0).Comments[0].Body)); !strings.Contains(text, "Vec<u8>") || strings.Contains(text, "&lt;") {
			t.Errorf("the code changed: %q", text)
		}
	})
	t.Run("no tag, no change", func(t *testing.T) {
		w := newWorld(t)
		w.run("publish", w.prepare(bazelFile(header("request_changes", 3)+"\n## Findings\n\n"+finding("One", "a.go", 2, "With `x := <-ch` and a < b this is **wrong**."))))
		page := rendered(t, w.payload(0).Comments[0].Body)
		strong := false
		walk(page, func(n *html.Node) {
			if n.Type == html.ElementNode && n.Data == "pre" {
				t.Errorf("no reason for plain text here")
			}
			strong = strong || (n.Type == html.ElementNode && n.Data == "strong" && textOf(n) == "wrong")
		})
		if !strong {
			t.Error("a comment with no tag stays rendered")
		}
	})
}
