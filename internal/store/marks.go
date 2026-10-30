package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/beroni/bazel/internal/agent"
	"github.com/beroni/bazel/internal/gh"
)

// marksFile is the index of what has already been reviewed, next to the reviews.
const marksFile = "revisados.json"

// maxMarks caps the index: a PR reviewed months ago no longer changes anything on screen.
const maxMarks = 500

// marksMu serializes the index's read-modify-write. The web interface
// finishes two reviews at the same time and both want to write.
var marksMu sync.Mutex

// Mark is the record of a review already done on a PR.
type Mark struct {
	Agent string    `json:"agent"`
	At    time.Time `json:"at"`
	// HeadOid is the commit that was at the tip of the PR when it was reviewed —
	// comparing it with the current one is what reveals a change after the review.
	HeadOid string `json:"head_oid"`
	File    string `json:"file,omitempty"`
	Posted  bool   `json:"posted,omitempty"`
}

// Marks is the index of reviews by PR key ("owner/repo#123").
type Marks map[string]Mark

// Status is how a PR shows up in the list after being reviewed.
type Status struct {
	Reviewed bool
	At       time.Time
	Agent    string
	Posted   bool
	// Changed says the PR got a new commit after the review — what is
	// on GitHub is no longer what was reviewed.
	Changed bool
}

// Status looks up a PR in the index.
func (m Marks) Status(pr gh.PR) Status {
	mark, ok := m[pr.Key()]
	if !ok {
		return Status{}
	}
	return Status{
		Reviewed: true,
		At:       mark.At,
		Agent:    mark.Agent,
		Posted:   mark.Posted,
		// Without the recorded commit we can't claim it changed — better
		// not to alarm than to alarm wrongly.
		Changed: mark.HeadOid != "" && pr.HeadRefOid != "" && mark.HeadOid != pr.HeadRefOid,
	}
}

// Age summarizes when the review was done ("2h", "3d").
func (s Status) Age(now time.Time) string {
	d := now.Sub(s.At)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}

// LoadMarks reads the index. A missing or corrupted file is an empty index:
// this is list decoration, it must not bring down the PR listing.
func LoadMarks(dir string) Marks {
	marksMu.Lock()
	defer marksMu.Unlock()
	return loadMarksLocked(dir)
}

func loadMarksLocked(dir string) Marks {
	data, err := os.ReadFile(filepath.Join(dir, marksFile))
	if err != nil {
		return Marks{}
	}
	var m Marks
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		return Marks{}
	}
	return m
}

// MarkReviewed records that a PR was reviewed, with the commit that was at the
// tip at the time. file is the saved markdown, if any.
func MarkReviewed(dir string, res agent.Result, file string) error {
	return update(dir, res.PR.Key(), func(m Mark) Mark {
		return Mark{
			Agent:   res.Agent,
			At:      time.Now(),
			HeadOid: res.PR.HeadRefOid,
			File:    file,
			// An agent that publishes on its own has already left the review on the PR.
			Posted: res.Posts || m.Posted,
		}
	})
}

// MarkPosted notes that the review for that PR went to GitHub.
func MarkPosted(dir, key string) error {
	return update(dir, key, func(m Mark) Mark {
		m.Posted = true
		if m.At.IsZero() {
			m.At = time.Now()
		}
		return m
	})
}

func update(dir, key string, fn func(Mark) Mark) error {
	if key == "" {
		return nil
	}
	marksMu.Lock()
	defer marksMu.Unlock()

	marks := loadMarksLocked(dir)
	marks[key] = fn(marks[key])
	trim(marks)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(marks, "", "  ")
	if err != nil {
		return err
	}
	// Write to a temporary file and rename: a review interrupted in the middle
	// of the write must not leave the index half-written.
	tmp := filepath.Join(dir, marksFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, marksFile))
}

// trim drops the oldest records when the index goes over the cap.
func trim(m Marks) {
	if len(m) <= maxMarks {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]].At.Before(m[keys[j]].At) })
	for _, k := range keys[:len(m)-maxMarks] {
		delete(m, k)
	}
}
