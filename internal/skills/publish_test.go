package skills_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/beroni/bazel/internal/skills"
)

// The publishing script is the part of the post skill that can do harm, so it is
// tested as a program: a report, a fake gh that keeps what it is sent, and the
// payload GitHub would have received. No model and no network.

const reviewedSha = "a1b2c3d4e5f67890abcdef1234567890abcdef12"

// fakeGH answers from files in $FAKE_DIR and records every write.
const fakeGH = `#!/bin/sh
d=$FAKE_DIR
case "$1 $2" in
"pr view")
  case "$*" in
  *headRefOid*) printf '{"headRefOid":"%s"}\n' "$(cat "$d/head")" ;;
  *) printf '{"author":{"login":"pr-author"},"url":"https://github.invalid/o/r/pull/7"}\n' ;;
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

func bazelFile(report string) string {
	return "# o/r#7 — Title\n- Author: @x\n- Head sha: " + reviewedSha + "\n- Agent: fleet\n\n---\n\n" + report
}

func header(verdict string, files int) string {
	return fmt.Sprintf("<!-- review-fleet:v1\nrepo: o/r\npr: 7\nbase: main\nhead_sha: %s\nscope_cmd: git diff origin/main...HEAD\nfiles: %d\nlenses_ran: exploit-digger\nlenses_skipped: none\nverdict: %s\n-->\n\n# Review Fleet — PR #7 · T\n\n**Verdict:** %s · x\n**Scope:** x\n**Lenses:** x\n", reviewedSha, files, verdict, verdict)
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
			if code != 3 || !strings.Contains(out, "REFUSED") || !strings.Contains(out, "paste as a comment") {
				t.Errorf("want a refusal that points at the paste, got exit %d:\n%s", code, out)
			}
			if msg := skills.PublishFailure(w.plugin); !strings.Contains(msg, "paste as a comment") {
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
		if len(p.Comments) != 0 || !strings.Contains(p.Body, "already settled on the PR (by @bob)") || !strings.Contains(p.Body, "request_changes") {
			t.Errorf("%+v", p)
		}
		if !strings.Contains(out, "already settled on the PR (by @bob)") && !strings.Contains(out, "Already settled on the PR (by @bob)") {
			t.Errorf("the summary should list it:\n%s", out)
		}
		writes, _ := os.ReadFile(filepath.Join(w.dir, "writes"))
		if strings.Contains(string(writes), "reactions") {
			t.Errorf("no 👍 on a settled thread: %s", writes)
		}
	})
	t.Run("a reply settles it, by whoever wrote it", func(t *testing.T) {
		w := newWorld(t)
		w.write("comments", "["+comment(11, "alice", "MEMBER", 2, "")+","+comment(12, "carol", "NONE", 2, `,"in_reply_to_id":11`)+"]\n")
		work := w.prepare(report)
		w.run("publish", work, "--same", "1=11")
		if p := w.payload(0); !strings.Contains(p.Body, "(by @carol)") {
			t.Errorf("%+v", p)
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
