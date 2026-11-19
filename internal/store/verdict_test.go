package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/beroni/bazel/internal/agent"
	"github.com/beroni/bazel/internal/gh"
)

const (
	headA = "a1b2c3d4e5f67890abcdef1234567890abcdef12"
	headB = "b2c3d4e5f67890abcdef1234567890abcdef1234"
)

// A decision goes in the index, is read back by whoever loads it next (a
// restarted server), changes to another, and clears.
func TestDecisaoGravaTrocaELimpa(t *testing.T) {
	dir := t.TempDir()
	if _, err := SetVerdict(dir, "Acme/Api#7", VerdictApprove, headA); err != nil {
		t.Fatal(err)
	}
	got, ok := LoadMarks(dir).VerdictOf("acme/api#7")
	if !ok || got.Verdict != VerdictApprove || got.Head != headA || got.At.IsZero() {
		t.Fatalf("after a restart: %+v %v", got, ok)
	}

	if _, err := SetVerdict(dir, "acme/api#7", VerdictChanges, headB); err != nil {
		t.Fatal(err)
	}
	got, _ = LoadMarks(dir).VerdictOf("acme/api#7")
	if got.Verdict != VerdictChanges || got.Head != headB {
		t.Errorf("changed: %+v", got)
	}

	if v, err := SetVerdict(dir, "acme/api#7", "", ""); err != nil || v != nil {
		t.Fatalf("clear: %v %v", v, err)
	}
	if _, ok := LoadMarks(dir).VerdictOf("acme/api#7"); ok {
		t.Error("a cleared decision is still there")
	}
	if len(LoadMarks(dir).Verdicts()) != 0 {
		t.Error("Verdicts lists a cleared decision")
	}
}

func TestDecisaoDesconhecidaERecusada(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"lgtm", "APPROVE", " approve"} {
		if _, err := SetVerdict(dir, "acme/api#7", v, headA); err == nil {
			t.Errorf("%q was accepted", v)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, marksFile)); !os.IsNotExist(err) {
		t.Errorf("a refused decision wrote the index: %v", err)
	}
}

// A new review of the PR rewrites its mark; the decision on it is the
// person's, and stays.
func TestNovoReviewMantemADecisao(t *testing.T) {
	dir := t.TempDir()
	pr := gh.PR{Repo: "acme/api", Number: 7, HeadRefOid: headB}
	if _, err := SetVerdict(dir, pr.Key(), VerdictComment, headA); err != nil {
		t.Fatal(err)
	}
	if err := MarkReviewed(dir, agent.Result{Agent: "fleet", PR: pr}, "r.md"); err != nil {
		t.Fatal(err)
	}
	m := LoadMarks(dir)
	if got, ok := m.VerdictOf(pr.Key()); !ok || got.Verdict != VerdictComment || got.Head != headA {
		t.Errorf("the review took the decision away: %+v %v", got, ok)
	}
	if m[markKey(pr.Key())].HeadOid != headB {
		t.Errorf("the review's head: %+v", m[markKey(pr.Key())])
	}
}

// The index is written whole, through a rename, under the lock: decisions on
// many PRs at once all land, and no half file or temporary is left behind.
func TestDecisoesAoMesmoTempoNaoSePerdem(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := SetVerdict(dir, fmt.Sprintf("acme/api#%d", i+1), VerdictApprove, headA); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := len(LoadMarks(dir).Verdicts()); n != 20 {
		t.Errorf("%d of 20 decisions on disk", n)
	}
	data, err := os.ReadFile(filepath.Join(dir, marksFile))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Error("the index is not whole JSON")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if name := e.Name(); name != marksFile && name != marksFile+".lock" {
			t.Errorf("left behind: %s", name)
		}
	}
}

func TestSlackLine(t *testing.T) {
	for verdict, want := range map[string]string{
		VerdictApprove: "https://github.com/o/r/pull/1 :white_check_mark:",
		VerdictComment: "https://github.com/o/r/pull/1 :memo::white_check_mark:",
		VerdictChanges: "https://github.com/o/r/pull/1 :git_change_request:",
	} {
		if got := SlackLine("https://github.com/o/r/pull/1", verdict); got != want {
			t.Errorf("%s: %q", verdict, got)
		}
	}
}

func TestHeadOf(t *testing.T) {
	path, err := Save(t.TempDir(), agent.Result{Agent: "fleet", PR: gh.PR{Repo: "acme/api", Number: 7, HeadRefOid: headA}, Body: "# Verdict\n\nok"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if got := HeadOf(string(data)); got != headA {
		t.Errorf("HeadOf = %q", got)
	}
	if got := HeadOf("# acme/api#7 — x\n\n- Author: @a\n\n---\n\n- Head sha: " + headB + "\n"); got != "" {
		t.Errorf("a head line in the body is not the header's: %q", got)
	}
}

// PV-2: the index keeps maxMarks records, dropping the ones touched longest
// ago. A decision just taken on an old review touches it: the next review that
// pushes the index over the cap must not take that decision away.
func TestDecisaoNovaNumaMarcaAntigaSobreviveAoCorte(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	marks := Marks{}
	for i := range maxMarks {
		marks[fmt.Sprintf("acme/api#%d", i+1)] = Mark{Agent: "fleet", At: old.Add(time.Duration(i) * time.Minute)}
	}
	data, err := json.Marshal(marks)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, marksFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	// #1 is the oldest review in the index.
	if _, err := SetVerdict(dir, "acme/api#1", VerdictApprove, headA); err != nil {
		t.Fatal(err)
	}
	if err := MarkReviewed(dir, agent.Result{Agent: "fleet", PR: gh.PR{Repo: "acme/api", Number: 9999}}, "r.md"); err != nil {
		t.Fatal(err)
	}
	m := LoadMarks(dir)
	if len(m) != maxMarks {
		t.Fatalf("%d marks, want %d", len(m), maxMarks)
	}
	if _, ok := m.VerdictOf("acme/api#1"); !ok {
		t.Error("the cut took away a decision just taken")
	}
	if _, ok := m["acme/api#2"]; ok {
		t.Error("the oldest untouched review should have gone instead")
	}
	if !m["acme/api#1"].At.Equal(old) {
		t.Errorf("deciding changed when the review was done: %v", m["acme/api#1"].At)
	}
}
