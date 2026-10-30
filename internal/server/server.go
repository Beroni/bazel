// Package server serves Bazel's web interface.
//
// It is the same machinery as the CLI — config, gh, agent, workspace and store
// do not change — with HTTP in place of the terminal. The difference that
// matters: a review takes minutes, so the handler enqueues and answers right
// away, and the result reaches the browser through the stream of /api/events.
//
// It is single-user by construction: it uses the machine's
// already-authenticated `gh` and the same ~/.bazel/config.yaml as the CLI.
// That is why it listens on loopback and refuses requests coming from another
// origin — whoever gets in here orders repositories cloned and agents with a
// shell run.
package server

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
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
	"sync/atomic"
	"time"

	"github.com/beroni/bazel/internal/config"
	"github.com/beroni/bazel/internal/gh"
	"github.com/beroni/bazel/internal/skills"
	"github.com/beroni/bazel/internal/store"
)

//go:embed static
var staticFS embed.FS

// prTTL is how long the PR list is good for before querying gh again.
const prTTL = 60 * time.Second

// Options are the parameters of `bazel serve`.
type Options struct {
	Addr        string
	Concurrency int
	Keep        bool
	Version     string
}

// Server is the web interface.
type Server struct {
	cfg        *config.Config
	me         string
	reviewsDir string
	configPath string
	opts       Options
	hub        *Hub
	jobs       *Manager
	done       chan struct{}

	// instance is random per server process. Job ids carry its first
	// characters (see Manager.idPrefix), and the page uses it to tell a
	// restarted server from a dropped connection and to forget what it cached
	// under the old ids.
	instance string

	// cfgMu protects the config fields the interface edits (repos).
	cfgMu sync.Mutex

	// bound is the address Run actually listens on — it differs from opts.Addr
	// when the requested port is 0.
	bound atomic.Pointer[string]

	// fetchMu serializes the gh queries; prMu guards the cache.
	fetchMu sync.Mutex
	prMu    sync.RWMutex
	prs     []gh.PR
	prErrs  []gh.RepoError
	prsAt   time.Time
}

// New assembles the server. ctx owns the reviews in progress: cancel it and
// they die with it.
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
	instance, err := newInstanceID()
	if err != nil {
		return nil, err
	}
	jobs := NewManager(ctx, cfg, reviewsDir, opts.Concurrency, opts.Keep, hub)
	jobs.idPrefix = instance[:4] + "-"
	return &Server{
		instance:   instance,
		cfg:        cfg,
		me:         me,
		reviewsDir: reviewsDir,
		configPath: configPath,
		opts:       opts,
		hub:        hub,
		jobs:       jobs,
		done:       make(chan struct{}),
	}, nil
}

// newInstanceID makes the id that tells one server process from the next.
func newInstanceID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("could not generate the server instance id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Handler returns the routes already wrapped in the origin check.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// The assets go with no-store, like the index. They are embedded in the
	// binary: updating Bazel changes the js and the css, and a browser serving
	// the previous version against a new server is a bug that does not
	// reproduce on the machine of whoever wrote it. There is no bandwidth to
	// save here — it is loopback.
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
	mux.HandleFunc("POST /api/jobs/{id}/dismiss", s.handleDismiss)
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

	// /mcp goes through the same guard: it is the same port for cloning and
	// running an agent, just spoken by machine.
	mux.HandleFunc("/mcp", s.handleMCP)

	return secureHeaders(s.guard(s.sameInstance(mux)))
}

// instanceHeader carries the server instance the page was loaded from.
const instanceHeader = "X-Bazel-Instance"

// sameInstance refuses an action on a job sent by a page of another server
// process. The instance prefix already keeps an old id from naming a new job
// by accident, but this is what tells the page why it was refused, so it
// resyncs at once rather than showing a bare 404. The page sends the instance
// it knows on every write under /api/jobs/; a mismatch is 409 and nothing
// happens. A request without the header (MCP, curl, tests) goes through as
// before.
func (s *Server) sameInstance(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && strings.HasPrefix(r.URL.Path, "/api/jobs/") {
			if got := r.Header.Get(instanceHeader); got != "" && got != s.instance {
				writeJSON(w, http.StatusConflict, map[string]any{
					"error": "the server restarted — this page is stale; reload",
					"stale": true,
				})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// secureHeaders goes on every response, the guard's refusals included. The
// page must not load inside another site's frame: a hidden iframe on
// 127.0.0.1 keeps a local Host, passes the guard, and could walk a click to
// "Publish inline review" and its confirm (clickjacking). X-Frame-Options is
// for older browsers, frame-ancestors for the rest. nosniff keeps the browser
// to the declared types — every response here declares one. no-referrer keeps
// a click on a link from telling the other site this page's address.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// semCache tells the browser not to keep what comes embedded in the binary.
func semCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// Run starts the server and only returns when ctx dies.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Handler: s.Handler(),
		// No WriteTimeout: the SSE stays open for the whole review.
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("could not listen on %s: %w", s.opts.Addr, err)
	}
	addr := ln.Addr().String()
	s.bound.Store(&addr)

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

	// Release the SSE streams before Shutdown, otherwise it waits for them
	// until the end.
	close(s.done)
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutCtx)
}

