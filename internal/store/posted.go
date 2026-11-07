package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Posted is what a saved review knows of its own trip to the PR. It lives next
// to the file, as <file>.posted, so that a restart remembers it: the index says
// only that something of the PR went, and memory forgets.
type Posted struct {
	// How is "inline" (the post agent) or "comment".
	How  string    `json:"how"`
	At   time.Time `json:"at"`
	Head string    `json:"head,omitempty"`
}

func postedPath(file string) string { return file + ".posted" }

// MarkFilePosted records that the review in file went to the PR. It is written
// whole or not at all, before the door that sent it answers.
func MarkFilePosted(file, how, head string) error {
	data, err := json.Marshal(Posted{How: how, At: time.Now(), Head: head})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".posted-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), postedPath(file))
}

// PostedOf reads what MarkFilePosted left for file. Only a record that is not
// there means the review did not go: one that cannot be read, for any other
// reason, counts as published, which is the safe side.
func PostedOf(file string) (Posted, bool) {
	data, err := os.ReadFile(postedPath(file))
	if errors.Is(err, fs.ErrNotExist) {
		return Posted{}, false
	}
	if err != nil {
		return Posted{How: "unknown"}, true
	}
	var p Posted
	if json.Unmarshal(data, &p) != nil {
		return Posted{How: "unknown"}, true
	}
	return p, true
}

// The claim: <file>.posting, made with O_EXCL before any door calls gh, so two
// doors, or two Bazels on the same folder, cannot both send the same review.
// It becomes <file>.posted when the review went, and goes away when gh failed.
// One left by a process that is gone is an unknown outcome: the review may be
// on the PR or not, and only a person can say.
var (
	// ErrPosted is a review that went to the PR.
	ErrPosted = errors.New("already published")
	// ErrClaimBusy is a review another live process is sending now.
	ErrClaimBusy = errors.New("another Bazel is publishing this review right now")
	// ErrClaimOrphan is a send that started and never said how it ended.
	ErrClaimOrphan = errors.New("this review may already be on the PR: a publication of it started and never finished")
)

// instance tells this process's claims from those of a Bazel that ran before
// with the same pid.
var instance = fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())

type claimRecord struct {
	PID      int       `json:"pid"`
	Instance string    `json:"instance"`
	At       time.Time `json:"at"`
}

// Claim is the right to send one saved review to the PR.
type Claim struct{ path string }

func postingPath(file string) string { return file + ".posting" }

// ClaimPosting takes the claim on file. again is a person's yes to sending it
// although a send of it may have happened: it clears an orphan claim, never a
// live one. A claim of this very process with no door holding it is a leftover
// and counts as an orphan too.
func ClaimPosting(file string, again bool) (*Claim, error) {
	if _, ok := PostedOf(file); ok {
		return nil, ErrPosted
	}
	for try := 0; try < 2; try++ {
		f, err := os.OpenFile(postingPath(file), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			data, _ := json.Marshal(claimRecord{PID: os.Getpid(), Instance: instance, At: time.Now()})
			_, werr := f.Write(data)
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				_ = os.Remove(postingPath(file))
				return nil, werr
			}
			return &Claim{path: file}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		var rec claimRecord
		if data, rerr := os.ReadFile(postingPath(file)); rerr == nil {
			_ = json.Unmarshal(data, &rec)
		}
		if rec.Instance != instance && rec.PID > 0 && processAlive(rec.PID) {
			return nil, ErrClaimBusy
		}
		if !again {
			return nil, ErrClaimOrphan
		}
		_ = os.Remove(postingPath(file))
	}
	return nil, ErrClaimOrphan
}

// Done records that the review went, then lets go of the claim.
func (c *Claim) Done(how, head string) error {
	if c == nil {
		return nil
	}
	err := MarkFilePosted(c.path, how, head)
	if err == nil {
		_ = os.Remove(postingPath(c.path))
	}
	return err
}

// Release lets go of the claim of a send that did not happen.
func (c *Claim) Release() {
	if c != nil {
		_ = os.Remove(postingPath(c.path))
	}
}

// postRecordLine marks, in a saved review's header, the files that keep their
// own record of going to the PR.
const postRecordLine = "- Publish record: kept"

// KeepsPostRecord says the saved review at file keeps its own record: no
// .posted means it did not go. A file without the line is from before, and
// only the index's word about its PR is left.
func KeepsPostRecord(file string) bool {
	data, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	head, _, _, _ := SavedParts(string(data))
	for _, line := range strings.Split(head, "\n") {
		if strings.TrimSpace(line) == postRecordLine {
			return true
		}
	}
	return false
}
