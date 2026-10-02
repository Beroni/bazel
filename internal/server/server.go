// Package server serve a interface web do Bazel.
//
// É a mesma máquina do CLI — config, gh, agent, workspace e store não mudam —
// com HTTP no lugar do terminal. A diferença que importa: um review leva
// minutos, então o handler enfileira e responde na hora, e o resultado chega
// ao navegador pelo stream de /api/events.
//
// É single-user por construção: usa o `gh` já autenticado da máquina e o
// mesmo ~/.bazel/config.yaml do CLI. Por isso escuta em loopback e recusa
// requisição que venha de outra origem — quem chega aqui manda clonar
// repositório e rodar agente com shell.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/beroni/bazel/internal/config"
	"github.com/beroni/bazel/internal/gh"
	"github.com/beroni/bazel/internal/skills"
	"github.com/beroni/bazel/internal/store"
)

//go:embed static
var staticFS embed.FS

// prTTL é quanto tempo a lista de PRs vale antes de consultar o gh de novo.
const prTTL = 60 * time.Second

// Options são os parâmetros do `bazel serve`.
type Options struct {
	Addr        string
	Concurrency int
	Keep        bool
	Version     string
}

// Server é a interface web.
type Server struct {
	cfg        *config.Config
	me         string
	reviewsDir string
	configPath string
	opts       Options
	hub        *Hub
	jobs       *Manager
	done       chan struct{}

	// cfgMu protege os campos do config que a interface edita (repos).
	cfgMu sync.Mutex

	// fetchMu serializa as consultas ao gh; prMu protege o cache.
	fetchMu sync.Mutex
	prMu    sync.RWMutex
	prs     []gh.PR
	prErrs  []gh.RepoError
	prsAt   time.Time
}

// New monta o servidor. ctx é o dono dos reviews em andamento: cancelou,
// morreram junto.
func New(ctx context.Context, cfg *config.Config, me string, opts Options) (*Server, error) {
	reviewsDir, err := cfg.ResolvedReviewsDir()
	if err != nil {
		return nil, err
	}
	configPath, err := config.Path()
	if err != nil {
		return nil, err
	}
	hub := NewHub()
	return &Server{
		cfg:        cfg,
		me:         me,
		reviewsDir: reviewsDir,
		configPath: configPath,
		opts:       opts,
		hub:        hub,
		jobs:       NewManager(ctx, cfg, reviewsDir, opts.Concurrency, opts.Keep, hub),
		done:       make(chan struct{}),
	}, nil
}

// Handler devolve as rotas já embrulhadas na checagem de origem.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Os assets vão com no-store, como o index. Eles estão embutidos no
	// binário: atualizar o Bazel muda o js e o css, e um navegador servindo a
	// versão anterior contra um servidor novo é um bug que não se reproduz na
	// máquina de quem escreveu. Não há banda a economizar aqui — é loopback.
	assets, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", semCache(http.FileServer(http.FS(assets)))))
	mux.HandleFunc("GET /{$}", s.handleIndex)

	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/prs", s.handlePRs)
	mux.HandleFunc("POST /api/reviews", s.handleReview)
	mux.HandleFunc("GET /api/jobs", s.handleJobs)
	mux.HandleFunc("GET /api/jobs/{id}", s.handleJob)
	mux.HandleFunc("GET /api/jobs/{id}/log", s.handleJobLog)
	mux.HandleFunc("DELETE /api/jobs/{id}", s.handleJobRemove)
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.handleCancel)
	mux.HandleFunc("POST /api/jobs/{id}/post", s.handlePost)
	mux.HandleFunc("POST /api/jobs/{id}/publish", s.handlePublish)
	mux.HandleFunc("POST /api/jobs/{id}/continue", s.handleContinue)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/repos", s.handleReposList)
	mux.HandleFunc("POST /api/repos", s.handleRepoAdd)
	mux.HandleFunc("DELETE /api/repos/{owner}/{name}", s.handleRepoRemove)
	mux.HandleFunc("GET /api/agents", s.handleAgentsList)
	mux.HandleFunc("POST /api/agents", s.handleAgentAdd)
	mux.HandleFunc("DELETE /api/agents/{name}", s.handleAgentRemove)
	mux.HandleFunc("POST /api/agents/{name}/default", s.handleAgentDefault)
	mux.HandleFunc("POST /api/agents/{name}/publishable", s.handleAgentPublishable)
	mux.HandleFunc("POST /api/pipelines", s.handlePipelineAdd)
	mux.HandleFunc("DELETE /api/pipelines/{name}", s.handlePipelineRemove)
	mux.HandleFunc("GET /api/config/file", s.handleConfigDownload)
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/skills", s.handleSkills)
	mux.HandleFunc("GET /api/reviews", s.handleSavedList)
	mux.HandleFunc("GET /api/reviews/{name}", s.handleSavedOne)
	mux.HandleFunc("POST /api/reviews/{name}/publish", s.handleSavedPublish)
	mux.HandleFunc("POST /api/reviews/{name}/comment", s.handleSavedComment)

	// O /mcp passa pelo mesmo guard: é a mesma porta para clonar e rodar
	// agente, só que falada por máquina.
	mux.HandleFunc("/mcp", s.handleMCP)

	return s.guard(mux)
}

