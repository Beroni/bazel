package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/beroni/bazel/internal/skills"
)

// inlineCheck runs over a saved review the reader bazel-post-report publishes
// with, and says why it would refuse it, or "". Inline publishing is where a
// report out of format is found out, and that is the last step: finding out
// when the review lands lets the user paste it as a comment instead. A machine
// without jq cannot publish inline either; the check then says nothing.
var inlineCheck = func(ctx context.Context, path string) string {
	jq, err := exec.LookPath("jq")
	if err != nil {
		return ""
	}
	// The program goes as an argument: no file of Bazel's own left anywhere.
	script := string(skills.ParseReportScript())
	if script == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, jq, "-Rs", script, path)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "it could not be read"
	}
	var parsed struct {
		Deny []string `json:"deny"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil || parsed.Deny == nil {
		return "it could not be read"
	}
	return joinUnique(parsed.Deny)
}

func joinUnique(items []string) string {
	seen := map[string]bool{}
	var out []string
	for _, it := range items {
		if it = strings.TrimSpace(it); it != "" && !seen[it] {
			seen[it] = true
			out = append(out, it)
		}
	}
	return strings.Join(out, "; ")
}

// inlineChecks remembers the answer per saved file and its size and time: the
// Saved tab asks on every open, and the file does not change under it.
type inlineChecks struct {
	mu   sync.Mutex
	seen map[string]inlineSeen
}

type inlineSeen struct {
	size  int64
	mod   time.Time
	issue string
}

func (c *inlineChecks) get(ctx context.Context, path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("not a file")
	}
	c.mu.Lock()
	if s, ok := c.seen[path]; ok && s.size == info.Size() && s.mod.Equal(info.ModTime()) {
		c.mu.Unlock()
		return s.issue, nil
	}
	c.mu.Unlock()
	issue := inlineCheck(ctx, path)
	c.mu.Lock()
	if c.seen == nil {
		c.seen = map[string]inlineSeen{}
	}
	if len(c.seen) > 512 {
		c.seen = map[string]inlineSeen{}
	}
	c.seen[path] = inlineSeen{info.Size(), info.ModTime(), issue}
	c.mu.Unlock()
	return issue, nil
}
