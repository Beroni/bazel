package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beroni/bazel/internal/config"
	"github.com/beroni/bazel/internal/gh"
)

// mcpCall sends a JSON-RPC message to /mcp and returns the status and body.
func mcpCall(t *testing.T, h http.Handler, body string, mod ...func(*http.Request)) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest("POST", "http://127.0.0.1:7777/mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	for _, m := range mod {
		m(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &out)
	}
	return w.Code, out
}

// mcpTool calls a tool and returns the structuredContent, or fails the test if
// it returned an error.
func mcpToolCall(t *testing.T, h http.Handler, name string, args any) map[string]any {
	t.Helper()
	res := mcpToolResult(t, h, name, args)
	if res["isError"] == true {
		t.Fatalf("%s devolveu erro: %v", name, res["content"])
	}
	sc, _ := res["structuredContent"].(map[string]any)
	return sc
}

func mcpToolResult(t *testing.T, h http.Handler, name string, args any) map[string]any {
	t.Helper()
	a, _ := json.Marshal(args)
	code, out := mcpCall(t, h, fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, a))
	if code != http.StatusOK || out["error"] != nil {
		t.Fatalf("tools/call %s: status %d, %v", name, code, out)
	}
	res, _ := out["result"].(map[string]any)
	return res
}

func toolNames(t *testing.T, h http.Handler) []string {
	t.Helper()
	_, out := mcpCall(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	res, _ := out["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	var names []string
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	return names
}

// This is what `claude mcp add --transport http` does when registering:
// initialize, the ready notification and the tool list.
func TestMCPHandshakeAndDiscovery(t *testing.T) {
	h := newTestServer(t).Handler()

	code, out := mcpCall(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	if code != http.StatusOK {
		t.Fatalf("initialize: status %d", code)
	}
	res := out["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" {
		t.Errorf("versão negociada = %v", res["protocolVersion"])
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Errorf("sem capability de tools: %v", res["capabilities"])
	}

	if code, _ := mcpCall(t, h, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted {
		t.Errorf("notificação devia dar 202, deu %d", code)
	}

	names := toolNames(t, h)
	for _, want := range []string{"list_prs", "enqueue_review", "list_jobs", "job_status", "job_log", "list_reviews", "read_review", "list_agents"} {
		if !contains(names, want) {
			t.Errorf("ferramenta %s não foi anunciada: %v", want, names)
		}
	}
	if contains(names, "publish_review") {
		t.Error("publish_review não pode aparecer sem mcp_allow_publish")
	}

	// Unknown version: the server offers its own.
	_, out = mcpCall(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if v := out["result"].(map[string]any)["protocolVersion"]; v != mcpVersions[0] {
		t.Errorf("versão desconhecida devia cair na %s, caiu em %v", mcpVersions[0], v)
	}
}

func TestMCPProtocolErrors(t *testing.T) {
	h := newTestServer(t).Handler()

	_, out := mcpCall(t, h, `{"jsonrpc":"2.0","id":3,"method":"resources/list"}`)
	if e, _ := out["error"].(map[string]any); e == nil || e["code"].(float64) != rpcMethodNotFound {
		t.Errorf("método desconhecido devia dar -32601: %v", out)
	}
	if code, _ := mcpCall(t, h, `{nope`); code != http.StatusBadRequest {
		t.Errorf("JSON inválido devia dar 400, deu %d", code)
	}
	res := mcpToolResult(t, h, "job_status", map[string]any{"id": "nao-existe"})
	if res["isError"] != true {
		t.Errorf("job inexistente devia voltar como erro da ferramenta: %v", res)
	}
	_, out = mcpCall(t, h, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"rm_rf","arguments":{}}}`)
	if out["error"] == nil {
		t.Errorf("ferramenta desconhecida devia dar erro de protocolo: %v", out)
	}

	r := httptest.NewRequest("GET", "http://127.0.0.1:7777/mcp", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /mcp devia dar 405, deu %d", w.Code)
	}

	code, _ := mcpCall(t, h, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, func(r *http.Request) {
		r.Header.Set("MCP-Protocol-Version", "1999-01-01")
	})
	if code != http.StatusBadRequest {
		t.Errorf("versão de protocolo desconhecida no header devia dar 400, deu %d", code)
	}
}

// withLens gives the server a named agent. The `agent` block alone, which is
// what newTestServer leaves, is refused by /mcp without the publish
// prohibition.
func withLens(srv *Server) *Server {
	srv.cfgMu.Lock()
	srv.cfg.Agents = []config.AgentDef{{Name: "lente", Task: "revise"}}
	srv.cfgMu.Unlock()
	return srv
}

// The /mcp makes the server clone and run agents just as the page does: the
// guard applies equally.
func TestMCPBehindGuard(t *testing.T) {
	h := newTestServer(t).Handler()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	if code, _ := mcpCall(t, h, body, func(r *http.Request) { r.Host = "evil.com" }); code != http.StatusForbidden {
		t.Errorf("Host forjado devia dar 403, deu %d", code)
	}
	if code, _ := mcpCall(t, h, body, func(r *http.Request) { r.Header.Set("Origin", "https://evil.com") }); code != http.StatusForbidden {
		t.Errorf("Origin de terceiro devia dar 403, deu %d", code)
	}
}

// web_ui comes from the address the server listens on, never from the Host of
// whoever asks — and, after Run, from the port it actually got.
func TestMCPWebUIIgnoresHost(t *testing.T) {
	srv := withLens(newTestServer(t))
	srv.prs = []gh.PR{testPR(482)}
	h := srv.Handler()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_agents","arguments":{}}}`
	_, out := mcpCall(t, h, body, func(r *http.Request) { r.Host = "localhost:6666" })
	got := out["result"].(map[string]any)["structuredContent"].(map[string]any)["web_ui"]
	if got != "http://127.0.0.1:0/" {
		t.Errorf("web_ui = %v, queria o endereço configurado e não o Host da requisição", got)
	}

	bound := "127.0.0.1:54321"
	srv.bound.Store(&bound)
	if got := srv.webUI(); got != "http://127.0.0.1:54321/" {
		t.Errorf("depois do Run, web_ui devia ter a porta real: %q", got)
	}
}

// The whole path of an external agent: enqueue, wait, read the review.
func TestMCPEnqueueAndPollToCompletion(t *testing.T) {
	srv := withLens(newTestServer(t))
	srv.prs = []gh.PR{testPR(482)}
	h := srv.Handler()

	out := mcpToolCall(t, h, "enqueue_review", map[string]any{"refs": []string{"acme/api-core#482"}})
	jobs, _ := out["jobs"].([]any)
	if len(jobs) != 1 {
		t.Fatalf("esperava 1 job, veio %v", out)
	}
	if got, want := out["web_ui"], srv.webUI(); got != want || !strings.HasPrefix(want, "http://127.0.0.1:") {
		t.Errorf("web_ui = %v, queria o endereço do servidor %q", got, want)
	}
	id := jobs[0].(map[string]any)["id"].(string)

	var job map[string]any
	for range 20 {
		job = mcpToolCall(t, h, "job_status", map[string]any{"id": id, "wait_seconds": 5})["job"].(map[string]any)
		if job["state"] != string(StateQueued) && job["state"] != string(StateRunning) {
			break
		}
	}
	if job["state"] != string(StateDone) {
		t.Fatalf("job não terminou: %v", job)
	}
	if !strings.Contains(job["body"].(string), "482") {
		t.Errorf("o corpo do review devia vir no job terminado: %q", job["body"])
	}
	if _, ok := job["html"]; ok {
		t.Error("o HTML é da página, não do MCP")
	}

	saved := job["saved_to"].(string)
	name := saved[strings.LastIndexAny(saved, `/\`)+1:]
	review := mcpToolCall(t, h, "read_review", map[string]any{"name": name})
	if !strings.Contains(review["body"].(string), "482") {
		t.Errorf("read_review não trouxe o review: %v", review)
	}
	list := mcpToolCall(t, h, "list_reviews", map[string]any{"repo": "acme/api-core"})
	if rs, _ := list["reviews"].([]any); len(rs) != 1 {
		t.Errorf("list_reviews devia trazer o review salvo: %v", list)
	}

	logs := mcpToolCall(t, h, "job_log", map[string]any{"id": id})
	if logs["live"] != false {
		t.Errorf("log de job terminado não está vivo: %v", logs)
	}
}

// Publishing over MCP is impossible without the key — neither through the tool
// nor through the shortcut of choosing an agent that publishes on its own.
func TestMCPPublishGate(t *testing.T) {
	t.Setenv("BAZEL_HOME", t.TempDir())
	cfg := cfgFor(t, "cat")
	cfg.Agents = []config.AgentDef{
		{Name: "lente", Task: "revise"},
		{Name: "poster", Task: "revise e publique", Posts: true},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := New(ctx, cfg, "beroni", Options{Addr: "127.0.0.1:0", Concurrency: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.prs = []gh.PR{testPR(482)}
	h := srv.Handler()

	res := mcpToolResult(t, h, "enqueue_review", map[string]any{"refs": []string{"acme/api-core#482"}, "agent": "poster"})
	if res["isError"] != true {
		t.Fatalf("agente que publica sozinho devia ser recusado: %v", res)
	}
	if got := srv.jobs.Snapshot(); len(got) != 0 {
		t.Errorf("nada devia ter entrado na fila: %v", got)
	}
	_, out := mcpCall(t, h, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"publish_review","arguments":{"id":"x"}}}`)
	if out["error"] == nil {
		t.Errorf("publish_review não pode existir com a chave desligada: %v", out)
	}

	srv.cfgMu.Lock()
	srv.cfg.MCPAllowPublish = true
	srv.cfgMu.Unlock()
	if !contains(toolNames(t, h), "publish_review") {
		t.Error("com mcp_allow_publish, publish_review devia aparecer")
	}
	res = mcpToolResult(t, h, "enqueue_review", map[string]any{"refs": []string{"acme/api-core#482"}, "agent": "poster"})
	if res["isError"] == true {
		t.Errorf("com a chave ligada o agente que publica devia rodar: %v", res["content"])
	}
}

// gateServer starts a server with the lente and poster agents (the latter
// publishes on its own), after mod tweaks the config.
func gateServer(t *testing.T, cmd string, mod func(*config.Config), args ...string) *Server {
	t.Helper()
	t.Setenv("BAZEL_HOME", t.TempDir())
	cfg := cfgFor(t, cmd, args...)
	cfg.Agents = []config.AgentDef{
		{Name: "lente", Task: "revise"},
		{Name: "poster", Task: "revise e publique", Posts: true},
	}
	if mod != nil {
		mod(cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := New(ctx, cfg, "beroni", Options{Addr: "127.0.0.1:0", Concurrency: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.prs = []gh.PR{testPR(482)}
	return srv
}

// The other paths to publishing without the switch: a pipeline with the
// `publish` step, one that includes an agent that publishes, and a default
// that publishes.
func TestMCPPublishGateOtherDoors(t *testing.T) {
	ref := map[string]any{"refs": []string{"acme/api-core#482"}}
	cases := []struct {
		nome  string
		mod   func(*config.Config)
		agent string
	}{
		{"pipeline com passo publish", func(c *config.Config) {
			c.Pipelines = []config.Pipeline{{Name: "frota", Steps: []string{"lente", "publish"}}}
		}, "frota"},
		{"pipeline com agente que publica", func(c *config.Config) {
			c.Pipelines = []config.Pipeline{{Name: "frota", Steps: []string{"lente", "poster"}}}
		}, "frota"},
		{"padrão que publica, sem agent", func(c *config.Config) {
			c.Default = "poster"
		}, ""},
		{"padrão é pipeline com publish, sem agent", func(c *config.Config) {
			c.Pipelines = []config.Pipeline{{Name: "frota", Steps: []string{"lente", "publish"}}}
			c.Default = "frota"
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.nome, func(t *testing.T) {
			srv := gateServer(t, "cat", c.mod)
			h := srv.Handler()
			args := map[string]any{"refs": ref["refs"]}
			if c.agent != "" {
				args["agent"] = c.agent
			}
			res := mcpToolResult(t, h, "enqueue_review", args)
			if res["isError"] != true {
				t.Fatalf("devia ser recusado sem mcp_allow_publish: %v", res)
			}
			if got := srv.jobs.Snapshot(); len(got) != 0 {
				t.Errorf("nada devia ter entrado na fila: %v", got)
			}
		})
	}
}

// Publishing without having declared posts is also publishing: a task with
// --post, a prompt that authorizes it, and the `agent` block alone, whose
// prompt Bazel did not write. Without the key, /mcp refuses all of them; with
// the default template's prohibition, the `agent` block passes.
func TestMCPPublishGateUndeclared(t *testing.T) {
	ref := []string{"acme/api-core#482"}
	cases := []struct {
		nome   string
		mod    func(*config.Config)
		agent  string
		recusa bool
	}{
		{"task com --post sem posts", func(c *config.Config) {
			c.Agents = append(c.Agents, config.AgentDef{Name: "furtivo", Task: "/review-fleet {{number}} --post"})
		}, "furtivo", true},
		{"prompt que autoriza publicar", func(c *config.Config) {
			c.Agents = append(c.Agents, config.AgentDef{Name: "furtivo", Task: "revise",
				Prompt: "{{task}}\nYou are authorized to publish on PR #{{number}}."})
		}, "furtivo", true},
		{"pipeline com passo que passa --post", func(c *config.Config) {
			c.Agents = append(c.Agents, config.AgentDef{Name: "furtivo", Task: "/x --post"})
			c.Pipelines = []config.Pipeline{{Name: "frota", Steps: []string{"lente", "furtivo"}}}
		}, "frota", true},
		{"bloco agent sem a proibição", func(c *config.Config) {
			c.Agents = nil
		}, "", true},
		{"bloco agent com a proibição", func(c *config.Config) {
			c.Agents = nil
			c.Agent.Prompt = "revise o PR {{number}}.\nDo not publish\nanything to GitHub."
		}, "", false},
	}
	for _, c := range cases {
		t.Run(c.nome, func(t *testing.T) {
			srv := gateServer(t, "cat", c.mod)
			h := srv.Handler()
			args := map[string]any{"refs": ref}
			if c.agent != "" {
				args["agent"] = c.agent
			}
			res := mcpToolResult(t, h, "enqueue_review", args)
			if got := res["isError"] == true; got != c.recusa {
				t.Fatalf("recusado = %v, queria %v: %v", got, c.recusa, res["content"])
			}
			if c.recusa && len(srv.jobs.Snapshot()) != 0 {
				t.Errorf("nada devia ter entrado na fila: %v", srv.jobs.Snapshot())
			}
		})
	}
}

// With the switch on, publish_review still only publishes what finished,
// exists and has valid indices.
func TestMCPPublishReviewGuards(t *testing.T) {
	allow := func(c *config.Config) { c.MCPAllowPublish = true }

	t.Run("id inexistente", func(t *testing.T) {
		h := gateServer(t, "cat", allow).Handler()
		res := mcpToolResult(t, h, "publish_review", map[string]any{"id": "nao-existe"})
		if res["isError"] != true {
			t.Errorf("job inexistente devia ser erro: %v", res)
		}
	})

	t.Run("job em execução", func(t *testing.T) {
		srv := gateServer(t, "sh", allow, "-c", "sleep 30")
		h := srv.Handler()
		out := mcpToolCall(t, h, "enqueue_review", map[string]any{"refs": []string{"acme/api-core#482"}, "agent": "lente"})
		id := out["jobs"].([]any)[0].(map[string]any)["id"].(string)
		res := mcpToolResult(t, h, "publish_review", map[string]any{"id": id})
		if res["isError"] != true || !strings.Contains(fmt.Sprint(res["content"]), "not finished") {
			t.Errorf("job que não terminou devia ser recusado: %v", res)
		}
	})

	t.Run("skip negativo", func(t *testing.T) {
		srv := gateServer(t, "cat", allow)
		h := srv.Handler()
		out := mcpToolCall(t, h, "enqueue_review", map[string]any{"refs": []string{"acme/api-core#482"}, "agent": "lente"})
		id := out["jobs"].([]any)[0].(map[string]any)["id"].(string)
		for range 20 {
			job := mcpToolCall(t, h, "job_status", map[string]any{"id": id, "wait_seconds": 5})["job"].(map[string]any)
			if job["state"] == string(StateDone) {
				break
			}
		}
		res := mcpToolResult(t, h, "publish_review", map[string]any{"id": id, "skip": []int{-1}})
		if res["isError"] != true || !strings.Contains(fmt.Sprint(res["content"]), "invalid finding index") {
			t.Errorf("skip [-1] devia ser recusado pelo índice: %v", res)
		}
	})
}

// mcpRaw sends the raw body to /mcp and returns the status and body undecoded
// — a batch comes back as an array, and mcpCall only knows objects.
func mcpRaw(t *testing.T, h http.Handler, body string) (int, []byte) {
	t.Helper()
	r := httptest.NewRequest("POST", "http://127.0.0.1:7777/mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

// rpcErrorCode returns the error code of a JSON-RPC response, or 0.
func rpcErrorCode(resp map[string]any) float64 {
	e, _ := resp["error"].(map[string]any)
	if e == nil {
		return 0
	}
	code, _ := e["code"].(float64)
	return code
}

// JSON that does not parse is -32700; valid JSON that is not a request is
// -32600. Mixing the two makes the client blame the transport for its own error.
func TestMCPJSONRPCErrorCodes(t *testing.T) {
	h := newTestServer(t).Handler()

	single := []struct {
		nome string
		body string
		want float64
	}{
		{"JSON inválido", `{nope`, rpcParseError},
		{"JSON truncado", `{"jsonrpc":"2.0","id":1,`, rpcParseError},
		{"method com tipo errado", `{"jsonrpc":"2.0","id":1,"method":5}`, rpcInvalidRequest},
		{"corpo que não é objeto", `42`, rpcInvalidRequest},
		{"sem method nem id", `{"jsonrpc":"2.0"}`, rpcInvalidRequest},
		{"lote vazio", `[]`, rpcInvalidRequest},
		{"lote com JSON inválido", `[{"jsonrpc":"2.0","id":1,"method":"ping"},`, rpcParseError},
	}
	for _, c := range single {
		code, raw := mcpRaw(t, h, c.body)
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Errorf("%s: devia dar uma única resposta objeto, veio %s", c.nome, raw)
			continue
		}
		if got := rpcErrorCode(out); got != c.want {
			t.Errorf("%s: código = %v, queria %v (%s)", c.nome, got, c.want, raw)
		}
		if code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, queria 400", c.nome, code)
		}
	}

	// Batch with a non-object item: only that item fails.
	code, raw := mcpRaw(t, h, `[1,{"jsonrpc":"2.0","id":9,"method":"ping"}]`)
	var batch []map[string]any
	if err := json.Unmarshal(raw, &batch); err != nil || len(batch) != 2 {
		t.Fatalf("lote devia voltar 2 respostas, veio %s", raw)
	}
	if code != http.StatusOK {
		t.Errorf("lote com item ruim devia dar 200, deu %d", code)
	}
	if got := rpcErrorCode(batch[0]); got != rpcInvalidRequest {
		t.Errorf("item [1] devia dar -32600, deu %v: %v", got, batch[0])
	}
	if batch[1]["error"] != nil || batch[1]["result"] == nil {
		t.Errorf("o ping ao lado devia passar: %v", batch[1])
	}

	_, raw = mcpRaw(t, h, `[1]`)
	if err := json.Unmarshal(raw, &batch); err != nil || len(batch) != 1 || rpcErrorCode(batch[0]) != rpcInvalidRequest {
		t.Errorf("[1] devia dar uma resposta -32600, veio %s", raw)
	}

	// Batch above the ceiling: refused whole, without running any item.
	items := make([]string, mcpMaxBatch+1)
	for i := range items {
		items[i] = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"ping"}`, i)
	}
	code, raw = mcpRaw(t, h, "["+strings.Join(items, ",")+"]")
	var tooBig map[string]any
	if err := json.Unmarshal(raw, &tooBig); err != nil || rpcErrorCode(tooBig) != rpcInvalidRequest {
		t.Errorf("lote de %d devia dar uma única resposta -32600, veio %s", len(items), raw)
	}
	if code != http.StatusBadRequest {
		t.Errorf("lote grande demais devia dar 400, deu %d", code)
	}
	code, raw = mcpRaw(t, h, "["+strings.Join(items[:mcpMaxBatch], ",")+"]")
	if err := json.Unmarshal(raw, &batch); err != nil || len(batch) != mcpMaxBatch || code != http.StatusOK {
		t.Errorf("lote de %d devia passar, veio %d %s", mcpMaxBatch, code, raw)
	}
}

// The tool's reach into the world: whatever brings third-party text (title,
// PR body, review) is "open world"; the rest is just server state.
func TestMCPOpenWorldHints(t *testing.T) {
	h := newTestServer(t).Handler()
	_, out := mcpCall(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools, _ := out["result"].(map[string]any)["tools"].([]any)
	want := map[string]bool{
		"list_prs": true, "job_status": true, "job_log": true, "read_review": true,
		"list_jobs": true, "list_reviews": true, "list_agents": false, "enqueue_review": true,
	}
	seen := 0
	for _, raw := range tools {
		tool := raw.(map[string]any)
		name := tool["name"].(string)
		exp, ok := want[name]
		if !ok {
			continue
		}
		seen++
		ann, _ := tool["annotations"].(map[string]any)
		if got, _ := ann["openWorldHint"].(bool); got != exp {
			t.Errorf("%s: openWorldHint = %v, queria %v", name, got, exp)
		}
	}
	if seen != len(want) {
		t.Errorf("faltou ferramenta na lista: vi %d de %d", seen, len(want))
	}
}

// MCP only queues what the config watches, and no more than mcpMaxRefs at a
// time. The page (/api/reviews) has no such limit.
func TestMCPEnqueueLimits(t *testing.T) {
	srv := withLens(newTestServer(t))
	srv.prs = []gh.PR{testPR(482)}
	h := srv.Handler()

	recusa := func(nome string, args map[string]any, contem string) {
		t.Helper()
		res := mcpToolResult(t, h, "enqueue_review", args)
		if res["isError"] != true {
			t.Errorf("%s: devia ser recusado: %v", nome, res)
			return
		}
		if !strings.Contains(fmt.Sprint(res["content"]), contem) {
			t.Errorf("%s: mensagem sem %q: %v", nome, contem, res["content"])
		}
		if got := srv.jobs.Snapshot(); len(got) != 0 {
			t.Errorf("%s: nada devia ter entrado na fila: %v", nome, got)
		}
	}

	recusa("repo fora da lista", map[string]any{"refs": []string{"outro/repo#1"}}, "not a watched repository")
	recusa("URL de repo fora da lista", map[string]any{"refs": []string{"https://github.com/outro/repo/pull/1"}}, "not a watched repository")
	recusa("um dentro e um fora", map[string]any{"refs": []string{"acme/api-core#482", "outro/repo#1"}}, "not a watched repository")

	muitos := make([]string, mcpMaxRefs+1)
	for i := range muitos {
		muitos[i] = fmt.Sprintf("acme/api-core#%d", 482+i)
	}
	recusa("refs acima do limite", map[string]any{"refs": muitos}, "too many")

	_, out := mcpCall(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	for _, raw := range out["result"].(map[string]any)["tools"].([]any) {
		tool := raw.(map[string]any)
		if tool["name"] != "enqueue_review" {
			continue
		}
		refs := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)["refs"].(map[string]any)
		if got, _ := refs["maxItems"].(float64); got != mcpMaxRefs {
			t.Errorf("schema de refs sem maxItems=%d: %v", mcpMaxRefs, refs)
		}
	}

	// A repo in the wrong case is the same repo, and goes in with the config's spelling.
	out2 := mcpToolCall(t, h, "enqueue_review", map[string]any{"refs": []string{"ACME/API-Core#482"}})
	if jobs, _ := out2["jobs"].([]any); len(jobs) != 1 {
		t.Errorf("owner/repo é case-insensitive: %v", out2)
	}

	// With no repos in the config there is nothing to watch: refuse and say what to do.
	srv.cfgMu.Lock()
	srv.cfg.Repos = nil
	srv.cfgMu.Unlock()
	res := mcpToolResult(t, h, "enqueue_review", map[string]any{"refs": []string{"acme/api-core#482"}})
	if res["isError"] != true || !strings.Contains(fmt.Sprint(res["content"]), "repos") {
		t.Errorf("sem cfg.Repos devia recusar pedindo para configurar repos: %v", res)
	}
}

// The page sends reviews for any repository; only MCP is restricted.
func TestPageEnqueueIgnoresMCPRepoLimit(t *testing.T) {
	srv := newTestServer(t)
	other := testPR(5)
	other.Repo = "outro/repo"
	srv.prs = []gh.PR{other}
	h := srv.Handler()
	r := httptest.NewRequest("POST", "http://127.0.0.1:7777/api/reviews", strings.NewReader(`{"refs":["outro/repo#5"]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code >= 400 {
		t.Errorf("a página devia aceitar repo fora do config: %d %s", w.Code, w.Body.String())
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