// URL is the address to open in the browser.
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

// webUI is the page address, built from what the server listens on and not
// from the request's Host: outside loopback the Host does not pass through
// the guard, and whoever asks would pick the link they receive.
func (s *Server) webUI() string {
	addr := s.opts.Addr
	if b := s.bound.Load(); b != nil {
		addr = *b
	}
	return s.URL(addr) + "/"
}

// --- middleware ---

// guard shuts the door on third-party pages. A local server that clones
// repositories and starts agents cannot accept a POST fired by any site open
// in another tab, nor a forged Host pointing here.
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

// Exposed says the server listens beyond loopback: a network IP, 0.0.0.0,
// or an address without a host (`:7777`), which net.Listen opens on all
// interfaces. It differs from loopback(), which decides the Host check —
// `:7777` still only accepts a local Host, but the port is on the network.
func (s *Server) Exposed() bool {
	host, _, err := net.SplitHostPort(s.opts.Addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip == nil || !ip.IsLoopback()
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
		"instance":       s.instance,
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
		// An `agent` template that fires off a command on its own makes every
		// agent run two: the chosen one and the template's. The page warns,
		// because nothing in the result gives it away — only the spend.
		"stray_command": stray,
		// The Claude quota comes empty until the first review of this session.
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

// prFilter is the cut of the PR list. The page and /mcp ask for the same
// list; only the MCP filters by repository and review state, and only the
// page needs the description as HTML.
type prFilter struct {
	Refresh bool
	Mine    bool
	Author  string
	Repo    string
	// Status is "" (all), "unreviewed", "reviewed", "changed" or "posted".
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
	// The review index is reread on every listing: a review that finished in
	// another tab (or in the CLI) already shows up marked here.
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

// loadPRs queries gh, with a cache: each listing fires a `gh pr list` per
// repository and the page reloads more than the API deserves.
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

	// Another request may have fetched while we waited for the lock.
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
	// Agent is the name of the chosen agent or pipeline. Empty = the default.
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

// resolveChoice finds the agent or pipeline by name. Empty = the default.
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

// enqueueRefs queues one review per reference. What went wrong comes back
// in errs, one per item — a bad ref does not take down the others.
func (s *Server) enqueueRefs(ctx context.Context, refs []string, choice config.Choice) (queued []jobView, errs []string) {
	// Cached PRs avoid a `gh pr view` per item when they came from the list.
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

// handleJobLog delivers a review's log from a given point. The browser keeps
// the `next` from the previous response and only asks for what is missing —
// that is what lets the log move live without resending everything every
// second.
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

// handleJobRemove takes the job out of the session's queue. Cancels it first,
// if it is still alive.
func (s *Server) handleJobRemove(w http.ResponseWriter, r *http.Request) {
	if err := s.jobs.Remove(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": r.PathValue("id")})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.jobs.Cancel(r.PathValue("id")); err != nil {
		writeErr(w, refusalStatus(err, http.StatusConflict), err)
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
		// 502 is only for gh failing to post. A request Bazel turns down —
		// unknown job, not finished, not publishable, busy, already posted — is
		// the client's (BUG-QA-05).
		status := refusalStatus(err, http.StatusBadGateway)
		if errors.Is(err, errPublishBusy) || errors.Is(err, errAlreadyPosted) {
			status = http.StatusConflict
		}
		writeErr(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handleDismiss is "keep it here": the card stops waiting for you to publish,
// and nothing goes to the PR.
func (s *Server) handleDismiss(w http.ResponseWriter, r *http.Request) {
	view, err := s.jobs.Dismiss(r.PathValue("id"))
	if err != nil {
		writeErr(w, refusalStatus(err, http.StatusConflict), err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handlePublish sends the agent to publish on the PR the review the user read.
// It runs in the review's own card — with its own step and log, because it is
// another agent running — and not in a separate job: publishing is the end of
// the review, not a loose job in the queue.
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	skip, err := readSkip(w, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	view, err := s.jobs.PublishWithAgent(r.PathValue("id"), skip)
	if err != nil {
		// Unknown job is 404, like /post; every other refusal is 409.
		writeErr(w, refusalStatus(err, http.StatusConflict), err)
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

	// The stream opens with the instance id. EventSource reconnects by itself
	// after a restart, and this first event is how the page learns the job ids
	// it holds belong to a process that is gone.
	if hello, err := json.Marshal(event{Type: "hello", Data: map[string]string{"instance": s.instance}}); err == nil {
		fmt.Fprintf(w, "data: %s\n\n", hello)
		flusher.Flush()
	}

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
	// Exactly owner/name (BUG-QA-03): "acme/" used to go in as "acme", and a PR
	// URL as "acme/web-app/pull/7", because only the slash was checked.
	repo, err := config.NormalizeRepo(req.Repo)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	s.cfgMu.Lock()
	added := s.cfg.AddRepo(repo)
	err = s.cfg.Save()
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

// --- agents ---
//
// The agent list starts empty in a new config and is assembled here, from the
// skills Claude Code has installed on this machine: each agent is a skill
// running over the PR. That is why there is no factory list — it would point
// to skills this computer may never have had.

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

	// The description comes from the installed skill, not from the browser: it
	// is the skill's frontmatter that says what it does. A skill that is not
	// on the machine does not even get in — an agent like that would only fail
	// at run time.
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

// handleAgentDefault puts an agent at the front of the list — the first one is
// what runs when nobody chooses.
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

// handleAgentPublishable turns on or off sending an agent's output to the PR.
// It is the panel checkbox: not every skill produces a review, and one that
// does not has no reason to offer a publish button.
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

// handlePipelineAdd chains agents from the list into a sequence over the same
// clone. The steps come by agent name — the page only lets you choose among
// the ones that exist, and the config refuses the rest.
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

// handleConfigDownload delivers config.yaml as a file. It is what you take to
// another machine: repos, agents and pipelines are the whole configuration,
// and copying it is faster than rebuilding it click by click on the other
// side.
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

// writeAgents returns the whole list after changing it: it is what the page
// redraws, from the selector to the configuration.
func (s *Server) writeAgents(w http.ResponseWriter, status int, extra map[string]any) {
	out := map[string]any{"agents": s.agentViews()}
	for k, v := range extra {
		out[k] = v
	}
	writeJSON(w, status, out)
}

// summarize cuts a skill's description at the first sentence. A skill's
// frontmatter is a whole paragraph, with triggers and contraindications; the
// selector fits one line.
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

// savedView is a saved review plus what only the configuration can tell:
// whether what is in there can go to the PR.
type savedView struct {
	store.Entry
	Publishable bool `json:"publishable"`
}

// agentPublishes says whether an agent's output can go to the PR. An agent no
// longer in the list stays publishable: the review on disk outlives the
// configuration, and a name Bazel no longer recognizes is no reason to lock up
// a review that has already been read and approved.
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

// --- publishing a saved review ---
//
// A review outlives the server: it is in markdown on disk. The chance to take
// it to the PR has to outlive it too — before, closing Bazel before publishing
// left the text there and no way to use it.

// savedReview is a review read from disk, ready to go to the PR: the pull
// request it came from, where the file is, the body and who wrote it.
type savedReview struct {
	PR    gh.PR
	Path  string
	Body  string
	Agent string
}

// savedPR rereads a saved review and returns the PR it came from, along with
// the path, the body and the agent. The PR is fetched from GitHub: the file
// keeps the header, not its current state.
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

// refusePublish blocks what is not a review. It holds for the saved one as it
// does for the just-finished job: a PR's history does not become a comment on
// it.
func (s *Server) refusePublish(w http.ResponseWriter, sr savedReview) bool {
	if s.agentPublishes(sr.Agent) {
		return false
	}
	writeErr(w, http.StatusBadRequest, fmt.Errorf("what %q produces does not go to the PR", sr.Agent))
	return true
}

// handleSavedPublish runs the publishing agent over a saved review.
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

// handleSavedComment pastes a saved review onto the PR, with no agent.
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

// readSkip reads, from the body of a publish request, the findings the user
// unticked on screen — the indices renderReview numbered. An empty body is
// "publish everything": the four paths to the PR accept the list and none
// requires it.
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

// agentViews is the page's selector: the agents and pipelines of config.yaml,
// in the order they are there. The first is the default.
//
// Each one goes with the skill its task invokes and whether it is installed —
// an agent that calls a skill you do not have fails only at run time, and that
// is too late to find out.
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
	// The publishing agent is not in the selector, but it is an agent: whoever
	// opens the configuration wants to see it alongside.
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
			// An embedded skill is always available: it travels in the binary,
			// and there is nothing to install nor any way for it to be
			// missing.
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
		// publisher leaves the review selector: it runs later, at publication.
		"publisher": publisher,
	}
}

// handleSkills lists the skills installed on the machine — the ones that can
// become agents. It is the list the configuration builds its own from.
func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	dir, list := s.installedSkills()
	s.cfgMu.Lock()
	post := s.cfg.PostSkill()
	s.cfgMu.Unlock()
	// post_skill goes along so the page does not offer it as an agent: it only
	// publishes when it runs as the publish step.
	writeJSON(w, http.StatusOK, map[string]any{"dir": dir, "skills": list, "post_skill": post})
}

// installedSkills reads the skills from disk on every call: installing a skill
// should not require restarting Bazel.
func (s *Server) installedSkills() (string, []skills.Skill) {
	s.cfgMu.Lock()
	dir := strings.TrimSpace(s.cfg.SkillsDir)
	s.cfgMu.Unlock()
	if dir == "" {
		dir = skills.DefaultDir()
	}
	// The embedded ones join the list: they travel in the binary, are always
	// available, and it is from this list that the page lets you build agents
	// and pipelines. A disk skill with the same name loses — inside the clone,
	// what Claude Code will find is the project's, which is the embedded one.
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

// invalidatePRs forces the next listing to hit gh.
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
