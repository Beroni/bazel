package server

// O /mcp é o Bazel falado por máquina: um servidor MCP no transporte
// "streamable HTTP", na mesma porta e atrás do mesmo guard da página. Um
// agente externo — uma sessão do Claude Code, um bot de mensagens, uma rotina
// agendada — lista PRs, enfileira review, acompanha o job e lê o resultado. O
// review continua vivendo aqui, e a página continua sendo a única interface
// humana: todo resultado aponta para ela.
//
// A superfície é curada, não a API inteira: ler e enfileirar. Publicar no PR
// fica de fora a menos que o config diga `mcp_allow_publish: true` — e isso
// vale também para escolher um agente que publica sozinho, que seria a mesma
// porta pelo lado.
//
// É sem sessão: cada POST traz uma mensagem JSON-RPC e leva a resposta em
// JSON. Não há o que o servidor precise empurrar sem ser perguntado, então o
// GET (o stream de notificações) não é oferecido — o spec permite.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/beroni/bazel/internal/store"
)

// mcpVersions são as versões do protocolo que este servidor fala, da mais
// nova para a mais velha. O que muda entre elas não toca o que usamos.
var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// mcpMaxWait é o teto do job_status com espera: segura a requisição, mas não
// a ponto de o cliente desistir dela.
const mcpMaxWait = 60

const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if v := r.Header.Get("Mcp-Protocol-Version"); v != "" && !slices.Contains(mcpVersions, v) {
		http.Error(w, fmt.Sprintf("unsupported MCP protocol version %q", v), http.StatusBadRequest)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeRPC(w, http.StatusBadRequest, rpcFail(nil, rpcParseError, "could not read body"))
		return
	}
	data = []byte(strings.TrimSpace(string(data)))

	// Lote é coisa da versão 2025-03-26; as seguintes tiraram. Aceitar não
	// custa nada e não deixa um cliente mais velho na mão.
	if len(data) > 0 && data[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(data, &batch); err != nil || len(batch) == 0 {
			writeRPC(w, http.StatusBadRequest, rpcFail(nil, rpcParseError, "invalid JSON-RPC batch"))
			return
		}
		var out []rpcResponse
		for _, raw := range batch {
			if resp, ok := s.mcpMessage(r, raw); ok {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeRPC(w, http.StatusOK, out)
		return
	}

	resp, ok := s.mcpMessage(r, data)
	if !ok {
		// Notificação ou resposta do cliente: nada a devolver.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	status := http.StatusOK
	if resp.Error != nil && (resp.Error.Code == rpcParseError || resp.Error.Code == rpcInvalidRequest) {
		status = http.StatusBadRequest
	}
	writeRPC(w, status, resp)
}

// mcpMessage trata uma mensagem. ok=false quando ela não pede resposta.
func (s *Server) mcpMessage(r *http.Request, raw json.RawMessage) (rpcResponse, bool) {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return rpcFail(nil, rpcParseError, "invalid JSON"), true
	}
	notification := len(req.ID) == 0 || string(req.ID) == "null"
	if req.Method == "" {
		if notification {
			return rpcFail(nil, rpcInvalidRequest, "missing method"), true
		}
		// Resposta a algo que nunca perguntamos: ignora.
		return rpcResponse{}, false
	}
	if req.JSONRPC != "2.0" {
		if notification {
			return rpcResponse{}, false
		}
		return rpcFail(req.ID, rpcInvalidRequest, `jsonrpc must be "2.0"`), true
	}
	if notification {
		return rpcResponse{}, false
	}

	result, rerr := s.mcpDispatch(r, req)
	if rerr != nil {
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: rerr}, true
	}
	return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}, true
}

