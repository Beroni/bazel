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

// Revisado, o PR fica marcado; se ganhar commit novo depois, o marcado vira
// aviso — é o que a lista mostra.
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
	if st.Age(time.Now()) != "agora" {
		t.Errorf("idade do review: %q", st.Age(time.Now()))
	}

	// Commit novo no topo do PR.
	if got := LoadMarks(dir).Status(prFor("def456")); !got.Changed {
		t.Error("commit novo devia acusar mudança depois do review")
	}
	// Sem o commit (PR vindo de uma consulta antiga), não se inventa aviso.
	if got := LoadMarks(dir).Status(prFor("")); got.Changed {
		t.Error("sem commit para comparar, não dá para afirmar que mudou")
	}

	// Publicar anota sem apagar o resto.
	if err := MarkPosted(dir, pr.Key()); err != nil {
		t.Fatalf("MarkPosted: %v", err)
	}
	st = LoadMarks(dir).Status(pr)
	if !st.Posted || st.Agent != "review-fleet" {
		t.Errorf("publicar não podia perder o resto do registro: %+v", st)
	}
}

// Um agente que publica sozinho já deixa o review marcado como publicado.
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

// Índice ilegível é índice vazio: isto é enfeite de lista, não pode derrubar
// a listagem de PRs.
func TestLoadMarksSurvivesGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, marksFile), []byte("{isso não é json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadMarks(dir); len(got) != 0 {
		t.Errorf("esperava índice vazio, veio %v", got)
	}
	// E dá para gravar por cima sem erro.
	if err := MarkReviewed(dir, agent.Result{PR: prFor("abc"), Agent: "x"}, ""); err != nil {
		t.Fatalf("MarkReviewed sobre índice corrompido: %v", err)
	}
	if !LoadMarks(dir).Status(prFor("abc")).Reviewed {
		t.Error("o registro novo devia estar lá")
	}
}

// O índice não cresce sem limite.
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
	// Os que sobraram são os mais novos.
	if _, ok := m[gh.PR{Repo: "acme/api", Number: 0}.Key()]; ok {
		t.Error("o registro mais velho devia ter saído")
	}
	if _, ok := m[gh.PR{Repo: "acme/api", Number: maxMarks + 9}.Key()]; !ok {
		t.Error("o mais novo tinha de ficar")
	}
}
