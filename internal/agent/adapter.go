package agent

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/beroni/bazel/internal/config"
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

// provider says who a step runs on. The output format is what proves it, and a
// command that speaks none of the known ones is named by itself: a wrapper or a
// CLI Bazel has never heard of still gets a name, with no list to keep here.
func provider(step config.ResolvedAgent) string {
	switch outputFormat(step.Format, step.Args) {
	case "claude-stream":
		return "claude"
	case "codex-json":
		return "codex"
	case "grok-stream":
		return "grok"
	}
	return programName(step.Command)
}

// dateSuffixRe is the release date a model name ends in.
var dateSuffixRe = regexp.MustCompile(`-[0-9]{8}$`)

// attribution is the label of what a step ran on: `claude · opus-4-5`, or just
// the provider when the model is unknown or says nothing more.
func attribution(who, model string) string {
	model = dateSuffixRe.ReplaceAllString(strings.TrimPrefix(model, who+"-"), "")
	if model == "" || model == who {
		return who
	}
	return who + " · " + model
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
func (a *claudeAdapter) err() error     { return a.failure }
