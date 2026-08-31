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
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
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

	assets, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(assets))))
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
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/repos", s.handleReposList)
	mux.HandleFunc("POST /api/repos", s.handleRepoAdd)
	mux.HandleFunc("DELETE /api/repos/{owner}/{name}", s.handleRepoRemove)
	mux.HandleFunc("GET /api/agents", s.handleAgentsList)
	mux.HandleFunc("POST /api/agents", s.handleAgentAdd)
	mux.HandleFunc("DELETE /api/agents/{name}", s.handleAgentRemove)
	mux.HandleFunc("POST /api/agents/{name}/default", s.handleAgentDefault)
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/skills", s.handleSkills)
	mux.HandleFunc("GET /api/reviews", s.handleSavedList)
	mux.HandleFunc("GET /api/reviews/{name}", s.handleSavedOne)

	return s.guard(mux)
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
		return fmt.Errorf("não consegui escutar em %s: %w", s.opts.Addr, err)
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
			http.Error(w, "host não permitido", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host {
					http.Error(w, "origem não permitida", http.StatusForbidden)
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
	})
}

func (s *Server) handlePRs(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("refresh") != ""
	prs, repoErrs, at, err := s.loadPRs(r.Context(), force)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	scope := r.URL.Query().Get("scope")
	author := strings.TrimPrefix(strings.TrimSpace(r.URL.Query().Get("author")), "@")

	now := time.Now()
	// O índice de reviews é relido a cada listagem: um review que terminou
	// em outra aba (ou no CLI) já aparece marcado aqui.
	marks := store.LoadMarks(s.reviewsDir)
	out := make([]prView, 0, len(prs))
	for _, pr := range prs {
		mine := strings.EqualFold(pr.Author.Login, s.me)
		if scope == "mine" && !mine {
			continue
		}
		if author != "" && !strings.EqualFold(pr.Author.Login, author) {
			continue
		}
		v := newPRView(pr, mine, now).withStatus(marks.Status(pr), now)
		v.BodyHTML = renderMarkdown(pr.Body)
		out = append(out, v)
	}

	errs := make([]map[string]string, 0, len(repoErrs))
	for _, re := range repoErrs {
		errs = append(errs, map[string]string{"repo": re.Repo, "error": re.Err.Error()})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"prs":         out,
		"repo_errors": errs,
		"fetched_at":  at,
	})
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
		writeErr(w, http.StatusBadRequest, fmt.Errorf("corpo inválido: %w", err))
		return
	}
	if len(req.Refs) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("nenhum PR informado"))
		return
	}

	s.cfgMu.Lock()
	choice, err := s.cfg.DefaultChoice(), error(nil)
	if strings.TrimSpace(req.Agent) != "" {
		choice, err = s.cfg.ChoiceByName(req.Agent)
	} else if len(choice.Steps) == 0 {
		err = config.ErrNoAgents
	}
	s.cfgMu.Unlock()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	// Os PRs em cache evitam um `gh pr view` por item quando vieram da lista.
	s.prMu.RLock()
	cached := map[string]gh.PR{}
	for _, pr := range s.prs {
		cached[pr.Key()] = pr
	}
	s.prMu.RUnlock()

	var (
		queued []jobView
		errs   []string
	)
	for _, ref := range req.Refs {
		repo, number, err := gh.ParseRef(ref)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		key := fmt.Sprintf("%s#%d", repo, number)
		pr, ok := cached[key]
		if !ok {
			pr, err = gh.Get(r.Context(), repo, number)
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

	status := http.StatusAccepted
	if len(queued) == 0 {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{"jobs": queued, "errors": errs})
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.jobViews()})
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	view, ok := s.jobs.View(r.PathValue("id"), true)
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("review não encontrado"))
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
			writeErr(w, http.StatusBadRequest, fmt.Errorf("from inválido: %q", v))
			return
		}
		from = n
	}
	view, ok := s.jobs.Log(r.PathValue("id"), from)
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("review não encontrado"))
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
	view, err := s.jobs.Post(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handlePublish manda o agente publicar no PR o review que o usuário leu.
// Vira um job novo — com passos e log — porque é outro agente rodando, e não
// uma chamada de API que o Bazel resolve sozinho.
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	view, err := s.jobs.PublishWithAgent(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, view)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming não suportado", http.StatusInternalServerError)
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
		writeErr(w, http.StatusBadRequest, fmt.Errorf("corpo inválido: %w", err))
		return
	}
	repo := strings.TrimSpace(req.Repo)
	if !strings.Contains(strings.TrimPrefix(repo, "https://github.com/"), "/") {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("repositório inválido: %q (use owner/repo)", repo))
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
		writeErr(w, http.StatusBadRequest, fmt.Errorf("corpo inválido: %w", err))
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
		writeErr(w, http.StatusBadRequest, fmt.Errorf("a skill %q não está instalada em %s", name, dir))
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
		writeErr(w, http.StatusNotFound, fmt.Errorf("o agente %q não está na lista", r.PathValue("name")))
		return
	}
	s.writeAgents(w, http.StatusOK, nil)
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
		writeErr(w, http.StatusNotFound, fmt.Errorf("não consegui ler %s — rode `bazel init`", s.configPath))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": s.configPath, "yaml": string(data)})
}

func (s *Server) handleSavedList(w http.ResponseWriter, r *http.Request) {
	entries, err := store.List(s.reviewsDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if entries == nil {
		entries = []store.Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"dir": s.reviewsDir, "reviews": entries})
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
		"html": renderMarkdown(body),
	})
}

// --- helpers ---

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
			"name":      name,
			"installed": skills.Has(instaladas, name),
			"step":      step.Name,
		})
	}
	return map[string]any{
		"name":        c.Name,
		"description": c.Description,
		"pipeline":    c.Pipeline,
		"posts":       c.Posts,
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
	list := skills.List(dir)
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