// semCache manda o navegador não guardar o que vem embutido no binário.
func semCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// Run sobe o servidor e só volta quando o ctx morre.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Handler: s.Handler(),
		// Sem WriteTimeout: o SSE fica aberto o review inteiro.
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("could not listen on %s: %w", s.opts.Addr, err)
	}

	errc := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// Solta os SSE antes do Shutdown, senão ele espera por eles até o fim.
	close(s.done)
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutCtx)
}

// URL é o endereço para abrir no navegador.
func (s *Server) URL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// --- middleware ---

// guard fecha a porta para páginas de terceiros. Um servidor local que clona
// repositórios e sobe agentes não pode aceitar um POST disparado por qualquer
// site aberto em outra aba, nem um Host forjado apontando pra cá.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.loopback() && !localHost(r.Host) {
			http.Error(w, "host not allowed", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host {
					http.Error(w, "origin not allowed", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) loopback() bool {
	host, _, err := net.SplitHostPort(s.opts.Addr)
	if err != nil {
		return true
	}
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func localHost(h string) bool {
	host, _, err := net.SplitHostPort(h)
	if err != nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// --- handlers ---

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	page, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(page)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	repos := append([]string(nil), s.cfg.Repos...)
	authors := append([]string(nil), s.cfg.Authors...)
	drafts := s.cfg.IncludeDrafts
	stray := s.cfg.StrayCommand()
	s.cfgMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"me":             s.me,
		"version":        s.opts.Version,
		"repos":          repos,
		"authors":        authors,
		"include_drafts": drafts,
		"config_path":    s.configPath,
		"reviews_dir":    s.reviewsDir,
		"concurrency":    s.opts.Concurrency,
		"keep_workspace": s.opts.Keep,
		"agent": map[string]any{
			"command":  s.cfg.Agent.Command,
			"args":     s.cfg.Agent.Args,
			"checkout": s.cfg.Agent.Checkout,
			"timeout":  s.cfg.Agent.TimeoutSeconds,
		},
		"agents": s.agentViews(),
		"jobs":   s.jobViews(),
		// Um molde de `agent` que dispara um comando por conta própria faz todo
		// agente rodar dois: o escolhido e o do molde. A página avisa, porque
		// nada no resultado denuncia isso — só o gasto.
		"stray_command": stray,
		// A cota do Claude vem vazia até o primeiro review desta sessão.
		"limits": s.jobs.Limits(),
	})
}

func (s *Server) handlePRs(w http.ResponseWriter, r *http.Request) {
	list, err := s.listPRs(r.Context(), prFilter{
		Refresh: r.URL.Query().Get("refresh") != "",
		Mine:    r.URL.Query().Get("scope") == "mine",
		Author:  r.URL.Query().Get("author"),
		HTML:    true,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// prFilter é o recorte da lista de PRs. A página e o /mcp pedem a mesma
// lista; só o MCP filtra por repositório e por estado do review, e só a
// página precisa da descrição em HTML.
type prFilter struct {
	Refresh bool
	Mine    bool
	Author  string
	Repo    string
	// Status é "" (todos), "unreviewed", "reviewed", "changed" ou "posted".
	Status string
	HTML   bool
}

type prList struct {
	PRs        []prView            `json:"prs"`
	RepoErrors []map[string]string `json:"repo_errors"`
	FetchedAt  time.Time           `json:"fetched_at"`
}

func (s *Server) listPRs(ctx context.Context, f prFilter) (prList, error) {
	prs, repoErrs, at, err := s.loadPRs(ctx, f.Refresh)
	if err != nil {
		return prList{}, err
	}
	author := strings.TrimPrefix(strings.TrimSpace(f.Author), "@")
	repo := strings.TrimSpace(f.Repo)

	now := time.Now()
	// O índice de reviews é relido a cada listagem: um review que terminou
	// em outra aba (ou no CLI) já aparece marcado aqui.
	marks := store.LoadMarks(s.reviewsDir)
	out := make([]prView, 0, len(prs))
	for _, pr := range prs {
		mine := strings.EqualFold(pr.Author.Login, s.me)
		if f.Mine && !mine {
			continue
		}
		if author != "" && !strings.EqualFold(pr.Author.Login, author) {
			continue
		}
		if repo != "" && !strings.EqualFold(pr.Repo, repo) {
			continue
		}
		st := marks.Status(pr)
		if !statusMatches(st, f.Status) {
			continue
		}
		v := newPRView(pr, mine, now).withStatus(st, now)
		if f.HTML {
			v.BodyHTML = renderMarkdown(pr.Body)
		}
		out = append(out, v)
	}

	errs := make([]map[string]string, 0, len(repoErrs))
	for _, re := range repoErrs {
		errs = append(errs, map[string]string{"repo": re.Repo, "error": re.Err.Error()})
	}
	return prList{PRs: out, RepoErrors: errs, FetchedAt: at}, nil
}

func statusMatches(st store.Status, want string) bool {
	switch want {
	case "unreviewed":
		return !st.Reviewed
	case "reviewed":
		return st.Reviewed
	case "changed":
		return st.Reviewed && st.Changed
	case "posted":
		return st.Reviewed && st.Posted
	}
	return true
}

// loadPRs consulta o gh, com cache: cada listagem sobe um `gh pr list` por
// repositório e a página recarrega mais do que a API merece.
func (s *Server) loadPRs(ctx context.Context, force bool) ([]gh.PR, []gh.RepoError, time.Time, error) {
	s.prMu.RLock()
	fresh := time.Since(s.prsAt) < prTTL && !s.prsAt.IsZero()
	prs, errs, at := s.prs, s.prErrs, s.prsAt
	s.prMu.RUnlock()
	if fresh && !force {
		return prs, errs, at, nil
	}

	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()

	// Outro request pode ter buscado enquanto esperávamos o lock.
	s.prMu.RLock()
	fresh = time.Since(s.prsAt) < prTTL && !s.prsAt.IsZero()
	prs, errs, at = s.prs, s.prErrs, s.prsAt
	s.prMu.RUnlock()
	if fresh && !force {
		return prs, errs, at, nil
	}

	s.cfgMu.Lock()
	repos := append([]string(nil), s.cfg.Repos...)
	opts := gh.ListOptions{
		Authors:       append([]string(nil), s.cfg.Authors...),
		IncludeDrafts: s.cfg.IncludeDrafts,
	}
	s.cfgMu.Unlock()

	if len(repos) == 0 {
		now := time.Now()
		s.prMu.Lock()
		s.prs, s.prErrs, s.prsAt = nil, nil, now
		s.prMu.Unlock()
		return nil, nil, now, nil
	}

	found, repoErrs := gh.ListPRs(ctx, repos, opts)
	if err := ctx.Err(); err != nil {
		return nil, nil, time.Time{}, err
	}
	now := time.Now()
	s.prMu.Lock()
	s.prs, s.prErrs, s.prsAt = found, repoErrs, now
	s.prMu.Unlock()

	s.hub.Broadcast("prs", map[string]any{"count": len(found), "fetched_at": now})
	return found, repoErrs, now, nil
}

type reviewRequest struct {
	Refs []string `json:"refs"`
	// Agent é o nome do agente ou pipeline escolhido. Vazio = o padrão.
	Agent string `json:"agent"`
}

func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	var req reviewRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid body: %w", err))
		return
	}
	if len(req.Refs) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("no PR given"))
		return
	}
	choice, err := s.resolveChoice(req.Agent)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	queued, errs := s.enqueueRefs(r.Context(), req.Refs, choice)
	status := http.StatusAccepted
	if len(queued) == 0 {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{"jobs": queued, "errors": errs})
}

