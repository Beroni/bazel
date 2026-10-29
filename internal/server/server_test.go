package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beroni/bazel/internal/agent"
	"github.com/beroni/bazel/internal/config"
	"github.com/beroni/bazel/internal/gh"
	"github.com/beroni/bazel/internal/store"
)

func testPR(number int) gh.PR {
	pr := gh.PR{
		Repo:         "acme/api-core",
		Number:       number,
		Title:        "feat: rate limiting",
		URL:          "https://github.com/acme/api-core/pull/482",
		ChangedFiles: 2,
		HeadRefName:  "feat/rate-limit",
		BaseRefName:  "main",
		UpdatedAt:    time.Now(),
	}
	pr.Author.Login = "maria"
	return pr
}

// cfgFor devolve um config que roda cmd como agente, sem clone.
func cfgFor(t *testing.T, cmd string, args ...string) *config.Config {
	t.Helper()
	if _, err := exec.LookPath(cmd); err != nil {
		t.Skipf("%s não está no PATH", cmd)
	}
	cfg := config.Default()
	cfg.Repos = []string{"acme/api-core"}
	cfg.Agent.Command = cmd
	cfg.Agent.Args = args
	cfg.Agent.Checkout = false
	cfg.Agent.Prompt = "revise o PR {{number}} de {{repo}}"
	cfg.Agent.TimeoutSeconds = 30
	// Sem agents nomeados sobra a escolha única do bloco `agent` — que é o
	// que estes testes querem exercitar. Quem testa pipeline monta a sua.
	cfg.Agents = nil
	cfg.Pipelines = nil
	return cfg
}

// waitFor lê os eventos do hub até o job chegar num estado final.
func waitFor(t *testing.T, ch chan []byte, id string, want State) jobView {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("job %s não chegou em %s", id, want)
		case msg := <-ch:
			var ev struct {
				Type string  `json:"type"`
				Data jobView `json:"data"`
			}
			if err := json.Unmarshal(msg, &ev); err != nil || ev.Type != "job" || ev.Data.ID != id {
				continue
			}
			if ev.Data.State == want {
				return ev.Data
			}
			if ev.Data.State == StateFailed && want != StateFailed {
				t.Fatalf("job falhou: %s", ev.Data.Err)
			}
		}
	}
}

// O review roda fora da requisição que o pediu — este teste é o contrato
// inteiro: enfileirou, rodou, salvou em disco e avisou pelo hub.
func TestManagerRunsReviewAndSaves(t *testing.T) {
	dir := t.TempDir()
	// `cat` devolve o prompt recebido no stdin: dá para conferir o que o
	// agente viu sem depender de um agente de verdade.
	cfg := cfgFor(t, "cat")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hub := NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	m := NewManager(ctx, cfg, dir, 2, false, hub)
	view, err := m.Enqueue(testPR(482), false, cfg.DefaultChoice())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	done := waitFor(t, ch, view.ID, StateDone)
	if done.SavedTo == "" {
		t.Fatal("review terminou sem caminho salvo")
	}
	saved, err := os.ReadFile(done.SavedTo)
	if err != nil {
		t.Fatalf("lendo review salvo: %v", err)
	}
	if !strings.Contains(string(saved), "revise o PR 482 de acme/api-core") {
		t.Errorf("o review salvo não tem a saída do agente:\n%s", saved)
	}

	full, ok := m.View(view.ID, true)
	if !ok {
		t.Fatal("job sumiu do manager")
	}
	if !strings.Contains(full.HTML, "<p>") {
		t.Errorf("markdown não virou HTML: %q", full.HTML)
	}
}

// Uma pipeline vira um job com um passo por agente, e cada passo chega ao
// navegador com seu próprio estado — é o que a página anima.
func TestPipelineJobReportsEveryStep(t *testing.T) {
	cfg := cfgFor(t, "echo")
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agents = []config.AgentDef{
		{Name: "primeiro", Command: "echo", Args: []string{"achado do primeiro"}},
		{Name: "segundo", Command: "echo", Args: []string{"achado do segundo"}},
	}
	cfg.Pipelines = []config.Pipeline{{Name: "dupla", Steps: []string{"primeiro", "segundo"}}}
	choice, err := cfg.ChoiceByName("dupla")
	if err != nil {
		t.Fatalf("ChoiceByName: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hub := NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	m := NewManager(ctx, cfg, t.TempDir(), 1, false, hub)
	view, err := m.Enqueue(testPR(482), false, choice)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Na fila o job já anuncia o que vai rodar. O estado do primeiro passo
	// não entra na conta: o worker é livre para pegá-lo antes desta linha, e
	// exigir "queued" aqui é uma corrida com ele.
	if view.Agent != "dupla" || len(view.Steps) != 2 {
		t.Fatalf("job enfileirado devia listar os passos: %+v", view)
	}
	if view.Steps[0].Name != "primeiro" || view.Steps[1].Name != "segundo" {
		t.Fatalf("os passos deviam vir na ordem da pipeline: %+v", view.Steps)
	}

	done := waitFor(t, ch, view.ID, StateDone)
	if len(done.Steps) != 2 {
		t.Fatalf("esperava 2 passos no fim, vieram %d", len(done.Steps))
	}
	for _, st := range done.Steps {
		if st.State != StateDone {
			t.Errorf("passo %q terminou em %s: %s", st.Name, st.State, st.Err)
		}
	}

	full, _ := m.View(view.ID, true)
	if !strings.Contains(full.Body, "achado do primeiro") || !strings.Contains(full.Body, "achado do segundo") {
		t.Errorf("o relatório devia juntar os dois passos:\n%s", full.Body)
	}
}

// Publicar acontece no card do próprio review: o mesmo job volta a rodar com o
// passo do agente de post no fim, e o review que o usuário leu continua lá.
func TestPublishWithAgentRunsInTheSameJob(t *testing.T) {
	dir := t.TempDir()
	cfg := cfgFor(t, "cat")
	semClone := false
	cfg.PostAgent = config.AgentDef{
		Name: "post-report",
		Task: "/post-report {{review_file}}",
		// Publicar normalmente clona; aqui não há repositório de verdade.
		Checkout: &semClone,
		Prompt:   "{{task}}",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	m := NewManager(ctx, cfg, dir, 1, false, hub)
	review, err := m.Enqueue(testPR(482), false, cfg.DefaultChoice())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	done := waitFor(t, ch, review.ID, StateDone)
	if done.SavedTo == "" {
		t.Fatal("review sem arquivo salvo")
	}
	corpo, _ := m.View(review.ID, true)

	pub, err := m.PublishWithAgent(review.ID, nil)
	if err != nil {
		t.Fatalf("PublishWithAgent: %v", err)
	}
	if pub.ID != review.ID {
		t.Fatalf("publicar devia rodar no card do review: %s virou %s", review.ID, pub.ID)
	}
	if !pub.Publishing {
		t.Errorf("o card devia estar publicando: %+v", pub)
	}
	if pub.Agent != cfg.DefaultChoice().Name {
		t.Errorf("o card continua sendo o do review, agente %q", pub.Agent)
	}
	if len(pub.Steps) != len(done.Steps)+1 {
		t.Fatalf("esperava o passo do post depois dos %d do review, vieram %d",
			len(done.Steps), len(pub.Steps))
	}
	if last := pub.Steps[len(pub.Steps)-1]; last.Name != "post-report" {
		t.Errorf("o último passo devia ser o agente de post, é %q", last.Name)
	}

	// Pedir de novo enquanto roda não abre um segundo card nem republica.
	again, err := m.PublishWithAgent(review.ID, nil)
	if err != nil {
		t.Fatalf("PublishWithAgent (2): %v", err)
	}
	if again.ID != pub.ID || len(again.Steps) != len(pub.Steps) {
		t.Errorf("publicação duplicada: %+v", again)
	}

	end := waitFor(t, ch, review.ID, StateDone)
	if !end.Posted || end.PostErr != "" {
		t.Errorf("o card devia terminar publicado: posted=%v err=%q", end.Posted, end.PostErr)
	}
	if end.Publishing {
		t.Error("terminada a publicação, o card volta a ser o do review")
	}
	// O review continua sendo o que o card mostra — o relatório do agente de
	// post não toma o lugar dele.
	full, _ := m.View(review.ID, true)
	if full.Body != corpo.Body {
		t.Errorf("o review na tela mudou depois de publicar:\n%s", full.Body)
	}
	if full.SavedTo != done.SavedTo {
		t.Errorf("o arquivo do review devia continuar %q, virou %q", done.SavedTo, full.SavedTo)
	}
	// E o agente de post recebeu o caminho do review: o `cat` devolve o
	// prompt que leu, e ele sai no log do passo da publicação.
	log, ok := m.Log(review.ID, 0)
	if !ok {
		t.Fatal("sem log")
	}
	var viuArquivo bool
	for _, l := range log.Lines {
		if l.Step == len(pub.Steps)-1 && strings.Contains(l.Text, done.SavedTo) {
			viuArquivo = true
		}
	}
	if !viuArquivo {
		t.Errorf("o agente de post não recebeu o arquivo do review: %+v", log.Lines)
	}
	// E a publicação não gerou um segundo .md: o índice de revisados mora
	// naquele diretório, mas publicar não escreve outro review.
	files, _ := os.ReadDir(dir)
	var mds int
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".md") {
			mds++
		}
	}
	if mds != 1 {
		t.Errorf("o diretório de reviews devia ter só o review: %d markdowns", mds)
	}
}

// Terminado o review, o PR fica marcado na listagem — e ganha o aviso quando
// recebe commit novo depois.
func TestListingMarksReviewedAndChangedPRs(t *testing.T) {
	dir := t.TempDir()
	cfg := cfgFor(t, "cat")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	pr := testPR(482)
	pr.HeadRefOid = "abc123"

	m := NewManager(ctx, cfg, dir, 1, false, hub)
	view, err := m.Enqueue(pr, false, cfg.DefaultChoice())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, ch, view.ID, StateDone)

	marks := store.LoadMarks(dir)
	st := marks.Status(pr)
	if !st.Reviewed || st.Changed {
		t.Fatalf("o PR devia ficar revisado e sem mudança: %+v", st)
	}

	// Mesmo PR, commit novo no topo: mudou depois do review.
	mexido := pr
	mexido.HeadRefOid = "def456"
	if got := marks.Status(mexido); !got.Reviewed || !got.Changed {
		t.Errorf("commit novo devia acender o aviso: %+v", got)
	}

	// E o que a página recebe carrega isso.
	v := newPRView(mexido, false, time.Now()).withStatus(marks.Status(mexido), time.Now())
	if !v.Reviewed || !v.Changed {
		t.Errorf("a view do PR devia levar o histórico: %+v", v)
	}
}

