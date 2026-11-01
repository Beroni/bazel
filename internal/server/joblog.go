package server

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/beroni/bazel/internal/agent"
)

// jobLog writes one job's whole log to a file, as it arrives. The page keeps a
// window of it (maxLogLines), which is right for the screen and wrong for
// finding out why an agent failed twenty minutes in. A disk that fails costs the
// file, never the review.
type jobLog struct {
	mu sync.Mutex
	f  *os.File
}

// logNameRe is the name Bazel gives a job's log. Retention only ever touches
// these: the folder is the user's choice, and it may hold anything else.
var logNameRe = regexp.MustCompile(`^[0-9a-f]{4}-j[0-9]+-[A-Za-z0-9._-]+\.log$`)

var logUnsafeRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// openJobLog opens the job's file in dir, or returns nil when there is no dir or
// the file cannot be opened. It appends: a publication in the review's own card
// runs as the same job.
func openJobLog(dir, jobID, prKey string) (*jobLog, string) {
	if dir == "" {
		return nil, ""
	}
	name := jobID + "-" + logUnsafeRe.ReplaceAllString(prKey, "-") + ".log"
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bazel: job log: %v\n", err)
		return nil, ""
	}
	return &jobLog{f: f}, path
}

// write appends one line of the agent's. Lines from stdout and stderr arrive
// from two goroutines.
func (l *jobLog) write(e agent.Event) {
	if l == nil || e.Kind != agent.EventLog {
		return
	}
	who := e.Agent
	if who == "" {
		who = e.Name
	}
	if e.Stream == "stderr" {
		who += " stderr"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	if _, err := fmt.Fprintf(l.f, "%s [%s] %s\n", time.Now().Format("15:04:05"), who, e.Text); err != nil {
		fmt.Fprintf(os.Stderr, "bazel: job log: %v\n", err)
		l.f.Close()
		l.f = nil
	}
}

func (l *jobLog) close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}

// pruneJobLogs keeps the newest keep job logs in dir and removes the rest.
func pruneJobLogs(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type logFile struct {
		name string
		at   time.Time
	}
	var logs []logFile
	for _, e := range entries {
		if !e.Type().IsRegular() || !logNameRe.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		logs = append(logs, logFile{e.Name(), info.ModTime()})
	}
	slices.SortFunc(logs, func(a, b logFile) int {
		if c := b.at.Compare(a.at); c != 0 {
			return c
		}
		return strings.Compare(a.name, b.name)
	})
	if len(logs) <= keep {
		return nil
	}
	for _, l := range logs[keep:] {
		if err := os.Remove(filepath.Join(dir, l.name)); err != nil {
			return err
		}
	}
	return nil
}