func (s *Server) mcpDispatch(r *http.Request, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := mcpVersions[0]
		if slices.Contains(mcpVersions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "bazel", "version": s.opts.Version},
			"instructions": "Bazel runs code reviews on GitHub PRs with a fleet of agents. " +
				"Enqueue a review, poll job_status until it is done, then read the result. " +
				"Reviews are published to the PR by a human from the web UI (web_ui in every result).",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.mcpTools()}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{Code: rpcInvalidParams, Message: "invalid params"}
		}
		tool, ok := s.mcpTool(p.Name)
		if !ok {
			return nil, &rpcError{Code: rpcInvalidParams, Message: fmt.Sprintf("unknown tool %q", p.Name)}
		}
		args := p.Arguments
		if len(args) == 0 || string(args) == "null" {
			args = json.RawMessage("{}")
		}
		out, err := tool.call(r.Context(), args)
		if err != nil {
			// Falha da ferramenta vai no resultado, não como erro de protocolo:
			// é o que deixa o modelo do outro lado ler e corrigir o pedido.
			return map[string]any{
				"content": []map[string]any{{"type": "text", "text": err.Error()}},
				"isError": true,
			}, nil
		}
		out["web_ui"] = webUI(r)
		text, _ := json.MarshalIndent(out, "", "  ")
		return map[string]any{
			"content":           []map[string]any{{"type": "text", "text": string(text)}},
			"structuredContent": out,
		}, nil
	}
	return nil, &rpcError{Code: rpcMethodNotFound, Message: fmt.Sprintf("method %q not found", req.Method)}
}

// webUI é o endereço da página para quem está do outro lado. Sai do Host que
// o guard já aceitou — é por onde o cliente chegou aqui.
func webUI(r *http.Request) string {
	return "http://" + r.Host + "/"
}

func (s *Server) allowPublish() bool {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return s.cfg.MCPAllowPublish
}

