package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/beroni/bazel/internal/config"
	"github.com/beroni/bazel/internal/gh"
)

// The queue outlives the process as a file: what was pending when Bazel stopped
// comes back on the next start, paused. Nothing runs or spends quota on its own
// after a restart: a job waits for a click, and a publication never comes back,
// because it needs someone who has just read what goes out. An agent that may
// publish comes back only if it never started, and its click asks first.
//
// The file is plain JSON and safe to delete.
const queueFileName = "queue.json"

// queuedJob is one pending job as the file keeps it. The whole PR goes in, so
// the card can be drawn with no call to GitHub.
type queuedJob struct {
	// Repo is apart because gh.PR leaves it out of its JSON: gh does not send it.
	Repo    string `json:"repo"`
	PR      gh.PR  `json:"pr"`
	Agent   string `json:"agent"`
	History bool   `json:"history,omitempty"`
	Mine    bool   `json:"mine,omitempty"`
}

// pendingLocked is what the file must hold. An agent that may publish and has
// started may already have posted, and running it again would post twice. That
// is PublishesBy, the rule of whoever refuses, and not Posts, which is only what
// was declared. Call with m.mu held.
func (m *Manager) pendingLocked() []queuedJob {
	out := []queuedJob{}
	for _, id := range m.order {
		j := m.jobs[id]
		switch {
		case j.publish != nil:
		case j.State == StatePaused, j.State == StateQueued, j.State == StateRunning && j.Choice.PublishesBy() == "":
			out = append(out, queuedJob{Repo: j.PR.Repo, PR: j.PR, Agent: j.Choice.Name, History: j.History, Mine: j.Mine})
		}
	}
	return out
}

// saveQueue writes the file when what is pending changed. Once the server is
// going down it stops: what the shutdown is killing is what must come back.
func (m *Manager) saveQueue() {
	if m.queueFile == "" || m.ctx.Err() != nil {
		return
	}
	m.queueMu.Lock()
	defer m.queueMu.Unlock()
	m.mu.Lock()
	pending := m.pendingLocked()
	m.mu.Unlock()
	data, err := json.MarshalIndent(map[string]any{"jobs": pending}, "", "  ")
	if err != nil || bytes.Equal(data, m.queueSaved) {
		return
	}
	if err := writeQueue(m.queueFile, data); err != nil {
		fmt.Fprintf(os.Stderr, "bazel: could not save the queue: %v\n", err)
		return
	}
	m.queueSaved = data
}

func writeQueue(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".queue-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// RestoreQueue brings back what the file holds, paused. It calls nothing and
// runs nothing. An entry whose agent is gone from the config is dropped with a
// line on the terminal, and a file that cannot be read is left alone for
// whoever wants to look at it.
func (m *Manager) RestoreQueue(path string) {
	m.queueFile = path
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var saved struct {
		Jobs []queuedJob `json:"jobs"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		fmt.Fprintf(os.Stderr, "bazel: %s is not readable, starting with an empty queue: %v\n", path, err)
		_ = os.Rename(path, fmt.Sprintf("%s.corrupt-%d", path, time.Now().UnixNano()))
		return
	}
	m.mu.Lock()
	for _, q := range saved.Jobs {
		q.PR.Repo = q.Repo
		choice, err := m.choiceFor(q)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bazel: dropped %s from the queue: %v\n", q.PR.Key(), err)
			continue
		}
		job := m.newJobLocked(q.PR, q.Mine, choice, q.History)
		job.State = StatePaused
	}
	m.mu.Unlock()
	m.saveQueue()
}

func (m *Manager) choiceFor(q queuedJob) (config.Choice, error) {
	if q.History {
		return m.cfg.HistoryChoice(), m.cfg.ValidateHistoryAgent()
	}
	ch, err := m.cfg.ChoiceByName(q.Agent)
	if err == nil && len(ch.Steps) == 0 {
		err = config.ErrNoAgents
	}
	return ch, err
}

// Resume sends a paused job to the queue: the click that restart left to you.
func (m *Manager) Resume(id string) (jobView, error) {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return jobView{}, refuse(http.StatusNotFound, fmt.Errorf("job %q does not exist", id))
	}
	if job.State != StatePaused {
		m.mu.Unlock()
		return jobView{}, refuse(http.StatusConflict, errors.New("that job is not paused"))
	}
	m.resumeLocked(job)
	m.mu.Unlock()
	m.publish(job)
	return m.mustView(id), nil
}

func (m *Manager) resumeLocked(job *Job) {
	job.State = StateQueued
	job.QueuedAt = time.Now()
	_ = m.sendLocked(job)
}