// resolveChoice acha o agente ou pipeline pelo nome. Vazio = o padrão.
func (s *Server) resolveChoice(name string) (config.Choice, error) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if strings.TrimSpace(name) != "" {
		return s.cfg.ChoiceByName(name)
	}
	choice := s.cfg.DefaultChoice()
	if len(choice.Steps) == 0 {
		return config.Choice{}, config.ErrNoAgents
	}
	return choice, nil
}

// enqueueRefs põe na fila um review por referência. O que não deu certo volta
// em errs, um por item — um ref ruim não derruba os outros.
func (s *Server) enqueueRefs(ctx context.Context, refs []string, choice config.Choice) (queued []jobView, errs []string) {
	// Os PRs em cache evitam um `gh pr view` por item quando vieram da lista.
	s.prMu.RLock()
	cached := map[string]gh.PR{}
	for _, pr := range s.prs {
		cached[pr.Key()] = pr
	}
	s.prMu.RUnlock()

	for _, ref := range refs {
		repo, number, err := gh.ParseRef(ref)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		key := fmt.Sprintf("%s#%d", repo, number)
		pr, ok := cached[key]
		if !ok {
			pr, err = gh.Get(ctx, repo, number)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %s", key, err))
				continue
			}
		}
		view, err := s.jobs.Enqueue(pr, strings.EqualFold(pr.Author.Login, s.me), choice)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", key, err))
			continue
		}
		queued = append(queued, view)
	}
	return queued, errs
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.jobViews()})
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	view, ok := s.jobs.View(r.PathValue("id"), true)
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("review not found"))
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handleJobLog entrega o log de um review a partir de um ponto. O navegador
// guarda o `next` da resposta anterior e só pede o que falta — é isso que
// deixa o log andar ao vivo sem reenviar tudo a cada segundo.
func (s *Server) handleJobLog(w http.ResponseWriter, r *http.Request) {
	from := 0
	if v := r.URL.Query().Get("from"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid \"from\": %q", v))
			return
		}
		from = n
	}
	view, ok := s.jobs.Log(r.PathValue("id"), from)
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("review not found"))
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handleJobRemove tira o job da fila da sessão. Cancela antes, se ainda
// estiver vivo.
func (s *Server) handleJobRemove(w http.ResponseWriter, r *http.Request) {
	if err := s.jobs.Remove(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": r.PathValue("id")})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.jobs.Cancel(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	skip, err := readSkip(w, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	view, err := s.jobs.Post(r.Context(), r.PathValue("id"), skip)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handlePublish manda o agente publicar no PR o review que o usuário leu. Roda
// no card do próprio review — com passo e log próprios, porque é outro agente
// rodando — e não num job à parte: publicar é o fim do review, não um trabalho
// solto na fila.
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	skip, err := readSkip(w, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	view, err := s.jobs.PublishWithAgent(r.PathValue("id"), skip)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, view)
}

// handleContinue solta uma pipeline que parou para você ler: ela volta para a
// fila e recomeça do passo seguinte, dentro do mesmo clone.
func (s *Server) handleContinue(w http.ResponseWriter, r *http.Request) {
	view, err := s.jobs.Continue(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, view)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := s.hub.Subscribe()
	defer s.hub.Unsubscribe(ch)

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case msg, open := <-ch:
			if !open {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		}
	}
}