// --- ferramentas ---

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`

	call func(ctx context.Context, args json.RawMessage) (map[string]any, error)
}

func (s *Server) mcpTools() []mcpToolDef {
	tools := []mcpToolDef{
		{
			Name:        "list_prs",
			Description: "List open pull requests in the watched repositories, with whether each was already reviewed by Bazel.",
			InputSchema: schema(map[string]any{
				"repo":    prop("string", "Only PRs of this repository (owner/repo)."),
				"mine":    prop("boolean", "Only PRs authored by the Bazel user."),
				"author":  prop("string", "Only PRs by this GitHub login."),
				"status":  enumProp("Filter by review state.", "any", "unreviewed", "reviewed", "changed", "posted"),
				"refresh": prop("boolean", "Bypass the 60s cache and query GitHub now."),
			}),
			Annotations: readOnly("List PRs"),
			call:        s.mcpListPRs,
		},
		{
			Name: "enqueue_review",
			Description: "Queue a review of one or more PRs. Returns the job ids; follow them with job_status. " +
				"The result is saved to disk and shown in the web UI for a human to read and publish.",
			InputSchema: schema(map[string]any{
				"refs": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"minItems":    1,
					"description": "PRs as owner/repo#123 or https://github.com/owner/repo/pull/123.",
				},
				"agent": prop("string", "Agent or pipeline name (see list_agents). Empty = the default."),
			}, "refs"),
			Annotations: map[string]any{"title": "Enqueue review", "readOnlyHint": false, "destructiveHint": false, "openWorldHint": true},
			call:        s.mcpEnqueue,
		},
		{
			Name:        "list_jobs",
			Description: "List the review jobs of this server session, newest first.",
			InputSchema: schema(map[string]any{}),
			Annotations: readOnly("List jobs"),
			call: func(context.Context, json.RawMessage) (map[string]any, error) {
				return map[string]any{"jobs": s.jobViews()}, nil
			},
		},
		{
			Name: "job_status",
			Description: "State of one review job; when it is done, includes the review body. " +
				"wait_seconds blocks until the job leaves queued/running or the wait runs out.",
			InputSchema: schema(map[string]any{
				"id":           prop("string", "Job id from enqueue_review or list_jobs."),
				"wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": mcpMaxWait, "description": "Long-poll up to this many seconds."},
			}, "id"),
			Annotations: readOnly("Job status"),
			call:        s.mcpJobStatus,
		},
		{
			Name:        "job_log",
			Description: "Agent log lines of a job from sequence number `from` on. Pass the returned `next` to get only what is new.",
			InputSchema: schema(map[string]any{
				"id":   prop("string", "Job id."),
				"from": map[string]any{"type": "integer", "minimum": 0, "description": "First sequence number to return."},
			}, "id"),
			Annotations: readOnly("Job log"),
			call:        s.mcpJobLog,
		},
		{
			Name:        "list_reviews",
			Description: "List the reviews saved to disk, newest first — they outlive the server session.",
			InputSchema: schema(map[string]any{
				"repo": prop("string", "Only reviews of this repository (owner/repo)."),
			}),
			Annotations: readOnly("List reviews"),
			call:        s.mcpListReviews,
		},
		{
			Name:        "read_review",
			Description: "Read a saved review (markdown) by file name, as given by list_reviews or a job's saved_to.",
			InputSchema: schema(map[string]any{
				"name": prop("string", "File name of the review, e.g. owner-repo-123-20260101-120000.md."),
			}, "name"),
			Annotations: readOnly("Read review"),
			call:        s.mcpReadReview,
		},
		{
			Name:        "list_agents",
			Description: "List the agents and pipelines a review can run with. The first is the default.",
			InputSchema: schema(map[string]any{}),
			Annotations: readOnly("List agents"),
			call: func(context.Context, json.RawMessage) (map[string]any, error) {
				return map[string]any{"agents": s.agentViews(), "mcp_allow_publish": s.allowPublish()}, nil
			},
		},
	}
	if s.allowPublish() {
		tools = append(tools, mcpToolDef{
			Name: "publish_review",
			Description: "Publish a finished review job to its PR through the post agent. " +
				"Enabled by mcp_allow_publish in the Bazel config: only publish what a human has read.",
			InputSchema: schema(map[string]any{
				"id": prop("string", "Job id of a finished review."),
				"skip": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "integer", "minimum": 0},
					"description": "Indexes of findings to leave out.",
				},
			}, "id"),
			Annotations: map[string]any{"title": "Publish review", "readOnlyHint": false, "destructiveHint": false, "openWorldHint": true},
			call:        s.mcpPublish,
		})
	}
	return tools
}

func (s *Server) mcpTool(name string) (mcpToolDef, bool) {
	for _, t := range s.mcpTools() {
		if t.Name == name {
			return t, true
		}
	}
	return mcpToolDef{}, false
}

func (s *Server) mcpListPRs(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	var a struct {
		Repo    string `json:"repo"`
		Mine    bool   `json:"mine"`
		Author  string `json:"author"`
		Status  string `json:"status"`
		Refresh bool   `json:"refresh"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	switch a.Status {
	case "", "any", "unreviewed", "reviewed", "changed", "posted":
	default:
		return nil, fmt.Errorf("invalid status %q", a.Status)
	}
	list, err := s.listPRs(ctx, prFilter{
		Refresh: a.Refresh, Mine: a.Mine, Author: a.Author, Repo: a.Repo, Status: a.Status,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"prs": list.PRs, "repo_errors": list.RepoErrors, "fetched_at": list.FetchedAt}, nil
}

