package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	// Na fila o job já anuncia o que vai rodar.
	if view.Agent != "dupla" || len(view.Steps) != 2 || view.Steps[0].State != StateQueued {
		t.Fatalf("job enfileirado devia listar os passos: %+v", view)
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

// Publicar é um job novo, com o agente de post e o review já salvo em mãos —
// e o review original continua lá, do jeito que o usuário leu.
func TestPublishWithAgentQueuesPostJob(t *testing.T) {
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

	pub, err := m.PublishWithAgent(review.ID)
	if err != nil {
		t.Fatalf("PublishWithAgent: %v", err)
	}
	if pub.ID == review.ID {
		t.Fatal("publicar devia virar um job novo")
	}
	if !pub.Publishing || pub.PublishOf != review.ID {
		t.Errorf("o job de publicação devia apontar para o review: %+v", pub)
	}
	if pub.Agent != "post-report" {
		t.Errorf("devia rodar o agente de post, roda %q", pub.Agent)
	}

	// Pedir de novo enquanto roda não abre um segundo.
	again, err := m.PublishWithAgent(review.ID)
	if err != nil {
		t.Fatalf("PublishWithAgent (2): %v", err)
	}
	if again.ID != pub.ID {
		t.Errorf("publicação duplicada: %s e %s", pub.ID, again.ID)
	}

	end := waitFor(t, ch, pub.ID, StateDone)
	// O prompt do post levou o caminho do review lido.
	full, _ := m.View(pub.ID, true)
	if !strings.Contains(full.Body, done.SavedTo) {
		t.Errorf("o agente de post não recebeu o arquivo do review:\n%s", full.Body)
	}
	// E a publicação não gerou um segundo .md.
	if end.SavedTo != "" {
		t.Errorf("publicação não devia salvar arquivo, salvou %q", end.SavedTo)
	}
	// Só um .md no diretório: o índice de revisados mora ali do lado, mas
	// publicar não escreve um segundo review.
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
	if _, err := m.PublishWithAgent(view.ID); err == nil {
		t.Error("review em andamento não devia poder ser publicado")
	}
	if _, err := m.PublishWithAgent("j999"); err == nil {
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
		t.Errorf("from inválido devia dar 400, deu %d", rec.Code)
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
	if !publisher.Publisher || publisher.Name != "post-report" {
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
		{"img onerror", "<img src=x onerror=alert(1)>", "onerror"},
		{"svg onload", "<svg onload=alert(1)>", "onload"},
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
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "não está instalada") {
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
	if !strings.Contains(rec.Body.String(), "nenhum agente configurado") {
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
	if !strings.Contains(string(saved), "- Gasto: 33,2k tokens") {
		t.Errorf("o review salvo devia registrar o gasto:\n%s", saved)
	}
}