func (s *Server) handleReposList(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	repos := append([]string(nil), s.cfg.Repos...)
	s.cfgMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos})
}

func (s *Server) handleRepoAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo string `json:"repo"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid body: %w", err))
		return
	}
	repo := strings.TrimSpace(req.Repo)
	if !strings.Contains(strings.TrimPrefix(repo, "https://github.com/"), "/") {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid repository: %q (use owner/repo)", repo))
		return
	}

	s.cfgMu.Lock()
	added := s.cfg.AddRepo(repo)
	err := s.cfg.Save()
	repos := append([]string(nil), s.cfg.Repos...)
	s.cfgMu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if added {
		s.invalidatePRs()
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos, "added": added})
}

func (s *Server) handleRepoRemove(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("owner") + "/" + r.PathValue("name")

	s.cfgMu.Lock()
	removed := s.cfg.RemoveRepo(repo)
	err := s.cfg.Save()
	repos := append([]string(nil), s.cfg.Repos...)
	s.cfgMu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if removed {
		s.invalidatePRs()
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos, "removed": removed})
}

// --- agentes ---
//
// A lista de agentes começa vazia num config novo e é montada aqui, a partir
// das skills que o Claude Code tem instaladas nesta máquina: cada agente é uma
// skill rodando sobre o PR. Por isso não há lista de fábrica — ela apontaria
// para skills que este computador pode nunca ter tido.

func (s *Server) handleAgentsList(w http.ResponseWriter, r *http.Request) {
	s.writeAgents(w, http.StatusOK, nil)
}

func (s *Server) handleAgentAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Skill string `json:"skill"`
		Posts bool   `json:"posts"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid body: %w", err))
		return
	}

	// A descrição vem da skill instalada, não do navegador: é o frontmatter
	// dela que diz o que ela faz. Skill que não está na máquina nem entra —
	// um agente assim só falharia na hora de rodar.
	name := strings.TrimPrefix(strings.TrimSpace(req.Skill), "/")
	dir, list := s.installedSkills()
	var found *skills.Skill
	for i := range list {
		if strings.EqualFold(list[i].Name, name) {
			found = &list[i]
			break
		}
	}
	if found == nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("the skill %q is not installed in %s", name, dir))
		return
	}

	s.cfgMu.Lock()
	def, err := s.cfg.AddAgentFromSkill(found.Name, summarize(found.Description), req.Posts)
	if err == nil {
		err = s.cfg.Save()
	}
	s.cfgMu.Unlock()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.writeAgents(w, http.StatusCreated, map[string]any{"added": def.Name})
}

