// This file translates the JSONL from `claude -p --output-format stream-json`
// into readable log lines. It's what makes the web interface show the agent
// working — without it, a `claude -p` stays silent until the report comes out.
package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/beroni/bazel/internal/pricing"
)

// isStreamJSON says whether the args ask for the event format. Only then is
// stdout JSONL: in any other case it's the report itself, and goes raw to the
// log.
func isStreamJSON(args []string) bool {
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, "--output-format="); ok {
			return strings.TrimSpace(v) == "stream-json"
		}
		if a == "--output-format" && i+1 < len(args) {
			return strings.TrimSpace(args[i+1]) == "stream-json"
		}
	}
	return false
}

// streamEvent is the part of a stream event that matters here. The format has
// many more fields than this; everything not mapped is ignored on purpose, so a
// new version of the agent doesn't break the parser.
type streamEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	// Result and IsError come in the final event.
	Result     string  `json:"result"`
	IsError    bool    `json:"is_error"`
	DurationMS int     `json:"duration_ms"`
	CostUSD    float64 `json:"total_cost_usd"`
	// Usage is the spend of the main conversation — only that. An agent that
	// spawns sub-agents spends much more than this says.
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		CacheWrite   int `json:"cache_creation_input_tokens"`
		CacheRead    int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	// ModelUsage is the spend per model, and it's what settles the count: it
	// includes the lenses that ran as sub-agents and the auxiliary models Claude
	// Code itself uses. In a run with a single lens, the `usage` above counted
	// 52k tokens while this one added up to 68k.
	ModelUsage map[string]struct {
		InputTokens  int     `json:"inputTokens"`
		OutputTokens int     `json:"outputTokens"`
		CacheRead    int     `json:"cacheReadInputTokens"`
		CacheWrite   int     `json:"cacheCreationInputTokens"`
		CostUSD      float64 `json:"costUSD"`
	} `json:"modelUsage"`
	// RateLimit comes in the `rate_limit_event` event: it's Claude's quota, with
	// the five-hour and seven-day windows. It arrives on its own, unasked.
	RateLimit *struct {
		Status         string `json:"status"`
		UnifiedWindows map[string]struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    int64   `json:"resetsAt"`
		} `json:"unifiedWindows"`
	} `json:"rate_limit_info"`
	// ParentToolUseID marks what ran inside a sub-agent.
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Tools           []any   `json:"tools"`
	Message         struct {
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		// Usage is the spend of the call that produced this message. It's what
		// allows counting while the agent is still working: added up, they
		// reproduce the final event's `usage`.
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			CacheWrite   int `json:"cache_creation_input_tokens"`
			CacheRead    int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// spent is the spend of the final event. The per-model detail wins when it
// exists — it is the only one that sees the sub-agents; without it what is
// left is `usage`, which is what a simpler agent reports.
//
// The cost in USD is computed from the price table, not from the event's
// `total_cost_usd` field: that field comes from Claude Code and may not be
// present in every provider. The table is configurable in config.yaml.
//
// The second return value is the same spend split by model, so the price
// table can charge each one its own price; nil when the tokens did not come
// from modelUsage and there is no way to split them.
func (ev streamEvent) spent() (Usage, []Usage) {
	u := Usage{CostUSD: ev.CostUSD}
	if len(ev.ModelUsage) == 0 {
		u.InputTokens = ev.Usage.InputTokens
		u.OutputTokens = ev.Usage.OutputTokens
		u.CacheWrite = ev.Usage.CacheWrite
		u.CacheRead = ev.Usage.CacheRead
		u.Model = ev.Message.Model
		return u, nil
	}
	var custo float64
	for _, m := range ev.ModelUsage {
		u.InputTokens += m.InputTokens
		u.OutputTokens += m.OutputTokens
		u.CacheWrite += m.CacheWrite
		u.CacheRead += m.CacheRead
		custo += m.CostUSD
	}
	// Per-model detail present but with no tokens at all: usage is what counts.
	usouModelUsage := u.Total() > 0
	if !usouModelUsage {
		u.InputTokens = ev.Usage.InputTokens
		u.OutputTokens = ev.Usage.OutputTokens
		u.CacheWrite = ev.Usage.CacheWrite
		u.CacheRead = ev.Usage.CacheRead
	}
	// total_cost_usd is the final word on cost; the per-model sum only comes
	// in when it's missing.
	if u.CostUSD == 0 {
		u.CostUSD = custo
	}
	// The main model is the one that spent the most tokens — on a tie, the
	// first in alphabetical order. Iterating the map directly would give a
	// different name on each run.
	names := make([]string, 0, len(ev.ModelUsage))
	for name := range ev.ModelUsage {
		names = append(names, name)
	}
	sort.Strings(names)
	var (
		maior   int
		byModel []Usage
	)
	for _, name := range names {
		m := ev.ModelUsage[name]
		part := Usage{
			InputTokens:  m.InputTokens,
			OutputTokens: m.OutputTokens,
			CacheWrite:   m.CacheWrite,
			CacheRead:    m.CacheRead,
			CostUSD:      m.CostUSD,
			Model:        name,
		}
		if u.Model == "" || part.Total() > maior {
			u.Model, maior = name, part.Total()
		}
		byModel = append(byModel, part)
	}
	// The per-model detail only counts if it is what counted the tokens; when
	// the count fell back to `usage`, there is no way to split it between the
	// models.
	if !usouModelUsage {
		byModel = nil
	}
	if u.Model == "" {
		u.Model = ev.Message.Model
	}
	return u, byModel
}

// contentBlock is a message content block: text, tool call or tool
// result.
type contentBlock struct {
	Type string `json:"type"`
	// ID is the identifier of a tool call — it's through it that the events
	// from inside a Task say which sub-agent they came from.
	ID      string          `json:"id"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   map[string]any  `json:"input"`
	IsError bool            `json:"is_error"`
	Content json.RawMessage `json:"content"`
}

// logEntry is a log line already attributed to whoever wrote it. An empty Agent
// is the step's own agent; filled in, it's a sub-agent it spawned.
type logEntry struct {
	Agent string
	Text  string
}

// streamParser reads the JSONL and keeps the agent's final text.
type streamParser struct {
	result string
	// usage is the settled spend, from the final event. An agent that doesn't
	// speak stream-json leaves this zeroed — there's nowhere to get it from.
	usage Usage
	// byModel is the closed spend split by model, for the price table. unsplit
	// says some final event did not bring the split — then it does not cover the
	// total, and whoever charges uses the total.
	byModel []Usage
	unsplit bool
	// live is the partial count, summed message by message while the agent
	// works. It stays below the total: it only sees the main conversation, and
	// the lenses that run as a sub-agent only come in at closing.
	live Usage
	// limits is the latest reading of Claude's quota.
	limits Limits
	// texts is the text the assistant wrote, used as the report if the final
	// event doesn't come (agent died midway, different format).
	texts []string
	// subagents maps the id of a Task call to the name of the sub-agent it
	// started. It's what allows telling which of the fleet's lenses wrote each
	// line: they run in parallel inside the same process and without this the
	// lines arrive jumbled and anonymous.
	subagents map[string]string
	// pricing is the price table the Runner charges with. The done line prices
	// the run with it, so the log shows the same number as the card, the report
	// footer and the saved file. Nil keeps the provider's total_cost_usd.
	pricing *pricing.Table
}

// line translates a stream line into zero or more log lines. A line that isn't
// valid JSON passes straight through: it's the only way not to swallow the
// output of an agent that doesn't speak this format.
func (p *streamParser) line(raw string) []logEntry {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	var ev streamEvent
	if err := json.Unmarshal([]byte(trimmed), &ev); err != nil {
		return []logEntry{{Text: raw}}
	}

	// Who wrote it: the step's agent, or the sub-agent that owns the Task
	// call this event came from.
	who := ""
	if ev.ParentToolUseID != nil && *ev.ParentToolUseID != "" {
		who = p.subagentName(*ev.ParentToolUseID)
	}
	one := func(text string) []logEntry { return []logEntry{{Agent: who, Text: text}} }

	switch ev.Type {
	case "rate_limit_event":
		p.readLimits(ev)
		// It doesn't become a log line: it's a number in a corner of the screen,
		// not something the agent is doing.
		return nil

	case "system":
		if ev.Subtype == "init" {
			return one(fmt.Sprintf("· session started · %d tools", len(ev.Tools)))
		}
		return nil

	case "result":
		p.result = ev.Result
		took := time.Duration(ev.DurationMS) * time.Millisecond
		// The spend counts even when the agent ends badly: the tokens were burned
		// just the same.
		u, byModel := ev.spent()
		p.usage.add(u)
		// add sums tokens and cost, not the name: the closing model is the one from
		// the first final event that brought it.
		if p.usage.Model == "" {
			p.usage.Model = u.Model
		}
		if byModel == nil {
			p.unsplit = true
		}
		p.byModel = append(p.byModel, byModel...)
		if ev.IsError || ev.Subtype != "success" {
			return one(fmt.Sprintf("✗ the agent ended with an error (%s)", ev.Subtype))
		}
		line := fmt.Sprintf("✓ done in %s", took.Round(time.Second))
		if t := p.usage.Total(); t > 0 {
			line += " · " + FormatTokens(t) + " tokens"
		}
		// The same price the Runner settles on (priced over the whole run), not
		// the event's own total_cost_usd: with a price table they differ, and the
		// log was the one place showing the other number (BUG-QA-01).
		if c := priced(p.usage, p.models(), p.pricing).CostUSD; c > 0 {
			line += fmt.Sprintf(" · $%.2f", c)
		}
		return one(line)

	case "assistant", "user":
		if u := ev.Message.Usage; ev.Type == "assistant" {
			p.live.add(Usage{
				InputTokens:  u.InputTokens,
				OutputTokens: u.OutputTokens,
				CacheWrite:   u.CacheWrite,
				CacheRead:    u.CacheRead,
			})
		}
		var blocks []contentBlock
		if err := json.Unmarshal(ev.Message.Content, &blocks); err != nil {
			return nil // plain text content: nothing useful to show
		}
		var out []logEntry
		for _, b := range blocks {
			out = append(out, p.block(who, ev.Type == "assistant", b)...)
		}
		return out
	}
	return nil
}

func (p *streamParser) block(who string, fromAssistant bool, b contentBlock) []logEntry {
	entry := func(text string) logEntry { return logEntry{Agent: who, Text: text} }

	switch b.Type {
	case "text":
		text := strings.TrimSpace(b.Text)
		if text == "" {
			return nil
		}
		if !fromAssistant {
			// Text in a user event is the brief handed to the sub-agent. It
			// works as the first line of its terminal — "this is what they
			// asked for" — but whole it's the entire skill in a little box.
			return []logEntry{entry(truncateRunes(firstLine(text), 200))}
		}
		// Only the main agent's text becomes the report: the sub-agent's
		// already arrives consolidated in the call's result.
		if who == "" {
			p.texts = append(p.texts, text)
		}
		var out []logEntry
		for _, l := range strings.Split(text, "\n") {
			out = append(out, entry(l))
		}
		return out

	case "tool_use":
		p.rememberSubagent(b)
		return []logEntry{entry("→ " + toolSummary(b.Name, b.Input))}

	case "tool_result":
		// The whole result is noise — only what went wrong deserves a line.
		if !b.IsError {
			return nil
		}
		return []logEntry{entry("  ✗ " + firstLine(rawText(b.Content)))}
	}
	// thinking and whatever else comes: kept out of the log.
	return nil
}

// rememberSubagent keeps the name of the sub-agent a call started, so the
// lines coming from inside it can tell whose they are.
//
// What decides is not the tool's name — it's called `Agent` in one version and
// `Task` in another — but having a `subagent_type` in the input: that's what
// tells "started an agent" apart from "read a file".
func (p *streamParser) rememberSubagent(b contentBlock) {
	if b.ID == "" {
		return
	}
	name := str(b.Input["subagent_type"])
	if name == "" && !isAgentTool(b.Name) {
		return
	}
	if name == "" {
		name = str(b.Input["description"])
	}
	if name == "" {
		name = "sub-agent"
	}
	p.register(b.ID, truncateRunes(name, 40))
}

// register records the name, breaking ties when the same lens starts twice:
// two `exploit-digger` in parallel are two terminals, not one.
func (p *streamParser) register(id, name string) {
	if p.subagents == nil {
		p.subagents = map[string]string{}
	}
	if _, já := p.subagents[id]; já {
		return
	}
	final := name
	for n := 2; p.nameTaken(final); n++ {
		final = fmt.Sprintf("%s %d", name, n)
	}
	p.subagents[id] = final
}

func (p *streamParser) nameTaken(name string) bool {
	for _, v := range p.subagents {
		if v == name {
			return true
		}
	}
	return false
}

// subagentName is whose lines come out of a call. A call the parser didn't see
// being born — lost event, different format — still gets its own name by id, so
// two unknowns don't become one.
func (p *streamParser) subagentName(parentID string) string {
	if name, ok := p.subagents[parentID]; ok {
		return name
	}
	p.register(parentID, "sub-agent")
	return p.subagents[parentID]
}

// isAgentTool recognizes the tool that starts a sub-agent by the names it has
// had.
func isAgentTool(name string) bool {
	switch name {
	case "Agent", "Task":
		return true
	}
	return false
}

// toolSummary sums up a tool call in one line: the name and the argument that
// says what it's doing.
func toolSummary(name string, input map[string]any) string {
	if name == "" {
		name = "?"
	}
	if isAgentTool(name) {
		who := str(input["subagent_type"])
		what := str(input["description"])
		switch {
		case who != "" && what != "":
			return fmt.Sprintf("Agent(%s): %s", who, what)
		case who != "":
			return "Agent(" + who + ")"
		}
		return "Agent: " + truncateRunes(what, 70)
	}
	for _, key := range []string{"file_path", "notebook_path", "path", "command", "pattern", "url", "query", "skill", "description", "prompt"} {
		if v := str(input[key]); v != "" {
			arg := truncateRunes(strings.ReplaceAll(v, "\n", " "), 90)
			if key == "pattern" {
				if in := str(input["path"]); in != "" {
					arg += " em " + in
				}
			}
			return name + " " + arg
		}
	}
	return name
}

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// rawText pulls the text out of content that can be either a string or a list
// of blocks.
func rawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, " ")
	}
	return string(raw)
}

// readLimits keeps the quota that came in the event. A window Claude doesn't
// send stays zeroed — the format may gain others, and guessing wouldn't help.
func (p *streamParser) readLimits(ev streamEvent) {
	if ev.RateLimit == nil {
		return
	}
	l := Limits{Status: ev.RateLimit.Status, At: time.Now()}
	for nome, w := range ev.RateLimit.UnifiedWindows {
		janela := Window{Utilization: w.Utilization}
		if w.ResetsAt > 0 {
			janela.ResetsAt = time.Unix(w.ResetsAt, 0)
		}
		switch nome {
		case "five_hour":
			l.Session = janela
		case "seven_day":
			l.Week = janela
		}
	}
	p.limits = l
}

// spend is the agent's spend: the settled one, when the final event arrived, or
// the partial count of the messages so far — which is what the page shows while
// the review runs.
func (p *streamParser) spend() Usage {
	if !p.usage.Empty() {
		return p.usage
	}
	return p.live
}

// models is the closed spend split by model, or nil when the split does not
// cover the total — the partial count, or a final event without modelUsage.
func (p *streamParser) models() []Usage {
	if p.usage.Empty() || p.unsplit {
		return nil
	}
	return p.byModel
}

// report is the agent's report: the text of the final event or, if it did
// not come, what the assistant wrote along the way.
func (p *streamParser) report() string {
	if r := strings.TrimSpace(p.result); r != "" {
		return r
	}
	return strings.TrimSpace(strings.Join(p.texts, "\n\n"))
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
