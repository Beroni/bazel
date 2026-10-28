package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// codexAdapter handles the documented JSONL protocol from `codex exec --json`.
// Unknown event and item fields are deliberately ignored so newer CLI versions
// remain usable; malformed lines still appear in the live log.
type codexAdapter struct {
	reportText string
	spend      Usage
	terminal   error
	messages   map[string]bool
	model      string
}

type codexEvent struct {
	Type    string    `json:"type"`
	Item    codexItem `json:"item"`
	Message string    `json:"message"`
	Usage   *struct {
		InputTokens           int `json:"input_tokens"`
		CachedInputTokens     int `json:"cached_input_tokens"`
		OutputTokens          int `json:"output_tokens"`
		ReasoningOutputTokens int `json:"reasoning_output_tokens"`
	} `json:"usage"`
	Error json.RawMessage `json:"error"`
}

type codexItem struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	Status           string `json:"status"`
	Command          string `json:"command"`
	AggregatedOutput string `json:"aggregated_output"`
	Text             string `json:"text"`
}

func (a *codexAdapter) line(raw string) []logEntry {
	var ev codexEvent
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
	case "thread.started":
		return one("· session started")
	case "turn.started":
		return one("· turn started")
	case "turn.completed":
		if ev.Usage != nil {
			// Codex's input includes the cached portion. Store the two parts
			// separately so Usage.Total does not count cached input twice.
			input := ev.Usage.InputTokens - ev.Usage.CachedInputTokens
			if input < 0 {
				input = 0
			}
			a.spend = Usage{InputTokens: input, CacheRead: ev.Usage.CachedInputTokens, OutputTokens: ev.Usage.OutputTokens}
		}
		if total := a.spend.Total(); total > 0 {
			return one("✓ done · " + FormatTokens(total) + " tokens")
		}
		return one("✓ done")
	case "turn.failed":
		msg := ev.errorMessage()
		if msg == "" {
			msg = "Codex turn failed"
		}
		a.terminal = fmt.Errorf("%s", msg)
		return one("✗ " + msg)
	case "error":
		msg := ev.errorMessage()
		if msg == "" {
			msg = "Codex error"
		}
		return one("✗ " + msg)
	case "item.started":
		if ev.Item.Type == "command_execution" {
			return one("→ " + truncateRunes(strings.ReplaceAll(ev.Item.Command, "\n", " "), 120))
		}
	case "item.completed":
		switch ev.Item.Type {
		case "agent_message":
			if ev.Item.ID != "" && a.messages[ev.Item.ID] {
				return nil
			}
			if a.messages == nil {
				a.messages = map[string]bool{}
			}
			a.messages[ev.Item.ID] = true
			text := strings.TrimSpace(ev.Item.Text)
			if text == "" {
				return nil
			}
			a.reportText = text
			out := make([]logEntry, 0, strings.Count(text, "\n")+1)
			for _, line := range strings.Split(text, "\n") {
				out = append(out, logEntry{Text: line})
			}
			return out
		case "command_execution":
			if ev.Item.Status == "failed" {
				line := "  ✗ command failed"
				if detail := strings.TrimSpace(firstLine(ev.Item.AggregatedOutput)); detail != "" {
					line += ": " + truncateRunes(detail, 180)
				}
				return one(line)
			}
			return one("  ✓ command finished")
		}
	}
	return nil
}

func (ev codexEvent) errorMessage() string {
	if message := strings.TrimSpace(ev.Message); message != "" {
		return message
	}
	return codexError(ev.Error)
}

func (a *codexAdapter) usage() Usage {
	u := a.spend
	u.Model = a.model
	return u
}
func (a *codexAdapter) limits() Limits { return Limits{} }
func (a *codexAdapter) report() string { return a.reportText }
func (a *codexAdapter) err() error     { return a.terminal }

func codexError(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return strings.TrimSpace(obj.Message)
	}
	return strings.TrimSpace(string(raw))
}