func (s *Server) handleAgentRemove(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	removed := s.cfg.RemoveAgent(r.PathValue("name"))
	err := s.cfg.Save()
	s.cfgMu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.writeAgents(w, http.StatusOK, map[string]any{"removed": removed})
}

// handleAgentDefault põe um agente na frente da lista — o primeiro é o que
// roda quando ninguém escolhe.
func (s *Server) handleAgentDefault(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	ok := s.cfg.SetDefaultAgent(r.PathValue("name"))
	err := s.cfg.Save()
	s.cfgMu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("the agent %q is not in the list", r.PathValue("name")))
		return
	}
	s.writeAgents(w, http.StatusOK, nil)
}

// handleAgentPublishable liga ou desliga a ida ao PR da saída de um agente.
// É o tique do painel: nem toda skill produz review, e a que não produz não
// tem por que oferecer um botão de publicar.
func (s *Server) handleAgentPublishable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Publishable bool `json:"publishable"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid body: %w", err))
		return
	}
	s.cfgMu.Lock()
	err := s.cfg.SetAgentPublishable(r.PathValue("name"), req.Publishable)
	if err == nil {
		err = s.cfg.Save()
	}
	s.cfgMu.Unlock()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.writeAgents(w, http.StatusOK, nil)
}

// handlePipelineAdd encadeia agentes da lista numa sequência sobre o mesmo
// clone. Os passos vêm por nome de agente — a página só deixa escolher entre
// os que existem, e o config recusa o resto.
func (s *Server) handlePipelineAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Steps       []string `json:"steps"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid body: %w", err))
		return
	}
	s.cfgMu.Lock()
	p, err := s.cfg.AddPipeline(req.Name, req.Description, req.Steps)
	if err == nil {
		err = s.cfg.Save()
	}
	s.cfgMu.Unlock()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.writeAgents(w, http.StatusCreated, map[string]any{"added": p.Name})
}

func (s *Server) handlePipelineRemove(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	removed := s.cfg.RemovePipeline(r.PathValue("name"))
	err := s.cfg.Save()
	s.cfgMu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.writeAgents(w, http.StatusOK, map[string]any{"removed": removed})
}

// handleConfigDownload entrega o config.yaml como arquivo. É o que se leva
// para outra máquina: repos, agentes e pipelines são a configuração inteira, e
// copiá-la é mais rápido do que remontá-la clique a clique do outro lado.
func (s *Server) handleConfigDownload(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(s.configPath)
	if err != nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("could not read %s", s.configPath))
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="config.yaml"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

// writeAgents devolve a lista inteira depois de mexer nela: é o que a página
// redesenha, do seletor à configuração.
func (s *Server) writeAgents(w http.ResponseWriter, status int, extra map[string]any) {
	out := map[string]any{"agents": s.agentViews()}
	for k, v := range extra {
		out[k] = v
	}
	writeJSON(w, status, out)
}

