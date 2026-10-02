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

// mcpCall manda uma mensagem JSON-RPC ao /mcp e devolve o status e o corpo.
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

// mcpTool chama uma ferramenta e devolve o structuredContent, ou falha o teste
// se ela devolveu erro.
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

// É o que o `claude mcp add --transport http` faz ao registrar: initialize,
// a notificação de pronto e a lista de ferramentas.
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

	// Versão desconhecida: o servidor oferece a dele.
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

// O /mcp manda clonar e rodar agente tanto quanto a página: o guard vale igual.
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

// O caminho inteiro de um agente externo: enfileira, espera, lê o review.
func TestMCPEnqueueAndPollToCompletion(t *testing.T) {
	srv := newTestServer(t)
	srv.prs = []gh.PR{testPR(482)}
	h := srv.Handler()

	out := mcpToolCall(t, h, "enqueue_review", map[string]any{"refs": []string{"acme/api-core#482"}})
	jobs, _ := out["jobs"].([]any)
	if len(jobs) != 1 {
		t.Fatalf("esperava 1 job, veio %v", out)
	}
	if !strings.HasPrefix(out["web_ui"].(string), "http://127.0.0.1:7777") {
		t.Errorf("resultado sem o endereço da página: %v", out["web_ui"])
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

// Publicar pelo MCP é impossível sem a chave — nem pela ferramenta, nem pelo
// atalho de escolher um agente que publica sozinho.
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

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