func (s *Server) mcpEnqueue(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	var a struct {
		Refs  []string `json:"refs"`
		Agent string   `json:"agent"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if len(a.Refs) == 0 {
		return nil, errors.New("no PR given")
	}
	choice, err := s.resolveChoice(a.Agent)
	if err != nil {
		return nil, err
	}
	// Um agente que publica sozinho é publicar por outro caminho: sem a chave
	// no config, ele não roda a pedido de quem chega pelo /mcp.
	if choice.Posts && !s.allowPublish() {
		return nil, fmt.Errorf("%q publishes to the PR on its own, and publishing over MCP is disabled "+
			"(set mcp_allow_publish: true in the Bazel config to allow it)", choice.Name)
	}
	queued, errs := s.enqueueRefs(ctx, a.Refs, choice)
	if len(queued) == 0 {
		return nil, fmt.Errorf("nothing was queued: %s", strings.Join(errs, "; "))
	}
	if errs == nil {
		errs = []string{}
	}
	return map[string]any{"jobs": queued, "errors": errs}, nil
}

func (s *Server) mcpJobStatus(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	var a struct {
		ID   string `json:"id"`
		Wait int    `json:"wait_seconds"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	wait := min(max(a.Wait, 0), mcpMaxWait)
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	for {
		view, ok := s.jobs.View(a.ID, true)
		if !ok {
			return nil, fmt.Errorf("job %q not found", a.ID)
		}
		live := view.State == StateQueued || view.State == StateRunning
		if !live || !time.Now().Before(deadline) {
			// O HTML é para a página; o corpo em markdown já é o review.
			view.HTML = ""
			return map[string]any{"job": view}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.done:
			return nil, errors.New("server is shutting down")
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *Server) mcpJobLog(_ context.Context, raw json.RawMessage) (map[string]any, error) {
	var a struct {
		ID   string `json:"id"`
		From int    `json:"from"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.From < 0 {
		return nil, fmt.Errorf("invalid from: %d", a.From)
	}
	view, ok := s.jobs.Log(a.ID, a.From)
	if !ok {
		return nil, fmt.Errorf("job %q not found", a.ID)
	}
	return map[string]any{"lines": view.Lines, "next": view.Next, "dropped": view.Dropped, "live": view.Live}, nil
}

func (s *Server) mcpListReviews(_ context.Context, raw json.RawMessage) (map[string]any, error) {
	var a struct {
		Repo string `json:"repo"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	entries, err := store.List(s.reviewsDir)
	if err != nil {
		return nil, err
	}
	out := make([]savedView, 0, len(entries))
	for _, e := range entries {
		if a.Repo != "" && !strings.EqualFold(e.Repo, strings.TrimSpace(a.Repo)) {
			continue
		}
		out = append(out, savedView{Entry: e, Publishable: s.agentPublishes(e.Agent)})
	}
	return map[string]any{"reviews": out}, nil
}

func (s *Server) mcpReadReview(_ context.Context, raw json.RawMessage) (map[string]any, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	body, err := store.Read(s.reviewsDir, strings.TrimSpace(a.Name))
	if err != nil {
		return nil, err
	}
	return map[string]any{"name": a.Name, "body": body}, nil
}

func (s *Server) mcpPublish(_ context.Context, raw json.RawMessage) (map[string]any, error) {
	// A lista de ferramentas já esconde esta; a checagem aqui é o que segura
	// uma chamada feita de memória depois de a chave ser desligada.
	if !s.allowPublish() {
		return nil, errors.New("publishing over MCP is disabled")
	}
	var a struct {
		ID   string `json:"id"`
		Skip []int  `json:"skip"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	for _, i := range a.Skip {
		if i < 0 {
			return nil, fmt.Errorf("invalid finding index %d", i)
		}
	}
	view, err := s.jobs.PublishWithAgent(a.ID, a.Skip)
	if err != nil {
		return nil, err
	}
	return map[string]any{"job": view}, nil
}

// --- helpers ---

func decodeArgs(raw json.RawMessage, v any) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func schema(props map[string]any, required ...string) map[string]any {
	out := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

func enumProp(desc string, values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values, "description": desc}
}

func readOnly(title string) map[string]any {
	return map[string]any{"title": title, "readOnlyHint": true, "openWorldHint": false}
}

func rpcFail(id json.RawMessage, code int, msg string) rpcResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func writeRPC(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}
