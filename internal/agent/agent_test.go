package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/beroni/bazel/internal/config"
	"github.com/beroni/bazel/internal/gh"
	"github.com/beroni/bazel/internal/pricing"
)

func testPR() gh.PR {
	pr := gh.PR{
		Repo: "acme/api-core", Number: 482, Title: "feat: rate limiting",
		URL: "https://github.com/acme/api-core/pull/482", ChangedFiles: 2,
		HeadRefName: "feat/rate-limit", BaseRefName: "main",
	}
	pr.Author.Login = "maria"
	return pr
}

// The lens's task goes into the template's {{task}} — that's all that changes
// from one agent to another.
func TestBuildPromptFillsTask(t *testing.T) {
	got := buildPrompt("{{task}}\n\nPR {{number}} de {{repo}}", "/exploit-digger {{number}}", testPR(), "", "", nil)
	want := "/exploit-digger 482\n\nPR 482 de acme/api-core"
	if got != want {
		t.Errorf("prompt errado:\n%q\nesperava:\n%q", got, want)
	}
}

// A template without {{task}} — written before the named agents — gets the
// instruction on the first line, which is where it used to be.
func TestBuildPromptPrependsTaskToLegacyTemplate(t *testing.T) {
	got := buildPrompt("revise o PR {{number}}", "/lazy-senior-dev {{number}}", testPR(), "", "", nil)
	if !strings.HasPrefix(got, "/lazy-senior-dev 482\n\n") {
		t.Errorf("a task devia abrir o prompt:\n%q", got)
	}
	if !strings.HasSuffix(got, "revise o PR 482") {
		t.Errorf("o molde original devia continuar ali:\n%q", got)
	}
}

// Without a task, the template doesn't end up with a blank line in front.
func TestBuildPromptWithoutTask(t *testing.T) {
	if got := buildPrompt("{{task}}\n\nrevise", "", testPR(), "", "", nil); got != "revise" {
		t.Errorf("esperava %q, veio %q", "revise", got)
	}
}

// Publish hands the agent the review that was already read, through the
// placeholders — and doesn't tell it to review again.
func TestPublishFillsReviewPlaceholders(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Checkout = false
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agent.Command = "cat"
	cfg.Agent.Args = nil
	// `cat` returns the prompt: that lets us check what the agent saw.
	cfg.PostAgent = config.AgentDef{
		Name:   "post-report",
		Task:   "/post-report {{review_file}}",
		Prompt: "{{task}}\n\nreview lido:\n{{review}}\n\nPR {{number}}",
		Posts:  true,
	}
	choice := cfg.PostChoice()
	// The clone is required for inline, but in this test there is no repository.
	choice.Steps[0].Checkout = false

	res, err := New(cfg).Publish(context.Background(), testPR(), choice,
		"/tmp/reviews/acme-482.md", "# Veredito\n\n1 achado menor.", nil)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	for _, want := range []string{
		"/post-report /tmp/reviews/acme-482.md",
		"# Veredito",
		"1 achado menor.",
		"PR 482",
	} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("o prompt de publicação não tem %q:\n%s", want, res.Body)
		}
	}
	if !res.Posts {
		t.Error("o resultado de uma publicação devia estar marcado como tal")
	}
}

// A pipeline runs the steps in order, on the same material, and the report
// comes out in sections.
func TestReviewRunsEveryStep(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Command = "echo"
	cfg.Agent.Checkout = false
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{
		{Name: "primeiro", Args: []string{"achado do primeiro"}, Command: "echo"},
		{Name: "segundo", Args: []string{"achado do segundo"}, Command: "echo"},
	}
	cfg.Pipelines = []config.Pipeline{{Name: "dupla", Steps: []string{"primeiro", "segundo"}}}

	choice, err := cfg.ChoiceByName("dupla")
	if err != nil {
		t.Fatalf("ChoiceByName: %v", err)
	}

	var events []Event
	res, err := New(cfg).Review(context.Background(), testPR(), choice, func(e Event) {
		events = append(events, e)
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}

	if res.Agent != "dupla" || len(res.Steps) != 2 {
		t.Fatalf("esperava 2 passos de 'dupla', veio %q com %d", res.Agent, len(res.Steps))
	}
	for _, want := range []string{"## primeiro", "achado do primeiro", "## segundo", "achado do segundo"} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("o relatório não tem %q:\n%s", want, res.Body)
		}
	}

	// Without checkout there's no clone: the step events come out in order...
	var kinds []string
	for _, e := range events {
		if e.Kind == EventLog {
			continue
		}
		kinds = append(kinds, string(e.Kind)+":"+e.Name)
	}
	want := "step:primeiro,step_done:primeiro,step:segundo,step_done:segundo"
	if got := strings.Join(kinds, ","); got != want {
		t.Errorf("eventos fora de ordem:\n%s\nesperava:\n%s", got, want)
	}

	// ...and what each agent wrote arrives as log, attributed to its step.
	logs := map[int][]string{}
	for _, e := range events {
		if e.Kind == EventLog {
			logs[e.Index] = append(logs[e.Index], e.Stream+":"+e.Text)
		}
	}
	if len(logs[0]) != 1 || logs[0][0] != "stdout:achado do primeiro" {
		t.Errorf("log do primeiro passo: %v", logs[0])
	}
	if len(logs[1]) != 1 || logs[1][0] != "stdout:achado do segundo" {
		t.Errorf("log do segundo passo: %v", logs[1])
	}
}

