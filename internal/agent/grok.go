package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// grokAdapter translates `grok -p --output-format streaming-json` events.
// The stream is deliberately permissive because Grok adds capability events
// frequently; only text, tool progress, usage, and terminal state matter here.
type grokAdapter struct {
	reportText strings.Builder
	live       Usage
	final      Usage
	terminal   error
	tools      map[string]string
	model      string
}

type grokEvent struct {
	Type         string     `json:"type"`
	ToolCallID   string     `json:"toolCallId"`
	ToolName     string     `json:"toolName"`
	Data         string     `json:"data"`
	Status       string     `json:"status"`
	Title        string     `json:"title"`
	Message      string     `json:"message"`
	StopReason   string     `json:"stopReason"`
	Usage        *grokUsage `json:"usage"`
	TotalCostUSD float64    `json:"total_cost_usd"`
	RawInput     struct {
		Command     string `json:"command"`
		Description string `json:"description"`
	} `json:"rawInput"`
	RawOutput *struct {
		ExitCode *int   `json:"exit_code"`
		Command  string `json:"command"`
		Output   string `json:"output_for_prompt"`
	} `json:"rawOutput"`
}

type grokUsage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	CacheReadTokens int `json:"cache_read_input_tokens"`
	CacheCreation   int `json:"cache_creation_input_tokens"`
	ReasoningTokens int `json:"reasoning_tokens"`
	TotalTokens     int `json:"total_tokens"`
}

func (u *grokUsage) value(cost float64) Usage {
	if u == nil {
		return Usage{CostUSD: cost}
	}
	// Grok reports reasoning as part of output_tokens; adding it separately
	// would double-count the terminal total it also emits.
	return Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheRead: u.CacheReadTokens, CacheWrite: u.CacheCreation, CostUSD: cost}
}

func (a *grokAdapter) line(raw string) []logEntry {
	var ev grokEvent
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &ev); err != nil || ev.Type == "" {
		return []logEntry{{Text: raw}}
	}
	one := func(text string) []logEntry {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []logEntry{{Text: text}}
	}
	switch ev.Type {
	case "text":
		a.reportText.WriteString(ev.Data)
		// Grok emits this event one token at a time. Keeping token fragments in
		// the terminal makes every word a separate line; retain them for the
		// final report and let tool events carry the useful live progress.
		return nil
	case "usage":
		u := ev.Usage.value(0)
		a.live.add(u)
	case "error":
		msg := strings.TrimSpace(ev.Message)
		if msg == "" {
			msg = "Grok error"
		}
		a.terminal = fmt.Errorf("%s", msg)
		return one("✗ " + msg)
	case "tool_call":
		command := ev.RawInput.Command
		if command == "" {
			command = ev.Title
		}
		if command == "" {
			command = ev.ToolName
		}
		if ev.ToolCallID != "" {
			if a.tools == nil {
				a.tools = make(map[string]string)
			}
			kind := ev.ToolName
			if kind == "" {
				kind = ev.Title
			}
			if kind == "" {
				kind = ev.RawInput.Command
			}
			a.tools[ev.ToolCallID] = kind
		}
		return one("→ " + truncateRunes(strings.ReplaceAll(command, "\n", " "), 120))
	case "tool_call_update":
		if ev.Status != "completed" && ev.Status != "failed" {
			return nil
		}
		if ev.RawOutput != nil && ev.RawOutput.ExitCode != nil && *ev.RawOutput.ExitCode == 1 && strings.EqualFold(a.tools[ev.ToolCallID], "grep") {
			return one("  · grep: no matches")
		}
		if ev.Status == "failed" || (ev.RawOutput != nil && ev.RawOutput.ExitCode != nil && *ev.RawOutput.ExitCode != 0) {
			line := "  ✗ command failed"
			if ev.RawOutput != nil {
				if detail := strings.TrimSpace(firstLine(ev.RawOutput.Output)); detail != "" {
					line += ": " + truncateRunes(detail, 180)
				}
			}
			return one(line)
		}
		return one("  ✓ command finished")
	case "end":
		a.final = ev.Usage.value(ev.TotalCostUSD)
		// The CLI uses "cancelled" when its own --max-turns budget expires.
		// Its stderr carries the useful "max turns reached" explanation, which
		// Runner turns into a single clear job error.
		if ev.StopReason == "cancelled" {
			return nil
		}
		if ev.StopReason != "" && ev.StopReason != "end_turn" {
			a.terminal = fmt.Errorf("Grok ended: %s", ev.StopReason)
			return one("✗ " + a.terminal.Error())
		}
		line := "✓ done"
		if total := a.final.Total(); total > 0 {
			line += " · " + FormatTokens(total) + " tokens"
		}
		if a.final.CostUSD > 0 {
			line += fmt.Sprintf(" · $%.2f", a.final.CostUSD)
		}
		return one(line)
	}
	return nil
}

func (a *grokAdapter) usage() Usage {
	u := a.final
	if u.Empty() {
		u = a.live
	}
	u.Model = a.model
	return u
}
func (a *grokAdapter) limits() Limits { return Limits{} }
func (a *grokAdapter) report() string { return strings.TrimSpace(a.reportText.String()) }
func (a *grokAdapter) err() error     { return a.terminal }
