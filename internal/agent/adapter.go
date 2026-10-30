package agent

import (
	"fmt"
)

// outputAdapter translates an agent's output into the Runner's internal contract.
// Usage is always a cumulative snapshot of the current step, never a delta.
type outputAdapter interface {
	line(string) []logEntry
	usage() Usage
	limits() Limits
	report() string
	err() error
}

// outputFormat is the format a step speaks: the declared one or, when none is,
// what its args ask for.
func outputFormat(format string, args []string) string {
	switch {
	case format != "":
		return format
	case isStreamJSON(args):
		return "claude-stream"
	}
	return "plain"
}

func newOutputAdapter(format string, args []string) (outputAdapter, error) {
	switch format = outputFormat(format, args); format {
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
