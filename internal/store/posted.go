package store

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// PostedOf reads what MarkFilePosted left for file. A record that cannot be
// read still says the review went: refusing a second post is the safe side.
func PostedOf(file string) (Posted, bool) {
	data, err := os.ReadFile(postedPath(file))
	if err != nil {
		return Posted{}, false
	}
	var p Posted
	if json.Unmarshal(data, &p) != nil {
		return Posted{How: "unknown"}, true
	}
	return p, true
}