func TestReviewPassesAgentEnv(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Checkout = false
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{{
		Name:    "env-check",
		Command: "sh",
		Args:    []string{"-c", `printf '%s' "$BAZEL_AGENT_ENV"`},
		Env:     map[string]string{"BAZEL_AGENT_ENV": "from-agent"},
	}}
	choice, err := cfg.ChoiceByName("env-check")
	if err != nil {
		t.Fatalf("ChoiceByName: %v", err)
	}
	res, err := New(cfg).Review(context.Background(), testPR(), choice, nil)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if res.Body != "from-agent" {
		t.Errorf("agent env not visible in process: %q", res.Body)
	}
}

// The log comes out while the agent runs, not only at the end — that's what the
// web interface shows live.
func TestLogStreamsWhileAgentRuns(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Checkout = false
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{{
		Name:    "conversador",
		Command: "sh",
		Args:    []string{"-c", "echo primeira; sleep 1; echo segunda"},
	}}
	cfg.Pipelines = nil
	choice, _ := cfg.ChoiceByName("conversador")

	type mark struct {
		text string
		at   time.Duration
	}
	start := time.Now()
	var marks []mark
	if _, err := New(cfg).Review(context.Background(), testPR(), choice, func(e Event) {
		if e.Kind == EventLog {
			marks = append(marks, mark{e.Text, time.Since(start)})
		}
	}); err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(marks) != 2 {
		t.Fatalf("esperava 2 linhas de log, vieram %v", marks)
	}
	if marks[0].at > 700*time.Millisecond {
		t.Errorf("a primeira linha só chegou em %s — está bufferizando até o fim", marks[0].at)
	}
	if marks[1].at < 900*time.Millisecond {
		t.Errorf("a segunda linha chegou cedo demais (%s)", marks[1].at)
	}
}

// With --output-format stream-json stdout is JSONL: the log gets the readable
// version and the report comes from the final event, not from the raw JSON.
func TestStreamJSONBecomesLogAndReport(t *testing.T) {
	events := []string{
		`{"type":"system","subtype":"init","tools":["Read","Grep"]}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"..."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"internal/gh/gh.go"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Task","input":{"subagent_type":"exploit-digger","description":"varrer o diff"}}]}}`,
		`{"type":"assistant","parent_tool_use_id":"toolu_1","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"Grep","input":{"pattern":"exec.Command"}}]}}`,
		`{"type":"assistant","parent_tool_use_id":"toolu_1","message":{"content":[{"type":"text","text":"achei uma brecha"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":"No files found"}]}}`,
		`{"type":"result","subtype":"success","result":"# Veredito\n\nNada de grave.","duration_ms":12000,"total_cost_usd":0.42,` +
			`"usage":{"input_tokens":1200,"output_tokens":8000,"cache_creation_input_tokens":4000,"cache_read_input_tokens":20000},` +
			`"modelUsage":{"claude-opus-5":{"inputTokens":1200,"outputTokens":8000,"cacheCreationInputTokens":4000,` +
			`"cacheReadInputTokens":20000,"costUSD":0.41},"claude-haiku-4-5":{"inputTokens":900,"outputTokens":100,"costUSD":0.01}}}`,
	}

	cfg := config.Default()
	cfg.Agent.Checkout = false
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{{
		Name:    "stream",
		Command: "sh",
		// The format flags go after the script: to `sh` they're just
		// positional parameters, and to Bazel they're what turns on stream mode.
		Args: []string{"-c", "cat <<'FIM'\n" + strings.Join(events, "\n") + "\nFIM\n", "sh", "--output-format", "stream-json"},
	}}
	cfg.Pipelines = nil
	choice, _ := cfg.ChoiceByName("stream")

	var log []string
	byAgent := map[string][]string{}
	res, err := New(cfg).Review(context.Background(), testPR(), choice, func(e Event) {
		if e.Kind == EventLog {
			log = append(log, e.Text)
			byAgent[e.Agent] = append(byAgent[e.Agent], e.Text)
		}
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}

	// Each line says who wrote it: the step's agent or the lens it started.
	// Without this the fleet's three lenses arrive jumbled and anonymous.
	if len(byAgent["stream"]) == 0 {
		t.Errorf("o agente do passo devia assinar as próprias linhas: %v", byAgent)
	}
	if got := byAgent["exploit-digger"]; len(got) != 2 {
		t.Errorf("as linhas de dentro do Task deviam ser do exploit-digger, vieram %v", byAgent)
	}

	if res.Body != "# Veredito\n\nNada de grave." {
		t.Errorf("o relatório devia vir do evento final, veio %q", res.Body)
	}
	if strings.Contains(res.Body, "achei uma brecha") {
		t.Error("o texto solto do sub-agente não é o relatório")
	}
	// The spend comes from the per-model detail, not from `usage`: it's the only
	// one that counts the lenses that ran as sub-agents and the auxiliary models.
	// Here it's the 33,200 of the main conversation plus the 1,000 from haiku.
	if res.Usage.Total() != 34200 || res.Usage.CostUSD != 0.42 {
		t.Errorf("o gasto devia somar todos os modelos: %+v", res.Usage)
	}

	got := strings.Join(log, "\n")
	for _, want := range []string{
		"· session started · 2 tools",
		"→ Read internal/gh/gh.go",
		"→ Agent(exploit-digger): varrer o diff",
		"achei uma brecha",
		"✗ No files found",
		"✓ done in 12s · 34,2k tokens · $0.42",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("o log não tem %q:\n%s", want, got)
		}
	}
	// Neither the raw JSON, nor the thinking, nor the rate limit become log.
	for _, noise := range []string{`{"type":`, "thinking", "rate_limit"} {
		if strings.Contains(got, noise) {
			t.Errorf("o log vazou %q:\n%s", noise, got)
		}
	}
}