// summarize corta a descrição de uma skill na primeira frase. O frontmatter
// de uma skill é um parágrafo inteiro, com gatilhos e contraindicações; no
// seletor cabe uma linha.
func summarize(s string) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	for _, end := range []string{". ", " — ", "; "} {
		if i := strings.Index(s, end); i > 0 && i < 200 {
			return s[:i]
		}
	}
	if r := []rune(s); len(r) > 200 {
		return strings.TrimSpace(string(r[:199])) + "…"
	}
	return strings.TrimSuffix(s, ".")
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(s.configPath)
	if err != nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("could not read %s", s.configPath))
		return
	}
	s.cfgMu.Lock()
	stray := s.cfg.StrayCommand()
	s.cfgMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"path": s.configPath, "yaml": string(data), "stray_command": stray,
	})
}

func (s *Server) handleSavedList(w http.ResponseWriter, r *http.Request) {
	entries, err := store.List(s.reviewsDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]savedView, 0, len(entries))
	for _, e := range entries {
		out = append(out, savedView{Entry: e, Publishable: s.agentPublishes(e.Agent)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"dir": s.reviewsDir, "reviews": out})
}

// savedView é um review salvo mais o que só a configuração sabe dizer: se o
// que está ali dentro pode ir ao PR.
type savedView struct {
	store.Entry
	Publishable bool `json:"publishable"`
}

// agentPublishes diz se a saída de um agente pode ir ao PR. Agente que não
// está mais na lista continua publicável: o review em disco sobrevive à
// configuração, e um nome que o Bazel não reconhece mais não é motivo para
// trancar um review que já foi lido e aprovado.
func (s *Server) agentPublishes(name string) bool {
	if strings.TrimSpace(name) == "" {
		return true
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	ch, err := s.cfg.ChoiceByName(name)
	if err != nil {
		return true
	}
	return ch.Publishable
}

func (s *Server) handleSavedOne(w http.ResponseWriter, r *http.Request) {
	body, err := store.Read(s.reviewsDir, r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": r.PathValue("name"),
		"body": body,
		"html": renderReview(store.Unwrap(body)),
	})
}

// --- publicar um review salvo ---
//
// Um review sobrevive ao servidor: está em markdown no disco. A chance de
// levá-lo ao PR tem de sobreviver junto — antes, fechar o Bazel antes de
// publicar deixava o texto lá e nenhuma forma de usá-lo.

// savedReview é um review lido do disco, pronto para ir ao PR: o pull request
// de onde ele veio, onde o arquivo está, o corpo e quem o escreveu.
type savedReview struct {
	PR    gh.PR
	Path  string
	Body  string
	Agent string
}

// savedPR relê um review salvo e devolve o PR de onde ele veio, junto com o
// caminho, o corpo e o agente. O PR é buscado no GitHub: o arquivo guarda o
// cabeçalho, não o estado atual dele.
func (s *Server) savedPR(ctx context.Context, name string) (savedReview, error) {
	file, err := store.Read(s.reviewsDir, name)
	if err != nil {
		return savedReview{}, err
	}
	repo, number := store.PRFromTitle(store.Heading(file))
	if repo == "" {
		return savedReview{}, fmt.Errorf("could not tell which PR %q belongs to", name)
	}
	pr, err := gh.Get(ctx, repo, number)
	if err != nil {
		return savedReview{}, err
	}
	return savedReview{
		PR:    pr,
		Path:  filepath.Join(s.reviewsDir, name),
		Body:  store.ReviewBody(file),
		Agent: store.AgentOf(file),
	}, nil
}

// refusePublish barra o que não é review. Vale para o salvo como vale para o
// job recém-terminado: a história de um PR não vira comentário nele.
func (s *Server) refusePublish(w http.ResponseWriter, sr savedReview) bool {
	if s.agentPublishes(sr.Agent) {
		return false
	}
	writeErr(w, http.StatusBadRequest, fmt.Errorf("what %q produces does not go to the PR", sr.Agent))
	return true
}