// Publicar um review que não terminou, ou publicar uma publicação, é recusado.
func TestPublishWithAgentRefusesBadSources(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := cfgFor(t, "sleep", "5")
	m := NewManager(ctx, cfg, t.TempDir(), 1, false, NewHub())

	view, err := m.Enqueue(testPR(482), false, cfg.DefaultChoice())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := m.PublishWithAgent(view.ID, nil); err == nil {
		t.Error("review em andamento não devia poder ser publicado")
	}
	if _, err := m.PublishWithAgent("j999", nil); err == nil {
		t.Error("job inexistente devia dar erro")
	}
}

// O mesmo PR pedido duas vezes não pode virar dois clones.
func TestEnqueueDedupesActivePR(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := cfgFor(t, "sleep", "5")
	m := NewManager(ctx, cfg, t.TempDir(), 1, false, NewHub())
	first, err := m.Enqueue(testPR(482), false, cfg.DefaultChoice())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	second, err := m.Enqueue(testPR(482), false, cfg.DefaultChoice())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("o mesmo PR virou dois jobs: %s e %s", first.ID, second.ID)
	}

	// Com outro agente, porém, é outro review — e entra na fila.
	cfg.Agents = []config.AgentDef{{Name: "outra-lente"}}
	outra, err := cfg.ChoiceByName("outra-lente")
	if err != nil {
		t.Fatalf("ChoiceByName: %v", err)
	}
	third, err := m.Enqueue(testPR(482), false, outra)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if third.ID == first.ID {
		t.Error("o mesmo PR com outro agente devia virar um job novo")
	}
}

