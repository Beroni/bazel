package store

import (
	"encoding/json"
	"errors"
	"io"
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

// The claim: <file>.posting, held with an exclusive lock for the whole of one
// send, by whichever door and whichever Bazel. The lock is what says someone is
// sending now; a process that dies lets go of it, so no pid is trusted. What
// the file says is how the last send went, read and changed only under the
// lock: empty is nothing pending, "sending" is a send that never said how it
// ended, "unknown" is one that failed after gh may have reached GitHub. Either
// of those asks a person before sending again. The file is truncated, never
// removed: another process may hold it open.
var (
	// ErrPosted is a review that went to the PR.
	ErrPosted = errors.New("already published")
	// ErrClaimBusy is a review another door is sending now.
	ErrClaimBusy = errors.New("this review is being published right now")
	// ErrClaimOrphan is a send whose outcome is not known.
	ErrClaimOrphan = errors.New("this review may already be on the PR: an earlier publication of it did not say how it ended, and sending again may post it twice")
)

// Claim is the right to send one saved review to the PR, held until Done,
// Release or Unknown.
type Claim struct {
	path string
	f    *os.File
}

func postingPath(file string) string { return file + ".posting" }

// ClaimPosting takes the claim on file. again is a person's yes to sending it
// although an earlier send of it may have reached the PR; it never overrides a
// send in progress or a recorded one.
func ClaimPosting(file string, again bool) (*Claim, error) {
	f, err := os.OpenFile(postingPath(file), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if !tryLock(f) {
		f.Close()
		return nil, ErrClaimBusy
	}
	c := &Claim{path: file, f: f}
	// Under the lock: what an earlier holder finished is there to see.
	if _, ok := PostedOf(file); ok {
		c.close()
		return nil, ErrPosted
	}
	state, err := io.ReadAll(io.NewSectionReader(f, 0, 1<<10))
	if err != nil {
		c.close()
		return nil, err
	}
	if strings.TrimSpace(string(state)) != "" && !again {
		c.close()
		return nil, ErrClaimOrphan
	}
	return c, nil
}

// CheckPosting says what ClaimPosting would, and takes nothing: for a door that
// queues the send for later.
func CheckPosting(file string, again bool) error {
	c, err := ClaimPosting(file, again)
	if err != nil {
		return err
	}
	c.close()
	return nil
}

// Sending marks, before gh runs, that a send is out: a process that dies now
// leaves it, and the next attempt asks.
func (c *Claim) Sending() error {
	return c.write("sending " + time.Now().Format(time.RFC3339) + "\n")
}

// Done records that the review went, then clears the claim.
func (c *Claim) Done(how, head string) error {
	if c == nil {
		return nil
	}
	defer c.close()
	if err := MarkFilePosted(c.path, how, head); err != nil {
		return err
	}
	return c.write("")
}

// Release clears the claim of a send that did not reach GitHub.
func (c *Claim) Release() {
	if c == nil {
		return
	}
	_ = c.write("")
	c.close()
}

// Unknown leaves word that the send failed in a way that may have reached
// GitHub all the same: the next attempt asks.
func (c *Claim) Unknown(why string) {
	if c == nil {
		return
	}
	_ = c.write("unknown " + time.Now().Format(time.RFC3339) + " " + strings.Join(strings.Fields(why), " ") + "\n")
	c.close()
}

func (c *Claim) write(state string) error {
	if c == nil || c.f == nil {
		return nil
	}
	if err := c.f.Truncate(0); err != nil {
		return err
	}
	if _, err := c.f.WriteAt([]byte(state), 0); err != nil {
		return err
	}
	return c.f.Sync()
}

func (c *Claim) close() {
	if c != nil && c.f != nil {
		unlock(c.f)
		c.f.Close()
		c.f = nil
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