// The tool that starts a sub-agent is called `Agent` (and used to be `Task`).
// Each call gets its own name: the fleet starts three lenses in parallel and
// they can't all fall into a single bucket called "sub-agent".
func TestStreamParserNamesEverySubagent(t *testing.T) {
	var p streamParser
	// Format checked against `claude -p --output-format stream-json`.
	eventos := []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_A","name":"Agent","input":{"description":"review de precisão","subagent_type":"senior-code-reviewer","prompt":"..."}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_B","name":"Agent","input":{"description":"varredura","subagent_type":"exploit-digger","prompt":"..."}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_C","name":"Agent","input":{"description":"cortes","subagent_type":"lazy-senior-dev","prompt":"..."}}]}}`,
		`{"type":"assistant","parent_tool_use_id":"toolu_B","message":{"content":[{"type":"tool_use","id":"x1","name":"Glob","input":{"pattern":"**/*.go"}}]}}`,
		`{"type":"assistant","parent_tool_use_id":"toolu_A","message":{"content":[{"type":"text","text":"1 achado menor"}]}}`,
		`{"type":"assistant","parent_tool_use_id":"toolu_C","message":{"content":[{"type":"text","text":"40 linhas a menos"}]}}`,
		`{"type":"assistant","parent_tool_use_id":"toolu_B","message":{"content":[{"type":"text","text":"nenhuma brecha"}]}}`,
	}
	porAgente := map[string]int{}
	for _, ev := range eventos {
		for _, entry := range p.line(ev) {
			porAgente[entry.Agent]++
		}
	}

	for _, lente := range []string{"senior-code-reviewer", "exploit-digger", "lazy-senior-dev"} {
		if porAgente[lente] == 0 {
			t.Errorf("nenhuma linha atribuída a %q: %v", lente, porAgente)
		}
	}
	if porAgente["sub-agente"] != 0 {
		t.Errorf("ninguém devia sobrar como \"sub-agente\": %v", porAgente)
	}
	// The three calls stay in the terminal of whoever fired them.
	if porAgente[""] != 3 {
		t.Errorf("as chamadas de Agent são do agente principal: %v", porAgente)
	}
}