// handleSavedPublish roda o agente de publicação sobre um review salvo.
func (s *Server) handleSavedPublish(w http.ResponseWriter, r *http.Request) {
	skip, err := readSkip(w, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sr, err := s.savedPR(r.Context(), r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if s.refusePublish(w, sr) {
		return
	}
	pr := sr.PR
	view, err := s.jobs.PublishSaved(pr, strings.EqualFold(pr.Author.Login, s.me), sr.Path, sr.Body, skip)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, view)
}

// handleSavedComment cola um review salvo no PR, sem agente.
func (s *Server) handleSavedComment(w http.ResponseWriter, r *http.Request) {
	skip, err := readSkip(w, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sr, err := s.savedPR(r.Context(), r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if s.refusePublish(w, sr) {
		return
	}
	if err := s.jobs.CommentSaved(r.Context(), sr.PR, sr.Body, skip); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	s.invalidatePRs()
	writeJSON(w, http.StatusOK, map[string]any{"posted": sr.PR.Key()})
}

// --- helpers ---

// readSkip lê, do corpo de um pedido de publicação, os achados que o usuário
// desmarcou na tela — os índices que o renderReview numerou. Corpo vazio é
// "publica tudo": os quatro caminhos para o PR aceitam a lista e nenhum a
// exige.
func readSkip(w http.ResponseWriter, r *http.Request) ([]int, error) {
	var req struct {
		Skip []int `json:"skip"`
	}
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("invalid body: %w", err)
	}
	for _, i := range req.Skip {
		if i < 0 {
			return nil, fmt.Errorf("invalid finding index %d", i)
		}
	}
	return req.Skip, nil
}

func (s *Server) jobViews() []jobView {
	return s.jobs.Snapshot()
}

// agentViews é o seletor da página: os agents e pipelines do config.yaml, na
// ordem em que estão lá. O primeiro é o padrão.
//
// Cada um vai com a skill que a sua task invoca e se ela está instalada — um
// agente que chama uma skill que você não tem falha só na hora de rodar, e
// isso é tarde demais para descobrir.
func (s *Server) agentViews() []map[string]any {
	_, instaladas := s.installedSkills()
	s.cfgMu.Lock()
	choices := s.cfg.Choices()
	post := s.cfg.PostChoice()
	s.cfgMu.Unlock()
	out := make([]map[string]any, 0, len(choices)+1)
	for _, c := range choices {
		out = append(out, s.agentView(c, instaladas, false))
	}
	// O agente de publicação não está no seletor, mas é um agente: quem abre
	// a configuração quer vê-lo junto.
	out = append(out, s.agentView(post, instaladas, true))
	return out
}

func (s *Server) agentView(c config.Choice, instaladas []skills.Skill, publisher bool) map[string]any {
	usadas := make([]map[string]any, 0, len(c.Steps))
	for _, step := range c.Steps {
		name := skills.TaskSkill(step.Task)
		if name == "" {
			continue
		}
		usadas = append(usadas, map[string]any{
			"name": name,
			// Skill embarcada está sempre disponível: ela viaja no binário, e
			// não há o que instalar nem como faltar.
			"installed": skills.Has(instaladas, name) || skills.IsBuiltin(name),
			"builtin":   skills.IsBuiltin(name),
			"step":      step.Name,
		})
	}
	return map[string]any{
		"name":        c.Name,
		"description": c.Description,
		"pipeline":    c.Pipeline,
		"posts":       c.Posts,
		"publishable": c.Publishable,
		"steps":       c.StepNames(),
		"skills":      usadas,
		// publisher sai do seletor de review: ele roda depois, na publicação.
		"publisher": publisher,
	}
}

// handleSkills lista as skills instaladas na máquina — as que podem virar
// agente. É a lista de onde a configuração monta a sua.
func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	dir, list := s.installedSkills()
	writeJSON(w, http.StatusOK, map[string]any{"dir": dir, "skills": list})
}

// installedSkills lê as skills do disco a cada chamada: instalar uma skill não
// devia exigir reiniciar o Bazel.
func (s *Server) installedSkills() (string, []skills.Skill) {
	s.cfgMu.Lock()
	dir := strings.TrimSpace(s.cfg.SkillsDir)
	s.cfgMu.Unlock()
	if dir == "" {
		dir = skills.DefaultDir()
	}
	// As embarcadas entram na lista: elas viajam no binário, estão sempre
	// disponíveis, e é a partir desta lista que a página deixa montar agentes
	// e pipelines. Uma skill de disco com o mesmo nome perde — dentro do clone
	// quem o Claude Code vai achar é a do projeto, que é a embarcada.
	list := skills.Builtin()
	for _, s := range skills.List(dir) {
		if !skills.IsBuiltin(s.Name) {
			list = append(list, s)
		}
	}
	if list == nil {
		list = []skills.Skill{}
	}
	return dir, list
}

// invalidatePRs força a próxima listagem a bater no gh.
func (s *Server) invalidatePRs() {
	s.prMu.Lock()
	s.prsAt = time.Time{}
	s.prMu.Unlock()
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