func TestCancelStopsRunningReview(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hub := NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	cfg := cfgFor(t, "sleep", "30")
	m := NewManager(ctx, cfg, t.TempDir(), 1, false, hub)
	view, err := m.Enqueue(testPR(482), false, cfg.DefaultChoice())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, ch, view.ID, StateRunning)

	if err := m.Cancel(view.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	got := waitFor(t, ch, view.ID, StateCanceled)
	if got.State != StateCanceled {
		t.Errorf("estado = %s, queria %s", got.State, StateCanceled)
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("BAZEL_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := New(ctx, cfgFor(t, "cat"), "beroni", Options{Addr: "127.0.0.1:0", Concurrency: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

// A porta local manda clonar repositório e rodar agente com shell. Qualquer
// página aberta em outra aba tem que bater na trave.
// O log chega ao navegador em pedaços: cada pedido traz só o que veio depois
// do `next` anterior. É isso que deixa a página acompanhar ao vivo.
func TestJobLogIsIncremental(t *testing.T) {
	cfg := cfgFor(t, "sh")
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agents = []config.AgentDef{{
		Name:    "falante",
		Command: "sh",
		Args:    []string{"-c", "echo linha1; echo linha2; echo aviso >&2"},
	}}
	cfg.Pipelines = nil
	choice, _ := cfg.ChoiceByName("falante")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	m := NewManager(ctx, cfg, t.TempDir(), 1, false, hub)
	view, err := m.Enqueue(testPR(482), false, choice)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, ch, view.ID, StateDone)

	full, ok := m.Log(view.ID, 0)
	if !ok {
		t.Fatal("job sem log")
	}
	if len(full.Lines) != 3 {
		t.Fatalf("esperava 3 linhas, vieram %d: %+v", len(full.Lines), full.Lines)
	}
	// stdout e stderr são dois pipes lidos em paralelo: entre eles não há
	// ordem garantida, mas cada um preserva a sua e o stderr entra no log.
	var out []string
	var stderr int
	for _, l := range full.Lines {
		switch l.Stream {
		case "stdout":
			out = append(out, l.Text)
		case "stderr":
			stderr++
		}
	}
	if strings.Join(out, ",") != "linha1,linha2" {
		t.Errorf("o stdout saiu fora de ordem: %v", out)
	}
	if stderr != 1 {
		t.Errorf("o stderr do agente devia entrar no log: %+v", full.Lines)
	}
	if full.Live {
		t.Error("review terminado não está mais vivo")
	}

	// Pedindo do meio, só vem o que falta — e nada além do fim.
	rest, _ := m.Log(view.ID, full.Lines[1].Seq)
	if len(rest.Lines) != 2 || rest.Lines[0].Seq != full.Lines[1].Seq {
		t.Errorf("pedido incremental trouxe %+v", rest.Lines)
	}
	if end, _ := m.Log(view.ID, full.Next); len(end.Lines) != 0 {
		t.Errorf("do fim em diante não devia vir nada: %+v", end.Lines)
	}

	// E o mesmo pelo HTTP, que é por onde a página pede.
	srv, err := New(ctx, cfg, "beroni", Options{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/jobs/"+view.ID+"/log?from=abc", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid from should give 400, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid") {
		t.Errorf("error message should be in English, got: %s", rec.Body.String())
	}
}

// A janela do log é limitada: um agente falante não pode encher a memória.
func TestLogWindowDropsOldest(t *testing.T) {
	j := &Job{}
	for i := range maxLogLines + 20 {
		j.appendLog(0, "lente", "stdout", fmt.Sprintf("linha %d", i))
	}
	if len(j.logs) != maxLogLines {
		t.Fatalf("a janela devia parar em %d, ficou com %d", maxLogLines, len(j.logs))
	}
	if j.dropped != 20 {
		t.Errorf("devia ter descartado 20 linhas, contou %d", j.dropped)
	}
	if j.logs[0].Text != "linha 20" {
		t.Errorf("a janela devia ficar com o fim, começa em %q", j.logs[0].Text)
	}
	// Linha gigante não vai inteira para a memória.
	j.appendLog(0, "lente", "stdout", strings.Repeat("x", maxLogLineRunes*2))
	if got := len([]rune(j.logs[len(j.logs)-1].Text)); got != maxLogLineRunes+1 {
		t.Errorf("linha gigante devia ser cortada, ficou com %d runas", got)
	}
}

// A página monta o seletor com o que o /api/state entrega, e um agente que não
// existe é recusado antes de virar job.
func TestStateListsAgentsAndReviewRejectsUnknown(t *testing.T) {
	cfg := cfgFor(t, "cat")
	// A lista de um config novo é vazia; esta é a que o usuário montaria na
	// página a partir das skills instaladas.
	cfg.Agents = []config.AgentDef{
		{Name: "review-fleet", Task: "/review-fleet {{number}}"},
		{Name: "exploit-digger", Task: "/exploit-digger {{number}}"},
		{Name: "lazy-senior-dev", Task: "/lazy-senior-dev {{number}}"},
	}
	cfg.Pipelines = []config.Pipeline{{
		Name:  "frota-em-série",
		Steps: []string{"review-fleet", "exploit-digger", "lazy-senior-dev"},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := New(ctx, cfg, "beroni", Options{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/state", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var st struct {
		Agents []struct {
			Name      string   `json:"name"`
			Pipeline  bool     `json:"pipeline"`
			Publisher bool     `json:"publisher"`
			Steps     []string `json:"steps"`
			Skills    []struct {
				Name      string `json:"name"`
				Installed bool   `json:"installed"`
			} `json:"skills"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decodificando /api/state: %v", err)
	}
	// Os agents e pipelines do config, mais o agente de publicação no fim.
	if len(st.Agents) != len(cfg.Agents)+len(cfg.Pipelines)+1 {
		t.Fatalf("o seletor veio com %d escolhas", len(st.Agents))
	}
	if st.Agents[0].Name != "review-fleet" {
		t.Errorf("a primeira escolha devia ser a padrão, veio %q", st.Agents[0].Name)
	}
	publisher := st.Agents[len(st.Agents)-1]
	if !publisher.Publisher || publisher.Name != "bazel-post-report" {
		t.Errorf("o último devia ser o agente de publicação, marcado como tal: %+v", publisher)
	}
	pipeline := st.Agents[len(st.Agents)-2]
	if !pipeline.Pipeline || len(pipeline.Steps) != 3 {
		t.Errorf("a pipeline devia vir com seus passos: %+v", pipeline)
	}

	// Cada agente diz qual skill ele invoca e se ela está instalada — a
	// página usa isso para avisar antes de o review falhar.
	fleet := st.Agents[0]
	if len(fleet.Skills) != 1 || fleet.Skills[0].Name != "review-fleet" {
		t.Errorf("o review-fleet devia declarar a skill que chama: %+v", fleet.Skills)
	}

	body := strings.NewReader(`{"refs":["acme/api-core#482"],"agent":"nao-existe"}`)
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/reviews", body)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("agente desconhecido devia dar 400, deu %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "review-fleet") {
		t.Errorf("o erro devia listar os agentes disponíveis: %s", rec.Body)
	}
}

func TestGuardBlocksForeignHostAndOrigin(t *testing.T) {
	h := newTestServer(t).Handler()

	tests := []struct {
		name   string
		req    *http.Request
		status int
	}{
		{"host próprio", httptest.NewRequest("GET", "http://127.0.0.1:7777/api/state", nil), http.StatusOK},
		{"host forjado", httptest.NewRequest("GET", "http://evil.com/api/state", nil), http.StatusForbidden},
		{"origem estranha", func() *http.Request {
			r := httptest.NewRequest("POST", "http://127.0.0.1:7777/api/reviews", strings.NewReader(`{"refs":["a/b#1"]}`))
			r.Header.Set("Origin", "https://evil.com")
			return r
		}(), http.StatusForbidden},
		{"origem própria", func() *http.Request {
			r := httptest.NewRequest("POST", "http://127.0.0.1:7777/api/reviews", strings.NewReader(`{"refs":[]}`))
			r.Header.Set("Origin", "http://127.0.0.1:7777")
			return r
		}(), http.StatusBadRequest}, // passa no guard, cai na validação
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, tc.req)
			if w.Code != tc.status {
				t.Errorf("status = %d, queria %d (%s)", w.Code, tc.status, w.Body.String())
			}
		})
	}
}

// O nome do review vem da URL: não pode virar leitura de arquivo qualquer.
func TestSavedReviewRejectsTraversal(t *testing.T) {
	h := newTestServer(t).Handler()
	for _, name := range []string{"../config.yaml", "..%2fconfig.yaml", "config.yaml"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:7777/api/reviews/"+name, nil))
		if w.Code == http.StatusOK {
			t.Errorf("%q foi lido — devia ter sido recusado", name)
		}
	}
}

// A descrição do PR é escrita por quem abriu o PR e cai direto no innerHTML
// da página — e essa página fala com um servidor que dispara review e comenta
// no GitHub no seu nome. Nada de script, nada de javascript:.
func TestRenderMarkdownSanitizes(t *testing.T) {
	perigos := []struct {
		name string
		src  string
		bad  string
	}{
		{"link javascript", "[clica](javascript:alert(1))", "javascript:"},
		{"script cru", "<script>alert(1)</script>", "<script"},
		{"img onerror", "<img src=x onerror=alert(1)>", "<img"},
		{"svg onload", "<svg onload=alert(1)>", "<svg"},
		{"iframe", "<iframe src=//evil.com></iframe>", "<iframe"},
		{"link data:", "[x](data:text/html,<script>alert(1)</script>)", "data:text/html"},
	}
	for _, tc := range perigos {
		t.Run(tc.name, func(t *testing.T) {
			got := renderMarkdown(tc.src)
			if strings.Contains(strings.ToLower(got), strings.ToLower(tc.bad)) {
				t.Errorf("%q sobreviveu à sanitização:\n%s", tc.bad, got)
			}
		})
	}

	// E o que é legítimo tem que continuar chegando inteiro.
	got := renderMarkdown("| a | b |\n| --- | --- |\n| 1 | 2 |\n\n- [x] feito\n\n```go\nx := 1\n```\n\n[rfc](https://example.com)")
	for _, want := range []string{"<table>", `type="checkbox"`, `class="language-go"`, `href="https://example.com"`} {
		if !strings.Contains(got, want) {
			t.Errorf("faltou %q no HTML:\n%s", want, got)
		}
	}
}

// A lista de agentes começa vazia e é montada na página, a partir das skills
// instaladas: adicionar, trocar o padrão e remover são três chamadas, e cada
// uma devolve a lista inteira já redesenhada.
func TestAgentEndpointsMontamAListaAPartirDasSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BAZEL_HOME", home) // o Save não pode encostar no ~/.bazel de verdade

	skillsDir := t.TempDir()
	for _, s := range []struct{ nome, desc string }{
		{"review-fleet", "Roda uma frota de lentes sobre o mesmo diff. Use para revisar um PR a fundo."},
		{"lazy-senior-dev", "Caça over-engineering num diff"},
	} {
		dir := filepath.Join(skillsDir, s.nome)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		md := "---\nname: " + s.nome + "\ndescription: " + s.desc + "\n---\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := config.Default()
	cfg.SkillsDir = skillsDir

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := New(ctx, cfg, "beroni", Options{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := srv.Handler()

	call := func(method, path, body string) (*httptest.ResponseRecorder, []agentePayload) {
		t.Helper()
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest(method, "http://127.0.0.1"+path, nil)
		} else {
			r = httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var out struct {
			Agents []agentePayload `json:"agents"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec, out.Agents
	}

	// Começa vazia: só o agente de publicação, que não é escolha do seletor.
	_, lista := call(http.MethodGet, "/api/agents", "")
	if len(lista) != 1 || !lista[0].Publisher {
		t.Fatalf("a lista devia começar vazia: %+v", lista)
	}

	rec, lista := call(http.MethodPost, "/api/agents", `{"skill":"review-fleet"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("adicionar devia dar 201, deu %d: %s", rec.Code, rec.Body)
	}
	if len(lista) != 2 || lista[0].Name != "review-fleet" || lista[0].Posts {
		t.Fatalf("o agente da skill devia entrar na lista: %+v", lista)
	}
	// A descrição sai do frontmatter da skill, cortada na primeira frase.
	if lista[0].Description != "Roda uma frota de lentes sobre o mesmo diff" {
		t.Errorf("descrição inesperada: %q", lista[0].Description)
	}
	if len(lista[0].Skills) != 1 || !lista[0].Skills[0].Installed {
		t.Errorf("o agente devia declarar a skill instalada que chama: %+v", lista[0].Skills)
	}

	// A mesma skill de novo é recusada; a variante que publica sozinha entra
	// com outro nome.
	if rec, _ := call(http.MethodPost, "/api/agents", `{"skill":"review-fleet"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("agente repetido devia dar 400, deu %d", rec.Code)
	}
	_, lista = call(http.MethodPost, "/api/agents", `{"skill":"review-fleet","posts":true}`)
	if len(lista) != 3 || lista[1].Name != "review-fleet-post" || !lista[1].Posts {
		t.Fatalf("a variante que publica devia entrar marcada: %+v", lista)
	}

	// Skill que não está na máquina não vira agente: isso só falharia na hora
	// de rodar, que é tarde demais.
	rec, _ = call(http.MethodPost, "/api/agents", `{"skill":"nao-instalada"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "is not installed") {
		t.Errorf("skill ausente devia dar 400 explicando: %d %s", rec.Code, rec.Body)
	}

	// Trocar o padrão é reordenar: o primeiro da lista é quem roda.
	_, lista = call(http.MethodPost, "/api/agents/review-fleet-post/default", "")
	if lista[0].Name != "review-fleet-post" {
		t.Errorf("o padrão devia ir para a frente: %+v", lista)
	}
	_, lista = call(http.MethodDelete, "/api/agents/review-fleet-post", "")
	if len(lista) != 2 || lista[0].Name != "review-fleet" {
		t.Errorf("remover devia tirar da lista: %+v", lista)
	}

	// E tudo isso está no config.yaml, que é o que sobrevive ao restart.
	yaml, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	if err != nil {
		t.Fatalf("o config devia ter sido salvo: %v", err)
	}
	if !strings.Contains(string(yaml), "/review-fleet {{number}}") {
		t.Errorf("a task do agente devia estar no arquivo:\n%s", yaml)
	}
}

// agentePayload é o agente como a página o recebe.
type agentePayload struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Posts       bool   `json:"posts"`
	Publisher   bool   `json:"publisher"`
	Skills      []struct {
		Name      string `json:"name"`
		Installed bool   `json:"installed"`
	} `json:"skills"`
}

// Sem agente nenhum não há review para enfileirar — e a página recebe o
// porquê, não um 500 seco.
func TestReviewSemAgenteConfigurado(t *testing.T) {
	t.Setenv("BAZEL_HOME", t.TempDir())
	cfg := config.Default()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := New(ctx, cfg, "beroni", Options{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/reviews",
		strings.NewReader(`{"refs":["acme/api-core#482"]}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("sem agente devia dar 400, deu %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "no agents configured") {
		t.Errorf("o erro devia dizer o que fazer: %s", rec.Body)
	}
}

// O gasto do agente chega à página: o que o evento final do stream-json diz
// vira os tokens e o custo do job, que é o que aparece no fim do review.
func TestJobViewLevaOGastoDeTokens(t *testing.T) {
	dir := t.TempDir()
	cfg := cfgFor(t, "sh")
	events := `{"type":"result","subtype":"success","result":"# Veredito\n\nOK.","duration_ms":9000,` +
		`"total_cost_usd":0.42,"usage":{"input_tokens":1200,"output_tokens":8000,` +
		`"cache_creation_input_tokens":4000,"cache_read_input_tokens":20000}}`
	// As flags de formato vão depois do script: para o `sh` são posicionais,
	// e para o Bazel são o que liga a leitura do stream.
	cfg.Agent.Args = []string{"-c", "cat <<'FIM'\n" + events + "\nFIM\n", "sh", "--output-format", "stream-json"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	m := NewManager(ctx, cfg, dir, 1, false, hub)
	view, err := m.Enqueue(testPR(482), false, cfg.DefaultChoice())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	done := waitFor(t, ch, view.ID, StateDone)
	if done.Tokens != 33200 {
		t.Errorf("o job devia somar 33200 tokens, veio %d", done.Tokens)
	}
	if done.Cost != 0.42 {
		t.Errorf("o custo devia chegar junto, veio %v", done.Cost)
	}
	// E fica no review salvo: quem abrir o arquivo depois vê o que custou.
	saved, err := os.ReadFile(done.SavedTo)
	if err != nil {
		t.Fatalf("lendo review salvo: %v", err)
	}
	if !strings.Contains(string(saved), "- Spend: 33,2k tokens") {
		t.Errorf("o review salvo devia registrar o gasto:\n%s", saved)
	}
}

// O ✕ do card tira o job da fila desta sessão — e avisa as outras abas.
func TestRemoveTiraJobDaFila(t *testing.T) {
	dir := t.TempDir()
	cfg := cfgFor(t, "cat")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	m := NewManager(ctx, cfg, dir, 1, false, hub)
	view, err := m.Enqueue(testPR(482), false, cfg.DefaultChoice())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, ch, view.ID, StateDone)

	if err := m.Remove(view.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(m.Snapshot()) != 0 {
		t.Errorf("o job devia ter saído da fila: %+v", m.Snapshot())
	}
	if _, ok := m.View(view.ID, false); ok {
		t.Error("o job removido não devia mais ser encontrado")
	}
	if err := m.Remove(view.ID); err == nil {
		t.Error("remover duas vezes devia dar erro")
	}

	// O aviso de saída chega às outras abas pelo mesmo canal dos jobs.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("não veio o aviso de job removido")
		case msg := <-ch:
			var ev struct {
				Type string            `json:"type"`
				Data map[string]string `json:"data"`
			}
			if err := json.Unmarshal(msg, &ev); err != nil || ev.Type != "job_gone" {
				continue
			}
			if ev.Data["id"] != view.ID {
				t.Errorf("aviso de outro job: %+v", ev.Data)
			}
			return
		}
	}
}

// Um review ainda vivo morre junto: card fora da tela e agente fora do ar.
func TestRemoveCancelaReviewEmAndamento(t *testing.T) {
	dir := t.TempDir()
	cfg := cfgFor(t, "sleep", "30")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	m := NewManager(ctx, cfg, dir, 1, false, hub)
	view, err := m.Enqueue(testPR(482), false, cfg.DefaultChoice())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Espera o worker pegar o job antes de tirá-lo da fila.
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, ok := m.View(view.ID, false)
		if ok && v.State == StateRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("o job não começou a rodar")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := m.Remove(view.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := m.View(view.ID, false); ok {
		t.Error("o job devia ter saído da fila")
	}
	// O `sleep 30` foi morto junto: se não tivesse sido, o Manager ainda
	// estaria ocupado e o próximo review ficaria esperando.
	segundo, err := m.Enqueue(testPR(999), false, cfg.DefaultChoice())
	if err != nil {
		t.Fatalf("Enqueue do segundo: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		v, _ := m.View(segundo.ID, false)
		if v.State == StateRunning {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("o worker não ficou livre depois da remoção")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Fechar o Bazel antes de publicar não pode custar o review: ele está em
// disco, e daí ainda dá para mandá-lo ao PR — sem job de origem nenhum, que é
// justamente o caso de quem reiniciou o servidor.
func TestPublishSavedRodaSemJobDeOrigem(t *testing.T) {
	dir := t.TempDir()
	cfg := cfgFor(t, "cat")
	// O agente de publicação também é o `cat` daqui: o que se testa é o
	// caminho do arquivo até a fila, não o que o post-report faria. Sem
	// clone: publicar de verdade clona, mas o repositório daqui é fictício.
	semClone := false
	cfg.PostAgent = config.AgentDef{Name: "post", Task: "publish {{review_file}}", Posts: true, Checkout: &semClone}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	m := NewManager(ctx, cfg, dir, 1, false, hub)
	path := filepath.Join(dir, "acme-api-core-482-20260830-120000.md")
	if err := os.WriteFile(path, []byte("# acme/api-core#482 — feat\n\n---\n\n# Verdict\n\nAll good.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	view, err := m.PublishSaved(testPR(482), false, path, "# Verdict\n\nAll good.", nil)
	if err != nil {
		t.Fatalf("PublishSaved: %v", err)
	}
	if !view.Publishing || view.Agent != "post" {
		t.Fatalf("devia virar um job de publicação: %+v", view)
	}

	// Pedir de novo enquanto o primeiro roda não abre um segundo: publicar
	// duas vezes o mesmo arquivo é comentar duas vezes no PR.
	again, err := m.PublishSaved(testPR(482), false, path, "# Verdict\n\nAll good.", nil)
	if err != nil {
		t.Fatalf("PublishSaved de novo: %v", err)
	}
	if again.ID != view.ID && again.State != StateDone {
		t.Errorf("esperava o mesmo job em curso, veio %+v", again)
	}

	// Uma publicação não gera outro arquivo: o review já está salvo, e este
	// job só levou o que estava nele ao PR.
	done := waitFor(t, ch, view.ID, StateDone)
	if done.SavedTo != "" {
		t.Errorf("uma publicação não devia salvar outro review: %q", done.SavedTo)
	}
	if _, err := m.PublishSaved(testPR(482), false, "", "corpo", nil); err == nil {
		t.Error("sem arquivo não há o que publicar")
	}
}

// Cada achado do review sai numa <section> numerada: é nela que a página põe
// a caixa de marcar, e o número é o que o publish recebe em `skip`.
func TestRenderReviewEmbrulhaOsAchados(t *testing.T) {
	src := "# Report\n\n## Findings\n\n### One\n\ntext <script>x</script>\n\n### Two\n\nmore\n\n## Coverage\n\n- ok\n"
	got := renderReview(src)
	for _, want := range []string{`<section class="finding" data-finding="0"><h3>One</h3>`, `<section class="finding" data-finding="1"><h3>Two</h3>`, "<h2>Coverage</h2>"} {
		if !strings.Contains(got, want) {
			t.Errorf("faltou %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "<section") != 2 || strings.Count(got, "</section>") != 2 {
		t.Errorf("esperava duas sections:\n%s", got)
	}
	if strings.Contains(got, "<script") {
		t.Error("o embrulho não pode pular a sanitização")
	}
	if plain := renderReview("just **text**"); plain != renderMarkdown("just **text**") {
		t.Errorf("sem achado, é o render normal: %s", plain)
	}
}

// Desmarcar um achado na tela tira só ele do que o agente de post recebe: o
// arquivo salvo continua inteiro e a cópia cortada vai em publish/.
func TestPublishWithAgentDeixaDeForaOsDesmarcados(t *testing.T) {
	dir := t.TempDir()
	// O `cat` devolve o prompt: o do review é o relatório com dois achados,
	// e o do post é a task — que traz o caminho do arquivo que ele recebeu.
	cfg := cfgFor(t, "cat")
	cfg.Agent.Prompt = "## Findings\n\n### First\n\nreal bug\n\n### Second\n\nfalse positive\n"
	semClone := false
	cfg.PostAgent = agenteQueLeACopia()
	cfg.PostAgent.Posts, cfg.PostAgent.Checkout = true, &semClone

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	m := NewManager(ctx, cfg, dir, 1, false, hub)
	review, err := m.Enqueue(testPR(482), false, cfg.DefaultChoice())
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	done := waitFor(t, ch, review.ID, StateDone)
	full, _ := m.View(review.ID, true)
	if strings.Count(full.HTML, `class="finding"`) != 2 {
		t.Fatalf("esperava dois achados embrulhados:\n%s", full.HTML)
	}

	if _, err := m.PublishWithAgent(review.ID, []int{1}); err != nil {
		t.Fatalf("PublishWithAgent: %v", err)
	}
	end := waitFor(t, ch, review.ID, StateDone)
	if !end.Posted {
		t.Fatalf("devia ter publicado: %+v", end)
	}
	// The agent got the copy, not the original, and read only the ticked finding in it.
	log, _ := m.Log(review.ID, 0)
	var lido strings.Builder
	var caminho string
	for _, l := range log.Lines {
		if caminho == "" && strings.Contains(l.Text, filepath.Join(dir, "publish")) {
			caminho = l.Text
			continue
		}
		// Only what comes after the path line was printed by the post agent.
		if caminho != "" {
			lido.WriteString(l.Text + "\n")
		}
	}
	if !strings.HasPrefix(filepath.Base(caminho), strings.TrimSuffix(filepath.Base(done.SavedTo), ".md")+"-") || caminho == done.SavedTo {
		t.Errorf("o agente de post devia receber uma cópia em publish/: %q", caminho)
	}
	if strings.Contains(lido.String(), "false positive") || !strings.Contains(lido.String(), "real bug") {
		t.Errorf("a cópia devia ter só o achado marcado:\n%s", lido.String())
	}
	orig, _ := os.ReadFile(done.SavedTo)
	if !strings.Contains(string(orig), "false positive") {
		t.Error("o review salvo não devia perder o achado desmarcado")
	}
	// E o review na tela continua inteiro.
	after, _ := m.View(review.ID, true)
	if after.Body != full.Body {
		t.Error("o corpo do review na tela mudou")
	}
}

// O corpo do pedido de publicação é opcional: vazio é "publica tudo", e um
// índice negativo é recusado antes de chegar à fila.
func TestReadSkipAceitaCorpoVazio(t *testing.T) {
	for _, tc := range []struct {
		body string
		want []int
		bad  bool
	}{
		{"", nil, false},
		{`{}`, nil, false},
		{`{"skip":[0,2]}`, []int{0, 2}, false},
		{`{"skip":[-1]}`, nil, true},
		{`nope`, nil, true},
	} {
		r := httptest.NewRequest("POST", "/x", strings.NewReader(tc.body))
		got, err := readSkip(httptest.NewRecorder(), r)
		if tc.bad {
			if err == nil {
				t.Errorf("%q devia falhar", tc.body)
			}
			continue
		}
		if err != nil || fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%q: got %v, %v; queria %v", tc.body, got, err, tc.want)
		}
	}
}

// Agente cuja saída não é review não tem caminho para o PR: nem o agente de
// post, nem o comentário colado. A página esconde os botões; isto é o que
// segura um pedido vindo de fora dela.
func TestPublicarRecusaSaidaQueNaoEReview(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	naoPublica := false
	cfg := cfgFor(t, "echo")
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agents = []config.AgentDef{
		{Name: "history-pr", Command: "echo", Args: []string{"48 commits, 3 rodadas"}, Publishable: &naoPublica},
	}
	choice, err := cfg.ChoiceByName("history-pr")
	if err != nil {
		t.Fatalf("ChoiceByName: %v", err)
	}

	hub := NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	m := NewManager(ctx, cfg, t.TempDir(), 1, false, hub)
	view, err := m.Enqueue(testPR(482), false, choice)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	done := waitFor(t, ch, view.ID, StateDone)
	if done.Publishable {
		t.Error("o job devia dizer à página que isto não vai ao PR")
	}
	if done.SavedTo == "" {
		t.Error("não publicar não quer dizer não salvar: o relatório fica em disco")
	}

	if _, err := m.PublishWithAgent(view.ID, nil); err == nil {
		t.Error("o agente de post não devia aceitar o que não é review")
	}
	if _, err := m.Post(context.Background(), view.ID, nil); err == nil {
		t.Error("colar como comentário é o mesmo destino por outra porta")
	}
}

// O painel monta a pipeline pela API: cria, aparece no seletor, e sai por uma
// rota própria — pipeline está em `pipelines:`, agente está em `agents:`.
func TestPipelineEndpoints(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	post := func(t *testing.T, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for _, skill := range []string{"history-pr", "review-fleet"} {
		if rec := post(t, "/api/agents", `{"skill":"`+skill+`"}`); rec.Code != http.StatusCreated {
			t.Skipf("a skill %s não está instalada nesta máquina: %s", skill, rec.Body)
		}
	}

	rec := post(t, "/api/pipelines", `{"name":"history then review","steps":["history-pr","review-fleet"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("criando a pipeline: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Agents []struct {
			Name     string   `json:"name"`
			Pipeline bool     `json:"pipeline"`
			Steps    []string `json:"steps"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decodificando: %v", err)
	}
	var achou bool
	for _, a := range out.Agents {
		if a.Name == "history then review" {
			achou = true
			if !a.Pipeline || len(a.Steps) != 2 || a.Steps[0] != "history-pr" {
				t.Errorf("a pipeline devia vir com seus passos na ordem: %+v", a)
			}
		}
	}
	if !achou {
		t.Fatalf("a pipeline criada devia estar no seletor: %+v", out.Agents)
	}

	// Passo que não é agente é recusado com o motivo, não com um 500.
	if rec := post(t, "/api/pipelines", `{"name":"x","steps":["nao-existe"]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("passo desconhecido devia dar 400, veio %d: %s", rec.Code, rec.Body)
	}

	req := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1/api/pipelines/history%20then%20review", nil)
	del := httptest.NewRecorder()
	h.ServeHTTP(del, req)
	if del.Code != http.StatusOK {
		t.Fatalf("removendo a pipeline: %d %s", del.Code, del.Body)
	}
	if strings.Contains(del.Body.String(), "history then review") {
		t.Errorf("a pipeline removida não devia voltar na lista: %s", del.Body)
	}
}

// O config.yaml é o que se leva para outra máquina: sai como arquivo, com o
// nome certo, e não como texto para copiar da tela.
func TestConfigDownload(t *testing.T) {
	srv := newTestServer(t)
	// O download é o arquivo em disco; sem ele gravado não há o que baixar.
	if err := srv.cfg.Save(); err != nil {
		t.Fatalf("gravando o config: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/config/file", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("baixando o config: %d %s", rec.Code, rec.Body)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, `filename="config.yaml"`) {
		t.Errorf("devia vir como anexo com nome: %q", cd)
	}
	if !strings.Contains(rec.Body.String(), "agent:") {
		t.Errorf("o corpo devia ser o yaml do disco: %q", rec.Body.String())
	}
}

// A pipeline que termina em `publish` não publica sozinha: salva o review e
// para mostrando só o que vai ao PR — o passo que não é review fica à parte —
// para você escolher os achados e mandar.
func TestPipelineEsperaParaPublicar(t *testing.T) {
	cfg := cfgFor(t, "echo")
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.Checkout = false
	nao := false
	cfg.Agents = []config.AgentDef{
		{Name: "historia", Command: "echo", Args: []string{"a história do PR"}, Publishable: &nao},
		{Name: "frota", Command: "echo", Args: []string{"o review da frota"}},
	}
	cfg.Pipelines = []config.Pipeline{{Name: "ler antes", Steps: []string{"historia", "frota", "publish"}}}
	choice, err := cfg.ChoiceByName("ler antes")
	if err != nil {
		t.Fatalf("ChoiceByName: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	m := NewManager(ctx, cfg, t.TempDir(), 1, false, hub)
	view, err := m.Enqueue(testPR(482), false, choice)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	fim := waitFor(t, ch, view.ID, StateDone)
	if !fim.AwaitsPublish {
		t.Error("o job devia dizer à página que espera você publicar")
	}
	if fim.Posted || fim.Publishing {
		t.Error("o passo publish não pode publicar sem você mandar")
	}
	if len(fim.Steps) != 2 {
		t.Errorf("o agente de publicação não podia ter entrado no card: %+v", fim.Steps)
	}
	cheio, _ := m.View(view.ID, true)
	if cheio.SavedTo == "" {
		t.Error("o review devia estar salvo — é ele que vai ao PR")
	}
	if !strings.Contains(cheio.PublishHTML, "o review da frota") || strings.Contains(cheio.PublishHTML, "a história do PR") {
		t.Errorf("o que vai ao PR é só o review:\n%s", cheio.PublishHTML)
	}
	if !strings.Contains(cheio.ContextHTML, "a história do PR") {
		t.Errorf("a história fica à parte, no Bazel:\n%s", cheio.ContextHTML)
	}
}

// O agente às vezes devolve o relatório colado num bloco ```markdown. Dentro
// dele nada renderiza — a tela mostra o markdown cru em vez do review — e por
// isso o bloco sai antes de qualquer outra coisa: na tela, no disco e no que
// vai ao PR.
func TestRelatorioSaiDoBlocoDeCodigo(t *testing.T) {
	const relatorio = "```markdown\n# Review Fleet — PR #482\n\n**Verdict:** comment\n\n## Findings\n\n### Token stored client-side\n\nThe refresh token is written to localStorage.\n```"

	cfg := cfgFor(t, "echo")
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.Checkout = false
	cfg.Agents = []config.AgentDef{
		{Name: "frota", Command: "printf", Args: []string{"%s", relatorio}},
		{Name: "depois", Command: "echo", Args: []string{"segundo passo"}},
	}
	cfg.Pipelines = []config.Pipeline{{Name: "dupla", Steps: []string{"frota", "depois"}}}
	choice, err := cfg.ChoiceByName("dupla")
	if err != nil {
		t.Fatalf("ChoiceByName: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	dir := t.TempDir()
	m := NewManager(ctx, cfg, dir, 1, false, hub)
	view, err := m.Enqueue(testPR(482), false, choice)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	waitFor(t, ch, view.ID, StateDone)
	fim, _ := m.View(view.ID, true)
	if strings.Contains(fim.Body, "```markdown") {
		t.Errorf("a cerca voltou no relatório final:\n%s", fim.Body)
	}
	if !strings.Contains(fim.HTML, "<h1>") || !strings.Contains(fim.Body, "segundo passo") {
		t.Errorf("o relatório final devia ter os dois passos, renderizados:\n%s", fim.Body)
	}
}

// Fila cheia devolve erro e marca o job como falho — sem travar o Manager.
// Antes, Enqueue chamava finish segurando o mutex que finish também trava.
func TestEnqueueComFilaCheiaNaoTrava(t *testing.T) {
	cfg := cfgFor(t, "cat")
	m := &Manager{
		cfg:   cfg,
		hub:   NewHub(),
		ctx:   context.Background(),
		queue: make(chan queued), // sem worker e sem buffer: sempre cheia
		jobs:  map[string]*Job{},
	}

	type result struct {
		v   jobView
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := m.Enqueue(testPR(482), false, cfg.DefaultChoice())
		done <- result{v, err}
	}()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("fila cheia devia devolver erro")
		}
		if r.v.State != StateFailed {
			t.Errorf("job devia sair como failed, saiu %q", r.v.State)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Enqueue travou com a fila cheia")
	}

	// O Manager continua respondendo depois.
	if _, ok := m.View("j1", false); !ok {
		t.Error("job da fila cheia sumiu")
	}
}

// Com a fila cheia, ninguém pode ver o job como queued antes de ele falhar:
// um Enqueue concorrente do mesmo PR que o achasse nessa janela receberia
// sucesso e um job que nunca entrou na fila.
func TestEnqueueComFilaCheiaNaoDevolveJobFantasma(t *testing.T) {
	cfg := cfgFor(t, "cat")
	m := &Manager{
		cfg:   cfg,
		hub:   NewHub(),
		ctx:   context.Background(),
		queue: make(chan queued), // sem worker e sem buffer: sempre cheia
		jobs:  map[string]*Job{},
	}

	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := m.Enqueue(testPR(482), false, cfg.DefaultChoice())
			if err == nil {
				errs <- fmt.Errorf("Enqueue com a fila cheia devolveu sucesso (job %s, %s)", v.ID, v.State)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		if j.State != StateFailed {
			t.Errorf("job %s ficou %q com a fila cheia", j.ID, j.State)
		}
	}
}

// Exposed avisa de qualquer escuta além do loopback — inclusive `:7777`, que
// abre todas as interfaces mesmo com a checagem de Host ligada.
func TestExposedDetectsNonLoopbackListen(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:7777":    false,
		"localhost:7777":    false,
		"[::1]:7777":        false,
		":7777":             true,
		"0.0.0.0:7777":      true,
		"[::]:7777":         true,
		"192.168.0.10:7777": true,
	} {
		s := &Server{opts: Options{Addr: addr}}
		if got := s.Exposed(); got != want {
			t.Errorf("Exposed(%q) = %v, queria %v", addr, got, want)
		}
	}
}

// agenteQueLeACopia is a post agent that prints the path it got and the file's
// content: the copy in publish/ goes away when the publication ends, so what the
// agent read can only be checked in the log.
func agenteQueLeACopia() config.AgentDef {
	return config.AgentDef{
		Name: "post", Command: "sh", Args: []string{"-c", `read cmd file; echo "$file"; cat "$file"`},
		Task: "publish {{review_file}}", Prompt: "{{task}}",
	}
}

// ghFalso puts a fake `gh` in front of PATH. Each call writes "start" and "end"
// to the returned log; the script decides the rest.
func ghFalso(t *testing.T, script string) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "gh.log")
	corpo := "#!/bin/sh\necho start >> " + log + "\n" + script + "\necho end >> " + log + "\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(corpo), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func contaNoLog(path, linha string) int {
	data, _ := os.ReadFile(path)
	n := 0
	for _, l := range strings.Split(string(data), "\n") {
		if l == linha {
			n++
		}
	}
	return n
}

// relatorioDaFrota has two findings; the history has one too, so numbering the
// ticks over the whole report would point at the wrong finding.
const relatorioDaFrota = "## Findings\n\n### First\n\nreal bug\n\n### Second\n\nfalse positive\n"
const relatorioDaHistoria = "## Findings\n\n### Contexto\n\na história do PR\n"

// esperaPublicar runs `[historia, frota, publish]` until the card stops to wait
// for you. post is the post agent; frota, what the fleet prints.
func esperaPublicar(t *testing.T, dir, frota string, post config.AgentDef) (*Manager, chan []byte, jobView) {
	t.Helper()
	cfg := cfgFor(t, "echo")
	cfg.Agent.Prompt = "{{task}}"
	nao, semClone := false, false
	cfg.Agents = []config.AgentDef{
		{Name: "historia", Command: "printf", Args: []string{"%s", relatorioDaHistoria}, Publishable: &nao},
		{Name: "frota", Command: "printf", Args: []string{"%s", frota}},
	}
	cfg.Pipelines = []config.Pipeline{{Name: "ler antes", Steps: []string{"historia", "frota", "publish"}}}
	if post.Checkout == nil {
		post.Checkout = &semClone
	}
	cfg.PostAgent = post
	choice, err := cfg.ChoiceByName("ler antes")
	if err != nil {
		t.Fatalf("ChoiceByName: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	hub := NewHub()
	ch := hub.Subscribe()
	t.Cleanup(func() { hub.Unsubscribe(ch) })

	m := NewManager(ctx, cfg, dir, 1, false, hub)
	view, err := m.Enqueue(testPR(482), false, choice)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	fim := waitFor(t, ch, view.ID, StateDone)
	if !fim.AwaitsPublish {
		t.Fatalf("o card devia esperar você publicar: %+v", fim)
	}
	return m, ch, fim
}

// Publishing through the post agent ends the card's wait and marks it published.
func TestPublicarPeloAgenteEncerraAEspera(t *testing.T) {
	m, ch, fim := esperaPublicar(t, t.TempDir(), relatorioDaFrota,
		config.AgentDef{Name: "post", Command: "cat", Task: "publish {{review_file}}", Prompt: "{{task}}"})
	if _, err := m.PublishWithAgent(fim.ID, nil); err != nil {
		t.Fatalf("PublishWithAgent: %v", err)
	}
	end := waitFor(t, ch, fim.ID, StateDone)
	if !end.Posted || end.AwaitsPublish {
		t.Errorf("publicado, o card sai da espera: posted=%v awaits=%v err=%q", end.Posted, end.AwaitsPublish, end.PostErr)
	}
	// And published is published: another click does not send the agent again.
	if _, err := m.PublishWithAgent(fim.ID, nil); err == nil {
		t.Error("o review já foi ao PR — publicar de novo devia ser recusado")
	}
}

// A publication that fails or is canceled puts the card back to waiting: nothing
// went to the PR.
func TestPublicacaoQueFalhaContinuaEsperando(t *testing.T) {
	t.Run("agente de post falha", func(t *testing.T) {
		m, ch, fim := esperaPublicar(t, t.TempDir(), relatorioDaFrota,
			config.AgentDef{Name: "post", Command: "false", Task: "publish {{review_file}}", Prompt: "{{task}}"})
		if _, err := m.PublishWithAgent(fim.ID, nil); err != nil {
			t.Fatalf("PublishWithAgent: %v", err)
		}
		end := waitFor(t, ch, fim.ID, StateDone)
		if end.Posted || !end.AwaitsPublish || end.PostErr == "" {
			t.Errorf("a falha não publicou nada: posted=%v awaits=%v err=%q", end.Posted, end.AwaitsPublish, end.PostErr)
		}
	})
	t.Run("publicação cancelada", func(t *testing.T) {
		m, ch, fim := esperaPublicar(t, t.TempDir(), relatorioDaFrota,
			config.AgentDef{Name: "post", Command: "sleep", Args: []string{"5"}, Task: "publish {{review_file}}", Prompt: "{{task}}"})
		if _, err := m.PublishWithAgent(fim.ID, nil); err != nil {
			t.Fatalf("PublishWithAgent: %v", err)
		}
		if err := m.Cancel(fim.ID); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		end := waitFor(t, ch, fim.ID, StateDone)
		if end.Posted || !end.AwaitsPublish {
			t.Errorf("cancelada, a publicação não foi: posted=%v awaits=%v", end.Posted, end.AwaitsPublish)
		}
	})
	t.Run("comentário falha", func(t *testing.T) {
		log := ghFalso(t, "exit 1")
		m, _, fim := esperaPublicar(t, t.TempDir(), relatorioDaFrota, config.AgentDef{Name: "post", Command: "echo"})
		for i := range 2 {
			_, err := m.Post(context.Background(), fim.ID, nil)
			if err == nil || errors.Is(err, errPublishBusy) {
				t.Fatalf("tentativa %d: queria o erro do gh, veio %v", i, err)
			}
		}
		if n := contaNoLog(log, "start"); n != 2 {
			t.Errorf("a falha devia liberar a segunda tentativa: gh chamado %d vezes", n)
		}
		v, _ := m.View(fim.ID, false)
		if v.Posted || !v.AwaitsPublish || v.PostErr == "" {
			t.Errorf("o comentário não foi: posted=%v awaits=%v err=%q", v.Posted, v.AwaitsPublish, v.PostErr)
		}
	})
	t.Run("comentário vai", func(t *testing.T) {
		log := ghFalso(t, "cat > /dev/null")
		m, _, fim := esperaPublicar(t, t.TempDir(), relatorioDaFrota, config.AgentDef{Name: "post", Command: "echo"})
		v, err := m.Post(context.Background(), fim.ID, nil)
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		if !v.Posted || v.AwaitsPublish {
			t.Errorf("comentado, o card sai da espera: posted=%v awaits=%v", v.Posted, v.AwaitsPublish)
		}
		if n := contaNoLog(log, "end"); n != 1 {
			t.Errorf("gh chamado %d vezes", n)
		}
	})
}

// Ticks are numbered over what goes to the PR, not the whole report: [1] is the
// fleet's second finding, even though the history has a finding before it.
func TestPularAchadoNumeradoNoQueVaiAoPR(t *testing.T) {
	dir := t.TempDir()
	m, ch, fim := esperaPublicar(t, dir, relatorioDaFrota, agenteQueLeACopia())
	cheio, _ := m.View(fim.ID, true)
	if strings.Count(cheio.PublishHTML, `class="finding"`) != 2 {
		t.Fatalf("o que vai ao PR devia ter os dois achados da frota:\n%s", cheio.PublishHTML)
	}
	if _, err := m.PublishWithAgent(fim.ID, []int{1}); err != nil {
		t.Fatalf("PublishWithAgent: %v", err)
	}
	waitFor(t, ch, fim.ID, StateDone)
	log, _ := m.Log(fim.ID, 0)
	var lido strings.Builder
	var viuCaminho bool
	for _, l := range log.Lines {
		if strings.Contains(l.Text, filepath.Join(dir, "publish")) {
			viuCaminho = true
			continue
		}
		// Only what comes after the path line was printed by the post agent.
		if viuCaminho {
			lido.WriteString(l.Text + "\n")
		}
	}
	got := lido.String()
	if got == "" {
		t.Fatal("o agente de post não imprimiu a cópia que recebeu")
	}
	if strings.Contains(got, "false positive") || !strings.Contains(got, "real bug") {
		t.Errorf("a cópia devia perder o achado 2 e manter o 1:\n%s", got)
	}
	if strings.Contains(got, "a história do PR") {
		t.Errorf("a história não vai ao PR:\n%s", got)
	}
}

// Two publications of the same review with different selections do not share
// the copy: each gets its own, and one does not overwrite the other.
func TestCopiaDePublicacaoEPorPublicacao(t *testing.T) {
	dir := t.TempDir()
	orig := filepath.Join(dir, "acme-api-core-482-20260909-120000.md")
	if err := os.WriteFile(orig, []byte("# acme/api-core#482\n\n---\n\n"+relatorioDaFrota), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(context.Background(), cfgFor(t, "echo"), dir, 1, false, NewHub())
	a, err := m.publishInputFor(orig, relatorioDaFrota, []int{0}, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.publishInputFor(orig, relatorioDaFrota, []int{1}, false)
	if err != nil {
		t.Fatal(err)
	}
	if a.Path == b.Path {
		t.Fatalf("as duas publicações gravaram no mesmo arquivo: %s", a.Path)
	}
	da, _ := os.ReadFile(a.Path)
	db, _ := os.ReadFile(b.Path)
	if strings.Contains(string(da), "real bug") || !strings.Contains(string(db), "real bug") || strings.Contains(string(db), "false positive") {
		t.Errorf("uma cópia mexeu na outra:\n%s\n---\n%s", da, db)
	}
	if a.Source != orig || b.Source != orig {
		t.Errorf("a cópia tem de lembrar de onde veio: %q %q", a.Source, b.Source)
	}
}

// The same file published twice from the Saved tab while the first is still
// running returns the same job, even with a finding unticked, when what goes to
// the agent is a copy in publish/.
func TestPublishSavedDeduplicaPelaOrigem(t *testing.T) {
	dir := t.TempDir()
	orig := filepath.Join(dir, "acme-api-core-482-20260909-120000.md")
	if err := os.WriteFile(orig, []byte("# acme/api-core#482\n\n---\n\n"+relatorioDaFrota), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := cfgFor(t, "sleep", "5")
	semClone := false
	cfg.PostAgent = config.AgentDef{Name: "post", Task: "publish {{review_file}}", Checkout: &semClone, Prompt: "{{task}}"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, cfg, dir, 1, false, NewHub())
	um, err := m.PublishSaved(testPR(482), false, orig, relatorioDaFrota, []int{1})
	if err != nil {
		t.Fatalf("PublishSaved: %v", err)
	}
	dois, err := m.PublishSaved(testPR(482), false, orig, relatorioDaFrota, []int{1})
	if err != nil {
		t.Fatalf("PublishSaved (2): %v", err)
	}
	if um.ID != dois.ID {
		t.Errorf("o mesmo arquivo virou duas publicações: %s e %s", um.ID, dois.ID)
	}
	_ = m.Cancel(um.ID)
}

// A report fenced in ``` comes out of the fence step by step: what goes to the
// PR comes from each step's body.
func TestDesembrulhaPorPassoNoQueVaiAoPR(t *testing.T) {
	const cercado = "```markdown\n# Review Fleet — PR #482\n\n## Findings\n\n### Token stored client-side\n\nThe refresh token is written to localStorage.\n```"
	m, _, fim := esperaPublicar(t, t.TempDir(), cercado, config.AgentDef{Name: "post", Command: "echo"})
	cheio, _ := m.View(fim.ID, true)
	if strings.Contains(cheio.PublishHTML, "```") || !strings.Contains(cheio.PublishHTML, "<h1>") {
		t.Errorf("o que vai ao PR devia sair do bloco e renderizar:\n%s", cheio.PublishHTML)
	}
}

// Only a card with something to choose waits: nothing publishable, an agent
// that already posted on its own, or no publishable step with output leave the
// card at the plain end.
func TestQuandoAPipelineNaoEspera(t *testing.T) {
	nao := false
	for _, tc := range []struct {
		nome   string
		agents []config.AgentDef
		steps  []string
	}{
		{"nada publicável", []config.AgentDef{
			{Name: "historia", Command: "echo", Args: []string{"a história"}, Publishable: &nao},
		}, []string{"historia", "publish"}},
		{"agente que posta sozinho", []config.AgentDef{
			{Name: "frota-post", Command: "echo", Args: []string{"postei"}, Posts: true},
		}, []string{"frota-post", "publish"}},
		{"todo passo publicável falhou", []config.AgentDef{
			{Name: "historia", Command: "echo", Args: []string{"a história"}, Publishable: &nao},
			{Name: "frota", Command: "false"},
		}, []string{"historia", "frota", "publish"}},
	} {
		t.Run(tc.nome, func(t *testing.T) {
			cfg := cfgFor(t, "echo")
			cfg.Agent.Prompt = "{{task}}"
			cfg.Agents = tc.agents
			cfg.Pipelines = []config.Pipeline{{Name: "p", Steps: tc.steps}}
			choice, err := cfg.ChoiceByName("p")
			if err != nil {
				t.Fatalf("ChoiceByName: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hub := NewHub()
			ch := hub.Subscribe()
			defer hub.Unsubscribe(ch)
			m := NewManager(ctx, cfg, t.TempDir(), 1, false, hub)
			view, err := m.Enqueue(testPR(482), false, choice)
			if err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			fim := waitFor(t, ch, view.ID, StateDone)
			if fim.AwaitsPublish {
				t.Error("não há o que escolher — o card não pode ficar esperando você")
			}
		})
	}
}

// A review that was not saved reaches neither the wait nor the comment button:
// the gate cannot be bypassed exactly when something already went wrong.
func TestPipelineQueNaoSalvouNaoPublica(t *testing.T) {
	log := ghFalso(t, "cat > /dev/null")
	cfg := cfgFor(t, "echo")
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agents = []config.AgentDef{{Name: "frota", Command: "printf", Args: []string{"%s", relatorioDaFrota}}}
	cfg.Pipelines = []config.Pipeline{{Name: "p", Steps: []string{"frota", "publish"}}}
	choice, err := cfg.ChoiceByName("p")
	if err != nil {
		t.Fatalf("ChoiceByName: %v", err)
	}
	// A file where the directory should be: Save has nowhere to write.
	dir := filepath.Join(t.TempDir(), "arquivo")
	if err := os.WriteFile(dir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)
	m := NewManager(ctx, cfg, dir, 1, false, hub)
	view, err := m.Enqueue(testPR(482), false, choice)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	fim := waitFor(t, ch, view.ID, StateFailed)
	if fim.AwaitsPublish || !strings.Contains(fim.Err, "could not save") {
		t.Errorf("o card devia falhar dizendo por quê: awaits=%v err=%q", fim.AwaitsPublish, fim.Err)
	}
	if _, err := m.Post(context.Background(), view.ID, nil); err == nil {
		t.Error("comentar contornaria o passo publish")
	}
	if _, err := m.PublishWithAgent(view.ID, nil); err == nil {
		t.Error("o agente de post contornaria o passo publish")
	}
	if n := contaNoLog(log, "start"); n != 0 {
		t.Errorf("nada podia ter ido ao PR: gh chamado %d vezes", n)
	}
}

// poeJob puts a finished job in the Manager, as if a review had just ended.
// The route tests use it so they do not depend on an agent.
func poeJob(m *Manager, j *Job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[j.ID] = j
	m.order = append(m.order, j.ID)
}

func jobEsperando(t *testing.T, srv *Server, id string) *Job {
	t.Helper()
	path := filepath.Join(t.TempDir(), "review.md")
	if err := os.WriteFile(path, []byte("# x\n\n---\n\n"+relatorioDaFrota), 0o644); err != nil {
		t.Fatal(err)
	}
	pr := testPR(482)
	j := &Job{
		ID:            id,
		PR:            pr,
		Choice:        srv.cfg.DefaultChoice(),
		State:         StateDone,
		QueuedAt:      time.Now(),
		SavedTo:       path,
		Result:        agent.Result{PR: pr, Body: relatorioDaFrota, StoppedAt: 1},
		AwaitsPublish: true,
	}
	if !j.Choice.Publishable {
		t.Fatalf("a escolha padrão devia ser publicável: %+v", j.Choice)
	}
	poeJob(srv.jobs, j)
	return j
}

// "Keep it here": the card stops waiting without publishing anything, and the
// page learns about it over SSE.
func TestDispensarAPublicacao(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	post := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	jobEsperando(t, srv, "j900")
	ch := srv.hub.Subscribe()
	defer srv.hub.Unsubscribe(ch)

	rec := post("/api/jobs/j900/dismiss")
	if rec.Code != http.StatusOK {
		t.Fatalf("dispensar: %d %s", rec.Code, rec.Body)
	}
	var v jobView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.AwaitsPublish || v.Posted || v.State != StateDone {
		t.Errorf("dispensado, o card não espera e nada foi: %+v", v)
	}
	select {
	case msg := <-ch:
		var ev struct {
			Type string  `json:"type"`
			Data jobView `json:"data"`
		}
		if err := json.Unmarshal(msg, &ev); err != nil || ev.Type != "job" || ev.Data.ID != "j900" || ev.Data.AwaitsPublish {
			t.Errorf("o SSE devia levar o card fora da espera: %s", msg)
		}
	case <-time.After(2 * time.Second):
		t.Error("dispensar não avisou a página")
	}
	if rec := post("/api/jobs/j900/dismiss"); rec.Code != http.StatusConflict {
		t.Errorf("o que não espera não tem o que dispensar: %d", rec.Code)
	}
	if rec := post("/api/jobs/j404/dismiss"); rec.Code == http.StatusOK {
		t.Errorf("job inexistente: %d", rec.Code)
	}
}

// Two clicks at once publish only once: the second is refused while the first
// is still in gh, through the comment or through the agent.
func TestPublicacaoDuplaERecusada(t *testing.T) {
	log := ghFalso(t, "cat > /dev/null\nsleep 1")
	srv := newTestServer(t)
	h := srv.Handler()
	post := func(path string) int {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	jobEsperando(t, srv, "j900")

	primeiro := make(chan int, 1)
	go func() { primeiro <- post("/api/jobs/j900/post") }()
	deadline := time.Now().Add(5 * time.Second)
	for contaNoLog(log, "start") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("o gh falso não foi chamado")
		}
		time.Sleep(10 * time.Millisecond)
	}

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, rota := range []string{"/api/jobs/j900/post", "/api/jobs/j900/publish"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = post(rota)
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusConflict {
			t.Errorf("clique %d durante a publicação devia dar 409, deu %d", i, c)
		}
	}
	if c := <-primeiro; c != http.StatusOK {
		t.Fatalf("o primeiro clique devia publicar: %d", c)
	}
	if c := post("/api/jobs/j900/post"); c != http.StatusConflict {
		t.Errorf("já publicado, outro comentário devia dar 409: %d", c)
	}
	if c := post("/api/jobs/j900/publish"); c != http.StatusConflict {
		t.Errorf("já publicado, o agente de post devia dar 409: %d", c)
	}
	if n := contaNoLog(log, "start"); n != 1 {
		t.Errorf("o PR recebeu %d comentários", n)
	}
	v, _ := srv.jobs.View("j900", false)
	if !v.Posted || v.AwaitsPublish || v.Publishing {
		t.Errorf("o card devia terminar publicado uma vez: %+v", v)
	}
}

// The session window does not drop the card waiting on you: it is the only
// place that review goes to the PR from with the choice made on screen.
func TestTrimGuardaQuemEspera(t *testing.T) {
	m := NewManager(context.Background(), cfgFor(t, "echo"), t.TempDir(), 1, false, NewHub())
	for i := range maxJobs + 1 {
		poeJob(m, &Job{ID: fmt.Sprintf("j%d", i), State: StateDone, AwaitsPublish: i == 0})
	}
	m.mu.Lock()
	m.trimLocked()
	_, espera := m.jobs["j0"]
	_, velho := m.jobs["j1"]
	n := len(m.order)
	m.mu.Unlock()
	if !espera {
		t.Error("o card que espera você publicar foi descartado")
	}
	if velho || n != maxJobs {
		t.Errorf("devia sair o mais velho que não espera: j1 ficou=%v, %d jobs", velho, n)
	}
}

// The copy in publish/ exists only for the post agent to read: once the
// publication ends (success, failure or cancel) it serves no one, and one per
// click must not pile up.
func TestCopiaDePublicacaoSomeDepoisDoAgente(t *testing.T) {
	casos := []struct {
		nome string
		post config.AgentDef
		fim  func(t *testing.T, m *Manager, id string)
	}{
		{"agente publica", config.AgentDef{Name: "post", Command: "cat", Task: "publish {{review_file}}", Prompt: "{{task}}"}, nil},
		{"agente falha", config.AgentDef{Name: "post", Command: "false", Task: "publish {{review_file}}", Prompt: "{{task}}"}, nil},
		{"publicação cancelada", config.AgentDef{Name: "post", Command: "sleep", Args: []string{"5"}, Task: "publish {{review_file}}", Prompt: "{{task}}"},
			func(t *testing.T, m *Manager, id string) {
				if err := m.Cancel(id); err != nil {
					t.Fatalf("Cancel: %v", err)
				}
			}},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			dir := t.TempDir()
			m, ch, fim := esperaPublicar(t, dir, relatorioDaFrota, c.post)
			if _, err := m.PublishWithAgent(fim.ID, []int{1}); err != nil {
				t.Fatalf("PublishWithAgent: %v", err)
			}
			if c.fim != nil {
				c.fim(t, m, fim.ID)
			}
			waitFor(t, ch, fim.ID, StateDone)
			// waitFor returns at the first done event; the worker may still be
			// finishing, so give the cleanup a moment before declaring a leak.
			var copias []string
			for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
				copias, _ = filepath.Glob(filepath.Join(dir, "publish", "*.md"))
				if len(copias) == 0 {
					break
				}
			}
			if len(copias) != 0 {
				t.Errorf("a cópia devia sumir quando a publicação termina: %v", copias)
			}
			if _, err := os.Stat(fim.SavedTo); err != nil {
				t.Errorf("o review salvo não pode sumir com a cópia: %v", err)
			}
		})
	}
}

// Publishing from the Saved tab also uses a copy, and it goes away the same way.
func TestCopiaDePublicacaoSomeNoPublishSaved(t *testing.T) {
	dir := t.TempDir()
	orig := filepath.Join(dir, "acme-api-core-482-20260909-120000.md")
	if err := os.WriteFile(orig, []byte("# acme/api-core#482\n\n---\n\n"+relatorioDaFrota), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := cfgFor(t, "echo")
	semClone := false
	cfg.PostAgent = config.AgentDef{Name: "post", Command: "cat", Task: "publish {{review_file}}", Prompt: "{{task}}", Checkout: &semClone}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)
	m := NewManager(ctx, cfg, dir, 1, false, hub)
	v, err := m.PublishSaved(testPR(482), false, orig, relatorioDaFrota, []int{1})
	if err != nil {
		t.Fatalf("PublishSaved: %v", err)
	}
	waitFor(t, ch, v.ID, StateDone)
	var copias []string
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		copias, _ = filepath.Glob(filepath.Join(dir, "publish", "*.md"))
		if len(copias) == 0 {
			break
		}
	}
	if len(copias) != 0 {
		t.Errorf("a cópia devia sumir: %v", copias)
	}
	if _, err := os.Stat(orig); err != nil {
		t.Errorf("o review salvo não pode sumir: %v", err)
	}
}

// A publication canceled while queued and sent again leaves two entries for
// the same job in the queue. With two workers, one of them takes the stale
// entry while the other runs the second publication. Whichever worker runs it,
// the live copy is not deleted under the post agent, which reads it and
// publishes, and no copy is left behind.
func TestRepublicarDepoisDeCancelarNaFila(t *testing.T) {
	dir := t.TempDir()
	libera := filepath.Join(t.TempDir(), "libera")
	cfg := cfgFor(t, "echo")
	cfg.Agent.Prompt = "{{task}}"
	semClone := false
	post := agenteQueLeACopia()
	// The post agent waits before reading, so the worker holding the other
	// queue entry gets to it while the copy is still needed.
	post.Args = []string{"-c", `sleep 1; read cmd file; echo "$file"; cat "$file"`}
	post.Checkout = &semClone
	cfg.PostAgent = post
	cfg.Agents = []config.AgentDef{
		{Name: "frota", Command: "printf", Args: []string{"%s", relatorioDaFrota}},
		// Holds a worker until the test creates the file.
		{Name: "trava", Command: "sh", Args: []string{"-c", `while [ ! -e "$0" ]; do sleep 0.05; done; echo livre`, libera}},
	}
	frota, err := cfg.ChoiceByName("frota")
	if err != nil {
		t.Fatalf("ChoiceByName: %v", err)
	}
	trava, err := cfg.ChoiceByName("trava")
	if err != nil {
		t.Fatalf("ChoiceByName: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	m := NewManager(ctx, cfg, dir, 2, false, hub)
	v, err := m.Enqueue(testPR(482), false, frota)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, ch, v.ID, StateDone)

	// Both workers busy: the publications below stay in the queue.
	var travas []string
	for _, n := range []int{1, 2} {
		tv, err := m.Enqueue(testPR(n), false, trava)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		travas = append(travas, tv.ID)
	}
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		rodando := 0
		for _, id := range travas {
			if tv, _ := m.View(id, false); tv.State == StateRunning {
				rodando++
			}
		}
		if rodando == len(travas) {
			break
		}
		if time.Now().After(end) {
			t.Fatal("os dois workers deviam estar ocupados")
		}
	}

	if _, err := m.PublishWithAgent(v.ID, []int{1}); err != nil {
		t.Fatalf("PublishWithAgent: %v", err)
	}
	if err := m.Cancel(v.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := m.PublishWithAgent(v.ID, []int{1}); err != nil {
		t.Fatalf("PublishWithAgent de novo: %v", err)
	}
	if err := os.WriteFile(libera, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	var fim jobView
	for end := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		fim, _ = m.View(v.ID, false)
		if fim.State == StateDone && (fim.Posted || fim.PostErr != "") {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("a publicação não terminou: %+v", fim)
		}
	}
	if !fim.Posted || fim.PostErr != "" {
		t.Fatalf("a segunda publicação devia concluir: posted=%v err=%q", fim.Posted, fim.PostErr)
	}
	log, _ := m.Log(v.ID, 0)
	var lido strings.Builder
	var viuCaminho bool
	for _, l := range log.Lines {
		if strings.Contains(l.Text, filepath.Join(dir, "publish")) {
			viuCaminho = true
			continue
		}
		// Only what comes after the path line was printed by the post agent.
		if viuCaminho {
			lido.WriteString(l.Text + "\n")
		}
	}
	got := lido.String()
	if !strings.Contains(got, "real bug") || strings.Contains(got, "false positive") {
		t.Errorf("o agente de post devia ler a cópia da segunda publicação:\n%s", got)
	}
	var copias []string
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		copias, _ = filepath.Glob(filepath.Join(dir, "publish", "*.md"))
		if len(copias) == 0 {
			break
		}
	}
	if len(copias) != 0 {
		t.Errorf("nenhuma das duas cópias podia sobrar: %v", copias)
	}
}

// A publication canceled or removed while still queued never reaches the post
// agent, so its copy goes away right there — not later, when a worker happens
// to pull the dead entry out of the queue.
func TestCopiaSomeQuandoAPublicacaoSaiDaFila(t *testing.T) {
	casos := []struct {
		nome   string
		salvo  bool
		remove bool
	}{
		{"salvo cancelado", true, false},
		{"salvo removido", true, true},
		{"no card removido", false, true},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			dir := t.TempDir()
			libera := filepath.Join(t.TempDir(), "libera")
			cfg := cfgFor(t, "echo")
			cfg.Agent.Prompt = "{{task}}"
			semClone := false
			cfg.PostAgent = config.AgentDef{Name: "post", Command: "cat", Task: "publish {{review_file}}", Prompt: "{{task}}", Checkout: &semClone}
			cfg.Agents = []config.AgentDef{
				{Name: "frota", Command: "printf", Args: []string{"%s", relatorioDaFrota}},
				{Name: "trava", Command: "sh", Args: []string{"-c", `while [ ! -e "$0" ]; do sleep 0.05; done; echo livre`, libera}},
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hub := NewHub()
			ch := hub.Subscribe()
			defer hub.Unsubscribe(ch)
			m := NewManager(ctx, cfg, dir, 1, false, hub)
			// Release the worker on the way out, before the temp dirs go.
			defer os.WriteFile(libera, nil, 0o644)

			var reviewID string
			if !c.salvo {
				frota, _ := cfg.ChoiceByName("frota")
				v, err := m.Enqueue(testPR(482), false, frota)
				if err != nil {
					t.Fatalf("Enqueue: %v", err)
				}
				waitFor(t, ch, v.ID, StateDone)
				reviewID = v.ID
			}
			trava, _ := cfg.ChoiceByName("trava")
			tv, err := m.Enqueue(testPR(1), false, trava)
			if err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			waitFor(t, ch, tv.ID, StateRunning)

			var pub jobView
			if c.salvo {
				orig := filepath.Join(dir, "acme-api-core-482-20260909-120000.md")
				if err := os.WriteFile(orig, []byte("# acme/api-core#482\n\n---\n\n"+relatorioDaFrota), 0o644); err != nil {
					t.Fatal(err)
				}
				pub, err = m.PublishSaved(testPR(482), false, orig, relatorioDaFrota, []int{1})
			} else {
				pub, err = m.PublishWithAgent(reviewID, []int{1})
			}
			if err != nil {
				t.Fatalf("publicando: %v", err)
			}
			if pub.State != StateQueued {
				t.Fatalf("a publicação devia esperar na fila: %s", pub.State)
			}
			if copias, _ := filepath.Glob(filepath.Join(dir, "publish", "*.md")); len(copias) != 1 {
				t.Fatalf("devia haver uma cópia na fila: %v", copias)
			}
			if c.remove {
				err = m.Remove(pub.ID)
			} else {
				err = m.Cancel(pub.ID)
			}
			if err != nil {
				t.Fatalf("tirando da fila: %v", err)
			}
			if copias, _ := filepath.Glob(filepath.Join(dir, "publish", "*.md")); len(copias) != 0 {
				t.Errorf("a cópia devia sumir quando a publicação sai da fila: %v", copias)
			}
			if err := os.WriteFile(libera, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			waitFor(t, ch, tv.ID, StateDone)
		})
	}
}

// Each server process has its own id: that is how the page sees the server
// restarted and the job ids started over from j1.
func TestInstanciaDoServidor(t *testing.T) {
	a, b := newTestServer(t), newTestServer(t)
	h := a.Handler()
	estado := func() string {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/state", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var st struct {
			Instance string `json:"instance"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		return st.Instance
	}
	id := estado()
	if id == "" {
		t.Fatal("/api/state devia trazer o id da instância")
	}
	if again := estado(); again != id {
		t.Errorf("o id tem de ser estável dentro do processo: %q depois %q", id, again)
	}
	if other := b.instance; other == "" || other == id {
		t.Errorf("outro processo precisa de outro id: %q e %q", id, other)
	}

	// The stream opens with the same id, before any job event.
	ts := httptest.NewServer(h)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	linhas := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		linhas <- string(buf[:n])
	}()
	select {
	case got := <-linhas:
		if !strings.Contains(got, `"type":"hello"`) || !strings.Contains(got, `"instance":"`+id+`"`) {
			t.Errorf("o primeiro evento do SSE devia ser o hello com a instância %q: %q", id, got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("o SSE não mandou nada ao conectar")
	}
}
