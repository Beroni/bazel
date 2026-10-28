package agent

import (
	"fmt"
)

// outputAdapter traduz a saída de um agente para o contrato interno do Runner.
// Usage é sempre um retrato acumulado do passo atual, nunca um delta.
type outputAdapter interface {
	line(string) []logEntry
	usage() Usage
	limits() Limits
	report() string
	err() error
}

func newOutputAdapter(format string, args []string) (outputAdapter, error) {
	if format == "" {
		if isStreamJSON(args) {
			format = "claude-stream"
		} else {
			format = "plain"
		}
	}
	switch format {
	case "claude-stream":
		return &claudeAdapter{}, nil
	case "codex-json":
		return &codexAdapter{model: "codex"}, nil
	case "grok-stream":
		return &grokAdapter{model: "grok"}, nil
	case "plain":
		return &plainAdapter{}, nil
	default:
		return nil, fmt.Errorf("unsupported agent output format %q", format)
	}
}

type claudeAdapter struct{ streamParser }

func (a *claudeAdapter) usage() Usage   { return a.spend() }
func (a *claudeAdapter) limits() Limits { return a.streamParser.limits }
func (a *claudeAdapter) err() error     { return nil }