// Two calls of the same type are two sub-agents, not one.
func TestStreamParserSeparatesTwinSubagents(t *testing.T) {
	var p streamParser
	p.line(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Agent","input":{"subagent_type":"exploit-digger"}}]}}`)
	p.line(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Agent","input":{"subagent_type":"exploit-digger"}}]}}`)

	um := p.line(`{"type":"assistant","parent_tool_use_id":"t1","message":{"content":[{"type":"text","text":"a"}]}}`)
	dois := p.line(`{"type":"assistant","parent_tool_use_id":"t2","message":{"content":[{"type":"text","text":"b"}]}}`)
	if um[0].Agent != "exploit-digger" || dois[0].Agent != "exploit-digger 2" {
		t.Errorf("gêmeos deviam ser distinguidos: %q e %q", um[0].Agent, dois[0].Agent)
	}
}

// A sub-agent the parser didn't see being born still gets its own name per call.
func TestStreamParserUnknownParentsStaySeparate(t *testing.T) {
	var p streamParser
	um := p.line(`{"type":"assistant","parent_tool_use_id":"perdido1","message":{"content":[{"type":"text","text":"a"}]}}`)
	dois := p.line(`{"type":"assistant","parent_tool_use_id":"perdido2","message":{"content":[{"type":"text","text":"b"}]}}`)
	if um[0].Agent == dois[0].Agent {
		t.Errorf("dois desconhecidos não podem virar um: %q", um[0].Agent)
	}
}

// A regular tool is not a sub-agent.
func TestStreamParserIgnoresPlainTools(t *testing.T) {
	var p streamParser
	p.line(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"r1","name":"Read","input":{"file_path":"main.go"}}]}}`)
	if len(p.subagents) != 0 {
		t.Errorf("Read não sobe agente: %v", p.subagents)
	}
}

// A line that isn't JSON passes straight through — an agent that doesn't speak
// this format can't vanish from the log.
func TestStreamParserPassesThroughNonJSON(t *testing.T) {
	var p streamParser
	if got := p.line("apenas texto"); len(got) != 1 || got[0].Text != "apenas texto" {
		t.Errorf("linha crua devia passar direto, veio %v", got)
	}
}

// Without a final event, the report is what the assistant wrote along the way.
func TestStreamParserFallsBackToAssistantText(t *testing.T) {
	var p streamParser
	p.line(`{"type":"assistant","message":{"content":[{"type":"text","text":"parcial"}]}}`)
	if got := p.report(); got != "parcial" {
		t.Errorf("esperava o texto do assistente, veio %q", got)
	}
}

func TestIsStreamJSON(t *testing.T) {
	cases := map[bool][][]string{
		true: {
			{"-p", "--output-format", "stream-json", "--verbose"},
			{"--output-format=stream-json"},
		},
		false: {
			{"-p"},
			{"--output-format", "json"},
			{"--output-format"},
		},
	}
	for want, sets := range cases {
		for _, args := range sets {
			if got := isStreamJSON(args); got != want {
				t.Errorf("isStreamJSON(%v) = %v", args, got)
			}
		}
	}
}

func TestCodexJSONBecomesLogReportAndUsage(t *testing.T) {
	var p codexAdapter
	events := []string{
		`{"type":"thread.started","thread_id":"t"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.started","item":{"id":"cmd","type":"command_execution","command":"rg TODO"}}`,
		`{"type":"item.completed","item":{"id":"cmd","type":"command_execution","status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"reason","type":"reasoning","text":"private"}}`,
		`{"type":"item.completed","item":{"id":"message","type":"agent_message","text":"# Review\n\nFound one bug."}}`,
		`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":60,"output_tokens":20,"reasoning_output_tokens":12}}`,
	}
	var logs []string
	for _, event := range events {
		for _, line := range p.line(event) {
			logs = append(logs, line.Text)
		}
	}
	if got := p.report(); got != "# Review\n\nFound one bug." {
		t.Errorf("report = %q", got)
	}
	if got := p.usage(); got != (Usage{InputTokens: 40, CacheRead: 60, OutputTokens: 20}) {
		t.Errorf("usage = %+v", got)
	}
	joined := strings.Join(logs, "\n")
	for _, want := range []string{"· session started", "→ rg TODO", "✓ done · 120 tokens"} {
		if !strings.Contains(joined, want) {
			t.Errorf("log missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "private") {
		t.Errorf("reasoning leaked into log: %s", joined)
	}
}

func TestCodexJSONIgnoresMalformedEventsAndFailsTerminalTurn(t *testing.T) {
	var p codexAdapter
	if got := p.line("not json"); len(got) != 1 || got[0].Text != "not json" {
		t.Errorf("malformed JSON should remain visible: %v", got)
	}
	p.line(`{"type":"turn.failed","message":"rate limit exceeded"}`)
	if err := p.err(); err == nil || !strings.Contains(err.Error(), "rate limit exceeded") {
		t.Errorf("terminal error = %v", err)
	}
}

func TestCodexJSONShowsFailedCommandReason(t *testing.T) {
	var p codexAdapter
	got := p.line(`{"type":"item.completed","item":{"type":"command_execution","status":"failed","aggregated_output":"fatal: invalid upstream\nmore detail"}}`)
	if len(got) != 1 || got[0].Text != "  ✗ command failed: fatal: invalid upstream" {
		t.Errorf("failed command log = %v", got)
	}
}

func TestCodexJSONRunsThroughRunner(t *testing.T) {
	events := []string{
		`{"type":"item.started","item":{"type":"command_execution","command":"git diff"}}`,
		`{"type":"item.completed","item":{"id":"final","type":"agent_message","text":"# Verdict\n\nLooks good."}}`,
		`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":3}}`,
	}
	cfg := config.Default()
	cfg.Agent.Checkout = false
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agents = []config.AgentDef{{
		Name:    "codex",
		Command: "sh",
		Args:    []string{"-c", "cat <<'EOF'\n" + strings.Join(events, "\n") + "\nEOF", "sh"},
		Format:  "codex-json",
	}}
	choice, _ := cfg.ChoiceByName("codex")
	var logs []string
	res, err := New(cfg).Review(context.Background(), testPR(), choice, func(e Event) {
		if e.Kind == EventLog {
			logs = append(logs, e.Text)
		}
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if res.Body != "# Verdict\n\nLooks good." {
		t.Errorf("report = %q", res.Body)
	}
	if res.Usage.Total() != 13 {
		t.Errorf("usage = %+v", res.Usage)
	}
	if got := strings.Join(logs, "\n"); !strings.Contains(got, "→ git diff") {
		t.Errorf("Codex progress not forwarded: %s", got)
	}
}

func TestOutputAdapterKeepsLegacyFormatDetection(t *testing.T) {
	legacy, err := newOutputAdapter("", []string{"-p", "--output-format", "stream-json"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := legacy.(*claudeAdapter); !ok {
		t.Fatalf("legacy adapter = %T, want claude", legacy)
	}
	plain, err := newOutputAdapter("", []string{"exec", "--json", "-"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plain.(*plainAdapter); !ok {
		t.Fatalf("unconfigured Codex adapter = %T, want plain", plain)
	}
	if _, err := newOutputAdapter("unknown", nil); err == nil {
		t.Error("unknown explicit format should fail")
	}
}

func TestGrokStreamBecomesLogReportAndUsage(t *testing.T) {
	var p grokAdapter
	events := []string{
		`{"type":"available_commands","tools":["read_file"]}`,
		`{"type":"thought","data":"private"}`,
		`{"type":"text","data":"# Review\n"}`,
		`{"type":"text","data":"Looks good."}`,
		`{"type":"usage","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40,"reasoning_tokens":10}}`,
		`{"type":"tool_call","title":"run_terminal_command","rawInput":{"command":"git diff","description":"inspect diff"}}`,
		`{"type":"tool_call_update","status":"completed","rawOutput":{"exit_code":0,"command":"git diff","output_for_prompt":"ok"}}`,
		`{"type":"end","stopReason":"end_turn","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40,"reasoning_tokens":10,"total_tokens":190},"total_cost_usd":0.02}`,
	}
	var logs []string
	for _, event := range events {
		for _, line := range p.line(event) {
			logs = append(logs, line.Text)
		}
	}
	if got := p.report(); got != "# Review\nLooks good." {
		t.Errorf("report = %q", got)
	}
	if got := p.usage(); got != (Usage{InputTokens: 100, OutputTokens: 20, CacheRead: 30, CacheWrite: 40, CostUSD: 0.02}) {
		t.Errorf("usage = %+v", got)
	}
	joined := strings.Join(logs, "\n")
	for _, want := range []string{"→ git diff", "✓ command finished", "✓ done · 190 tokens · $0.02"} {
		if !strings.Contains(joined, want) {
			t.Errorf("log missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "private") {
		t.Errorf("thought leaked into log: %s", joined)
	}
}

func TestGrokStreamShowsFailedCommandReason(t *testing.T) {
	var p grokAdapter
	got := p.line(`{"type":"tool_call_update","status":"completed","rawOutput":{"exit_code":1,"output_for_prompt":"fatal: bad revision\nmore"}}`)
	if len(got) != 1 || got[0].Text != "  ✗ command failed: fatal: bad revision" {
		t.Errorf("failed command log = %v", got)
	}
}

func TestGrokStreamTreatsGrepNoMatchesAsProgress(t *testing.T) {
	var p grokAdapter
	p.line(`{"type":"tool_call","toolCallId":"grep-1","toolName":"grep","title":"Grep","rawInput":{"command":"StatusMetricsProvider"}}`)
	got := p.line(`{"type":"tool_call_update","toolCallId":"grep-1","status":"completed","rawOutput":{"exit_code":1,"output_for_prompt":"exit: 1"}}`)
	if len(got) != 1 || got[0].Text != "  · grep: no matches" {
		t.Errorf("grep no-match log = %v", got)
	}
}

func TestGrokStreamErrorEventSetsTerminal(t *testing.T) {
	var p grokAdapter
	got := p.line(`{"type":"error","message":"max turns reached"}`)
	if len(got) != 1 || got[0].Text != "✗ max turns reached" {
		t.Errorf("error log = %v", got)
	}
	if p.err() == nil || p.err().Error() != "max turns reached" {
		t.Errorf("terminal error = %v", p.err())
	}
}

func TestGrokStreamAccumulatesLiveUsage(t *testing.T) {
	var p grokAdapter
	p.line(`{"type":"usage","usage":{"input_tokens":10,"output_tokens":2}}`)
	p.line(`{"type":"usage","usage":{"input_tokens":11,"output_tokens":3}}`)
	if got := p.usage(); got != (Usage{InputTokens: 21, OutputTokens: 5}) {
		t.Errorf("accumulated live usage = %+v", got)
	}
}

func TestGrokTurnLimitHasOneClearError(t *testing.T) {
	var p grokAdapter
	if got := p.line(`{"type":"end","stopReason":"cancelled"}`); got != nil || p.err() != nil {
		t.Errorf("turn-limit end should not produce a second error: logs=%v err=%v", got, p.err())
	}

	step := config.ResolvedAgent{Command: "grok", Format: "grok-stream"}
	if got := agentFailure(step, "Error: max turns reached").Error(); got != "Grok reached its configured turn limit before completing the review" {
		t.Errorf("turn-limit error = %q", got)
	}
}

// A step that fails doesn't bring down the review: it becomes a section with
// the error, and what worked stays in the report.
func TestReviewKeepsGoingAfterFailedStep(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Checkout = false
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{
		{Name: "quebrado", Command: "false"},
		{Name: "inteiro", Command: "echo", Args: []string{"achado"}},
	}
	cfg.Pipelines = []config.Pipeline{{Name: "dupla", Steps: []string{"quebrado", "inteiro"}}}
	choice, _ := cfg.ChoiceByName("dupla")

	res, err := New(cfg).Review(context.Background(), testPR(), choice, nil)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if res.Steps[0].Err == nil {
		t.Error("o primeiro passo devia ter falhado")
	}
	if !strings.Contains(res.Body, "✗ failed") || !strings.Contains(res.Body, "achado") {
		t.Errorf("o relatório devia trazer a falha e o achado:\n%s", res.Body)
	}
}

// All steps failing is the whole review failing.
func TestReviewFailsWhenEveryStepFails(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Checkout = false
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agents = []config.AgentDef{{Name: "quebrado", Command: "false"}}
	cfg.Pipelines = nil
	choice, _ := cfg.ChoiceByName("quebrado")

	if _, err := New(cfg).Review(context.Background(), testPR(), choice, nil); err == nil {
		t.Fatal("review sem nenhum passo bom devia falhar")
	}
}

// Cancelling releases the review right away, without touching the next steps.
func TestReviewStopsOnCancel(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Checkout = false
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{
		{Name: "demorado", Command: "sleep", Args: []string{"30"}},
		{Name: "nunca", Command: "echo", Args: []string{"não devia rodar"}},
	}
	cfg.Pipelines = []config.Pipeline{{Name: "dupla", Steps: []string{"demorado", "nunca"}}}
	choice, _ := cfg.ChoiceByName("dupla")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	var ran []string
	_, err := New(cfg).Review(ctx, testPR(), choice, func(e Event) {
		if e.Kind == EventStep {
			ran = append(ran, e.Name)
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("esperava cancelamento, veio %v", err)
	}
	if len(ran) != 1 || ran[0] != "demorado" {
		t.Errorf("o segundo passo não devia começar: %v", ran)
	}
}

// Each step's spend adds up into the review's — a three-lens pipeline returns
// a single number at the end.
func TestUsageSomaEFormata(t *testing.T) {
	var u Usage
	u.add(Usage{InputTokens: 500, OutputTokens: 250, CostUSD: 0.1})
	u.add(Usage{CacheRead: 1_000_000, CostUSD: 0.2})
	if u.Total() != 1_000_750 {
		t.Errorf("Total somou errado: %d", u.Total())
	}
	if got := u.String(); !strings.Contains(got, "1M tokens") || !strings.Contains(got, "$0.30") {
		t.Errorf("linha de gasto inesperada: %q", got)
	}

	// An agent that doesn't speak stream-json reports nothing, so there's no line.
	if got := (Usage{}).String(); got != "" {
		t.Errorf("sem gasto não há linha: %q", got)
	}

	for _, c := range []struct {
		n    int
		want string
	}{{0, "0"}, {999, "999"}, {1000, "1k"}, {45_230, "45,2k"}, {1_800_000, "1,8M"}} {
		if got := FormatTokens(c.n); got != c.want {
			t.Errorf("FormatTokens(%d) = %q, queria %q", c.n, got, c.want)
		}
	}
}

// The sum of the steps keeps the model as long as there is only one — that
// is what lets the StringWithCost fallback price a total that arrived without
// a cost. Steps from different models have no table row that prices them
// together.
func TestUsageSomaLevaOModelo(t *testing.T) {
	var u Usage
	u.add(Usage{InputTokens: 10, Model: "claude-opus-4-5"})
	u.add(Usage{OutputTokens: 5, Model: "claude-opus-4-5"})
	u.add(Usage{})
	if u.Model != "claude-opus-4-5" {
		t.Errorf("soma de um modelo só perdeu o Model: %q", u.Model)
	}
	if got := u.Plus(Usage{InputTokens: 1, Model: "claude-haiku-4-5"}).Model; got != "" {
		t.Errorf("soma de modelos diferentes devia zerar o Model, ficou %q", got)
	}
}

// Without modelUsage — a simpler agent, or an old version of Claude Code —
// the main conversation's `usage` still counts.
func TestUsageCaiNoUsageQuandoNaoHaModelUsage(t *testing.T) {
	var p streamParser
	p.line(`{"type":"result","subtype":"success","result":"ok","duration_ms":1000,` +
		`"total_cost_usd":0.05,"usage":{"input_tokens":10,"output_tokens":20,` +
		`"cache_creation_input_tokens":30,"cache_read_input_tokens":40}}`)
	if p.usage.Total() != 100 || p.usage.CostUSD != 0.05 {
		t.Errorf("devia cair no usage: %+v", p.usage)
	}
}

// The count moves while the agent works: each stream message carries what that
// call spent, and the partial goes to the screen before the settlement.
func TestUsageParcialChegaDuranteOReview(t *testing.T) {
	events := []string{
		`{"type":"system","subtype":"init","tools":[]}`,
		`{"type":"assistant","message":{"usage":{"input_tokens":10,"output_tokens":5,` +
			`"cache_creation_input_tokens":100,"cache_read_input_tokens":0},` +
			`"content":[{"type":"text","text":"lendo o diff"}]}}`,
		`{"type":"assistant","message":{"usage":{"input_tokens":2,"output_tokens":3,` +
			`"cache_creation_input_tokens":0,"cache_read_input_tokens":80},` +
			`"content":[{"type":"text","text":"pensando"}]}}`,
		`{"type":"result","subtype":"success","result":"# Veredito","duration_ms":900,` +
			`"total_cost_usd":0.5,"modelUsage":{"claude-opus-5":{"inputTokens":12,"outputTokens":8,` +
			`"cacheCreationInputTokens":100,"cacheReadInputTokens":80,"costUSD":0.5},` +
			`"sub":{"inputTokens":1000,"outputTokens":0,"costUSD":0}}}`,
	}

	cfg := config.Default()
	cfg.Agent.Checkout = false
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{{
		Name:    "stream",
		Command: "sh",
		Args:    []string{"-c", "cat <<'FIM'\n" + strings.Join(events, "\n") + "\nFIM\n", "sh", "--output-format", "stream-json"},
	}}
	choice, _ := cfg.ChoiceByName("stream")

	var parciais []int
	res, err := New(cfg).Review(context.Background(), testPR(), choice, func(e Event) {
		if e.Kind == EventUsage {
			parciais = append(parciais, e.Usage.Total())
		}
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}

	// One update per message that spent, plus the settlement.
	if len(parciais) < 3 {
		t.Fatalf("esperava a conta andando, vieram %v", parciais)
	}
	if parciais[0] != 115 || parciais[1] != 200 {
		t.Errorf("o parcial devia somar mensagem a mensagem: %v", parciais)
	}
	// The settlement is bigger than the partial: it's the one that sees the sub-agent.
	if fim := parciais[len(parciais)-1]; fim != 1200 {
		t.Errorf("o último aviso devia trazer o total fechado, veio %d", fim)
	}
	if res.Usage.Total() != 1200 {
		t.Errorf("o resultado devia ficar com o total fechado: %+v", res.Usage)
	}
}

// A step that stays in Bazel and failed does not vanish from the screen:
// its section shows the failure, as the pipeline report always did.
func TestContextBodyMostraPassoQueFalhou(t *testing.T) {
	choice := config.Choice{Name: "p", Pipeline: true, Publishable: true, Steps: []config.ResolvedAgent{
		{Name: "history-pr", Publishable: false},
		{Name: "review-fleet", Publishable: true},
	}}
	res := Result{Steps: []StepResult{
		{Name: "history-pr", Err: errors.New("exit status 1\nstack")},
		{Name: "review-fleet", Body: "- token guardado no cliente"},
	}}
	got := ContextBody(res, choice)
	if !strings.Contains(got, "history-pr failed: exit status 1") {
		t.Errorf("a falha do passo que fica no Bazel sumiu: %q", got)
	}
	if strings.Contains(got, "stack") || strings.Contains(got, "token guardado") {
		t.Errorf("só a primeira linha do erro, e nada do passo publicável: %q", got)
	}
}

// In a pipeline that joins a story and a review, the screen shows both and
// the PR gets only the review. That is the whole point of marking an agent as
// non-publishable.
func TestPublishableBody(t *testing.T) {
	steps := []StepResult{
		{Name: "history-pr", Body: "48 commits, 3 rodadas de review"},
		{Name: "review-fleet", Body: "- token guardado no cliente"},
	}
	full, err := joinSteps(steps)
	if err != nil {
		t.Fatalf("joinSteps: %v", err)
	}
	res := Result{Body: full, Steps: steps}

	t.Run("só o passo publicável sobe", func(t *testing.T) {
		choice := config.Choice{Name: "história e review", Pipeline: true, Publishable: true, Steps: []config.ResolvedAgent{
			{Name: "history-pr", Publishable: false},
			{Name: "review-fleet", Publishable: true},
		}}
		body, trimmed, err := PublishableBody(res, choice)
		if err != nil {
			t.Fatalf("PublishableBody: %v", err)
		}
		if !trimmed {
			t.Error("algo ficou de fora — o arquivo em disco já não serve para publicar")
		}
		if strings.Contains(body, "48 commits") {
			t.Errorf("a história não podia ir ao PR: %q", body)
		}
		if !strings.Contains(body, "token guardado no cliente") {
			t.Errorf("o review tinha de ir: %q", body)
		}
		// With a single step left, it comes out raw — without the "## review-fleet"
		// header, which only exists to separate steps.
		if strings.Contains(body, "## review-fleet") {
			t.Errorf("um passo só sai como sempre saiu: %q", body)
		}
	})

	t.Run("tudo publicável é o relatório inteiro", func(t *testing.T) {
		choice := config.Choice{Name: "tudo", Pipeline: true, Publishable: true, Steps: []config.ResolvedAgent{
			{Name: "history-pr", Publishable: true},
			{Name: "review-fleet", Publishable: true},
		}}
		body, trimmed, err := PublishableBody(res, choice)
		if err != nil {
			t.Fatalf("PublishableBody: %v", err)
		}
		if trimmed || body != full {
			t.Error("sem nada aparado, o que vai ao PR é o relatório como está")
		}
	})

	t.Run("nada publicável é erro", func(t *testing.T) {
		choice := config.Choice{Name: "só história", Steps: []config.ResolvedAgent{
			{Name: "history-pr", Publishable: false},
			{Name: "review-fleet", Publishable: false},
		}}
		if _, _, err := PublishableBody(res, choice); err == nil {
			t.Error("sem passo publicável não há o que publicar")
		}
	})
}

// An embedded skill needs to exist on disk for Claude Code to find it, and its
// place is the clone. Without a clone it has nowhere to exist, and writing it
// into the folder of whoever ran Bazel would be littering someone else's project.
func TestBuiltinPrecisaDoClone(t *testing.T) {
	comClone := config.Choice{Name: "publicar", Steps: []config.ResolvedAgent{
		{Name: "bazel-post-report", Task: "/bazel-post-report {{review_file}}", Checkout: true},
	}}
	if got := builtinSemClone(comClone); got != "" {
		t.Errorf("com checkout ligado não há o que reclamar, veio %q", got)
	}
	if !usaBuiltin(comClone) {
		t.Error("a task chama uma skill embarcada")
	}

	semClone := config.Choice{Name: "publicar", Steps: []config.ResolvedAgent{
		{Name: "bazel-post-report", Task: "/bazel-post-report {{review_file}}", Checkout: false},
	}}
	if got := builtinSemClone(semClone); got != "bazel-post-report" {
		t.Errorf("sem checkout a skill não tem onde ser escrita: %q", got)
	}

	propria := config.Choice{Name: "publicar", Steps: []config.ResolvedAgent{
		{Name: "post-report", Task: "/post-report {{review_file}}", Checkout: false},
	}}
	if usaBuiltin(propria) || builtinSemClone(propria) != "" {
		t.Error("a skill do usuário não é embarcada — checkout: false nela é escolha dele")
	}
}

// A Claude Code run mixes models. The main one is the one that spent the
// most — always the same one, run after run — and the table charges each
// model its own price instead of charging everything at the main model's
// price.
func TestCustoPorModeloEModeloPrincipalEstavel(t *testing.T) {
	ev := `{"type":"result","subtype":"success","result":"ok","total_cost_usd":9.99,` +
		`"modelUsage":{"claude-haiku-4-5-20251001":{"inputTokens":1000000,"outputTokens":0,"costUSD":0.5},` +
		`"claude-opus-5-5":{"inputTokens":2000000,"outputTokens":0,"costUSD":3}}}`
	for range 20 {
		var p streamParser
		p.line(ev)
		if p.usage.Model != "claude-opus-5-5" {
			t.Fatalf("modelo principal devia ser o que mais gastou, veio %q", p.usage.Model)
		}
		u := priced(p.spend(), p.models(), &pricing.Default)
		// 2M of Opus 5.5 at $4 + 1M of Haiku 4.5 at $1 — not 3M at $4.
		if u.CostUSD != 9 {
			t.Fatalf("custo por modelo = %.2f, queria 9.00", u.CostUSD)
		}
	}
}

// A model outside the table does not zero the cost the provider reported:
// Grok brings its own total_cost_usd, and erasing it would leave the card at
// $0.00.
func TestCustoDoProvedorSobreviveModeloDesconhecido(t *testing.T) {
	u := priced(Usage{InputTokens: 1000, CostUSD: 0.42, Model: "grok"}, nil, &pricing.Default)
	if u.CostUSD != 0.42 {
		t.Errorf("custo do provedor apagado: %.2f", u.CostUSD)
	}
	// In a mixed model, the unknown one comes in with the cost the provider
	// gave for it and the known one with the table price.
	u = priced(Usage{CostUSD: 5}, []Usage{
		{InputTokens: 1_000_000, Model: "claude-haiku-4-5", CostUSD: 3},
		{InputTokens: 1_000_000, Model: "modelo-local", CostUSD: 0.5},
	}, &pricing.Default)
	if u.CostUSD != 1.5 {
		t.Errorf("custo misto = %.2f, queria 1.50", u.CostUSD)
	}
	// Without a table, nothing changes.
	if u := priced(Usage{CostUSD: 0.42, Model: "claude-opus-5-5"}, nil, nil); u.CostUSD != 0.42 {
		t.Errorf("sem tabela o custo mudou: %.2f", u.CostUSD)
	}
}

// A cost that already came ready is not redone in the review footer:
// redoing it from the main model would undo the per-model accounting.
func TestStringWithCostNaoRefazCustoPronto(t *testing.T) {
	u := Usage{InputTokens: 1_000_000, CostUSD: 1.23, Model: "claude-opus-5-5"}
	if got := u.StringWithCost(&pricing.Default); !strings.Contains(got, "$1.23") {
		t.Errorf("custo pronto refeito: %q", got)
	}
	u.CostUSD = 0
	if got := u.StringWithCost(&pricing.Default); !strings.Contains(got, "$4.00") {
		t.Errorf("custo ausente devia vir da tabela: %q", got)
	}
}

// Each step keeps its unrounded cost: five cheap steps of $0.004 add up to
// $0.02 at the end of the review, not $0.00.
func TestCustoDePassosBaratosNaoSomaZero(t *testing.T) {
	var total Usage
	for range 5 {
		// 4k Haiku 4.5 tokens at $1/M = $0.004.
		total.add(priced(Usage{InputTokens: 4000, Model: "claude-haiku-4-5"}, nil, &pricing.Default))
	}
	if math.Abs(total.CostUSD-0.02) > 1e-9 {
		t.Errorf("soma de 5 passos de $0.004 = %f, queria 0.02", total.CostUSD)
	}
	if got := total.String(); !strings.Contains(got, "$0.02") {
		t.Errorf("a linha de gasto devia mostrar $0.02: %q", got)
	}
}

// The whole path (process, stream-json, per-model usage, price table) prices
// each model at its own rate in Review, instead of keeping the event's
// total_cost_usd or pricing everything at the main model's rate.
func TestReviewCobraCadaModeloPelaTabela(t *testing.T) {
	ev := `{"type":"result","subtype":"success","result":"ok","total_cost_usd":9.99,` +
		`"modelUsage":{"claude-haiku-4-5-20251001":{"inputTokens":1000000,"outputTokens":0,"costUSD":0.5},` +
		`"claude-opus-5-5":{"inputTokens":2000000,"outputTokens":0,"costUSD":3}}}`

	cfg := config.Default()
	cfg.Agent.Checkout = false
	cfg.Agent.Prompt = "{{task}}"
	cfg.Agent.TimeoutSeconds = 30
	cfg.Agents = []config.AgentDef{{
		Name:    "stream",
		Command: "sh",
		Args:    []string{"-c", "cat <<'FIM'\n" + ev + "\nFIM\n", "sh", "--output-format", "stream-json"},
	}}
	cfg.Pipelines = nil
	choice, _ := cfg.ChoiceByName("stream")

	r := New(cfg)
	r.PricingTable = &pricing.Default
	res, err := r.Review(context.Background(), testPR(), choice, func(Event) {})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	// 2M Opus 5.5 at $4 + 1M Haiku 4.5 at $1.
	if res.Usage.CostUSD != 9 {
		t.Errorf("custo do review = %.2f, queria 9.00", res.Usage.CostUSD)
	}
	if len(res.Steps) != 1 || res.Steps[0].Usage.Model != "claude-opus-5-5" || res.Steps[0].Usage.CostUSD != 9 {
		t.Errorf("passo devia levar o principal e o custo da tabela: %+v", res.Steps)
	}
}

// The done line in the log prices the run like the Runner does: with a price
// table, the card, the footer and the saved file show the table's number, and
// the log used to show the provider's total_cost_usd instead (BUG-QA-01).
func TestDoneLineUsesTheSamePriceAsTheCard(t *testing.T) {
	table := pricing.Table{"m-known": {Input: 2, Output: 10}}
	result := `{"type":"result","subtype":"success","result":"ok","duration_ms":4000,"total_cost_usd":9.99,` +
		`"usage":{"input_tokens":1000000,"output_tokens":100000},` +
		`"modelUsage":{"m-known":{"inputTokens":1000000,"outputTokens":100000,"costUSD":9.99}}}`

	p := streamParser{pricing: &table}
	var done string
	for _, e := range p.line(result) {
		done = e.Text
	}
	card := priced(p.spend(), p.models(), &table)
	if want := fmt.Sprintf("$%.2f", card.CostUSD); card.CostUSD != 3 || !strings.HasSuffix(done, want) {
		t.Errorf("done line %q should end with the card's %s (table price $3.00)", done, want)
	}

	// Without a table both keep the provider's number.
	var raw streamParser
	for _, e := range raw.line(result) {
		done = e.Text
	}
	if !strings.HasSuffix(done, "$9.99") {
		t.Errorf("with no table the done line keeps total_cost_usd: %q", done)
	}
}
