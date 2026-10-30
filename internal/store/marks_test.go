package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/beroni/bazel/internal/agent"
	"github.com/beroni/bazel/internal/gh"
)

func prFor(oid string) gh.PR {
	pr := gh.PR{Repo: "acme/api-core", Number: 482, HeadRefOid: oid}
	pr.Author.Login = "maria"
	return pr
}

// Once reviewed, the PR is marked; if it gets a new commit afterwards, the
// mark turns into a warning — that is what the list shows.
func TestMarkReviewedAndChangeDetection(t *testing.T) {
	dir := t.TempDir()
	pr := prFor("abc123")

	if got := LoadMarks(dir).Status(pr); got.Reviewed {
		t.Fatal("PR nunca revisado não pode vir marcado")
	}

	res := agent.Result{PR: pr, Agent: "review-fleet", Body: "# Veredito"}
	if err := MarkReviewed(dir, res, "/tmp/acme-482.md"); err != nil {
		t.Fatalf("MarkReviewed: %v", err)
	}

	st := LoadMarks(dir).Status(pr)
	if !st.Reviewed || st.Agent != "review-fleet" || st.Changed || st.Posted {
		t.Fatalf("status logo após o review: %+v", st)
	}
	if st.Age(time.Now()) != "just now" {
		t.Errorf("idade do review: %q", st.Age(time.Now()))
	}

	// New commit at the tip of the PR.
	if got := LoadMarks(dir).Status(prFor("def456")); !got.Changed {
		t.Error("commit novo devia acusar mudança depois do review")
	}
	// Without the commit (PR from an old query), no warning is made up.
	if got := LoadMarks(dir).Status(prFor("")); got.Changed {
		t.Error("sem commit para comparar, não dá para afirmar que mudou")
	}

	// Publishing records it without erasing the rest.
	if err := MarkPosted(dir, pr.Key()); err != nil {
		t.Fatalf("MarkPosted: %v", err)
	}
	st = LoadMarks(dir).Status(pr)
	if !st.Posted || st.Agent != "review-fleet" {
		t.Errorf("publicar não podia perder o resto do registro: %+v", st)
	}
}

// An agent that publishes on its own already leaves the review marked as published.
func TestMarkReviewedFromPostingAgent(t *testing.T) {
	dir := t.TempDir()
	pr := prFor("abc123")
	res := agent.Result{PR: pr, Agent: "review-fleet-post", Posts: true}
	if err := MarkReviewed(dir, res, ""); err != nil {
		t.Fatalf("MarkReviewed: %v", err)
	}
	if !LoadMarks(dir).Status(pr).Posted {
		t.Error("agente que publica sozinho devia marcar como publicado")
	}
}

// An unreadable index is an empty index: this is list decoration, it must not
// bring down the PR listing.
func TestLoadMarksSurvivesGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, marksFile), []byte("{isso não é json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadMarks(dir); len(got) != 0 {
		t.Errorf("esperava índice vazio, veio %v", got)
	}
	// And it can be overwritten without error.
	if err := MarkReviewed(dir, agent.Result{PR: prFor("abc"), Agent: "x"}, ""); err != nil {
		t.Fatalf("MarkReviewed sobre índice corrompido: %v", err)
	}
	if !LoadMarks(dir).Status(prFor("abc")).Reviewed {
		t.Error("o registro novo devia estar lá")
	}
}

// The index does not grow without limit.
func TestMarksTrim(t *testing.T) {
	m := Marks{}
	base := time.Now().Add(-time.Hour * 10000)
	for i := range maxMarks + 10 {
		m[gh.PR{Repo: "acme/api", Number: i}.Key()] = Mark{At: base.Add(time.Duration(i) * time.Hour)}
	}
	trim(m)
	if len(m) != maxMarks {
		t.Fatalf("esperava %d registros, ficaram %d", maxMarks, len(m))
	}
	// The ones left are the newest.
	if _, ok := m[gh.PR{Repo: "acme/api", Number: 0}.Key()]; ok {
		t.Error("o registro mais velho devia ter saído")
	}
	if _, ok := m[gh.PR{Repo: "acme/api", Number: maxMarks + 9}.Key()]; !ok {
		t.Error("o mais novo tinha de ficar")
	}
}

// A saved review says which PR it came from: that is what allows publishing
// it after the server has been restarted.
func TestPRFromTitleEReviewBody(t *testing.T) {
	file := "# acme/api-core#482 — feat: rate limiting\n\n" +
		"- Author: @maria\n- Spend: 1,2k tokens\n\n---\n\n# Verdict\n\nAll good.\n"

	if got := Heading(file); got != "acme/api-core#482 — feat: rate limiting" {
		t.Fatalf("cabeçalho inesperado: %q", got)
	}
	repo, number := PRFromTitle(Heading(file))
	if repo != "acme/api-core" || number != 482 {
		t.Errorf("PR do cabeçalho: %q #%d", repo, number)
	}
	// What goes to the PR is the report, without the header Save wrote.
	if body := ReviewBody(file); body != "# Verdict\n\nAll good." {
		t.Errorf("corpo inesperado: %q", body)
	}

	// A header in another format does not turn into a made-up PR.
	for _, ruim := range []string{"", "um título qualquer", "semrepo#12", "acme/api#abc"} {
		if r, n := PRFromTitle(ruim); r != "" || n != 0 {
			t.Errorf("%q não devia virar PR: %q #%d", ruim, r, n)
		}
	}
	// A file without a separator is all report.
	if got := ReviewBody("só o texto"); got != "só o texto" {
		t.Errorf("sem separador o corpo é o arquivo: %q", got)
	}
}

// A saved review carries who wrote it. That is what lets you know, a session
// later, whether what is on disk can go to the PR — the server restarts, the
// file stays, and a history report is still not a review.
func TestSavedEntryCarregaOAgente(t *testing.T) {
	dir := t.TempDir()
	pr := prFor("abc123")

	if _, err := Save(dir, agent.Result{PR: pr, Agent: "history-pr", Body: "48 commits"}, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("esperava 1 review salvo, veio %d", len(entries))
	}
	if entries[0].Agent != "history-pr" {
		t.Errorf("o agente devia vir do cabeçalho: %q", entries[0].Agent)
	}
	if entries[0].Repo != "acme/api-core" || entries[0].Number != 482 {
		t.Errorf("o PR continua saindo do título: %+v", entries[0])
	}
}

// In a pipeline Save writes each step's time after the name. What
// identifies the choice is the name, before the dash.
func TestAgentFromLine(t *testing.T) {
	casos := map[string]string{
		"- Agent: review-fleet": "review-fleet",
		"- Agent: história e review — history-pr (2s) → review-fleet (41s)": "história e review",
		"- Agent: ": "",
	}
	for linha, want := range casos {
		if got := AgentFromLine(linha); got != want {
			t.Errorf("AgentFromLine(%q) = %q, queria %q", linha, got, want)
		}
	}
	arquivo := "# acme/api-core#482 — título\n\n- Author: @maria\n- Agent: history-pr\n\n---\n\ncorpo\n"
	if got := AgentOf(arquivo); got != "history-pr" {
		t.Errorf("AgentOf = %q", got)
	}
	if got := AgentOf("# sem agente\n\n---\n\n- Agent: tarde demais\n"); got != "" {
		t.Errorf("depois do --- é corpo, não cabeçalho: %q", got)
	}
}
