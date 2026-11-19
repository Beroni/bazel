package store

import (
	"fmt"
	"strings"
	"time"
)

// The decisions a person takes on a PR after reading its review. They are
// Bazel's own: nothing here goes to GitHub.
const (
	VerdictApprove = "approve"
	VerdictComment = "approve_with_comments"
	VerdictChanges = "request_changes"
)

const headLine = "- Head sha: "

// slackEmoji is how each decision reads in the summary pasted into Slack.
var slackEmoji = map[string]string{
	VerdictApprove: ":white_check_mark:",
	VerdictComment: ":memo::white_check_mark:",
	VerdictChanges: ":git_change_request:",
}

// ValidVerdict says v is one of the decisions, or "" to clear it.
func ValidVerdict(v string) bool {
	_, ok := slackEmoji[v]
	return ok || v == ""
}

// SlackLine is the summary's line for a PR: its URL and the decision's emoji.
func SlackLine(url, verdict string) string {
	return url + " " + slackEmoji[verdict]
}

// Verdict is a person's decision on a PR, with the head it was taken on: a
// commit pushed after it is a change nobody decided on yet.
type Verdict struct {
	Verdict string    `json:"verdict"`
	Head    string    `json:"head,omitempty"`
	At      time.Time `json:"at"`
}

// Verdicts are the decisions in the index, by its key (lower case).
func (m Marks) Verdicts() map[string]Verdict {
	out := map[string]Verdict{}
	for k, v := range m {
		if v.Verdict != nil {
			out[k] = *v.Verdict
		}
	}
	return out
}

// VerdictOf is the decision on the PR with this key, if there is one.
func (m Marks) VerdictOf(key string) (Verdict, bool) {
	v := m[markKey(key)].Verdict
	if v == nil {
		return Verdict{}, false
	}
	return *v, true
}

// SetVerdict records the decision on a PR, taken on head; "" clears it. It goes
// in the review index, in the same atomic read-modify-write as every mark, so a
// restart keeps it.
func SetVerdict(dir, key, verdict, head string) (*Verdict, error) {
	if !ValidVerdict(verdict) {
		return nil, fmt.Errorf("unknown decision %q", verdict)
	}
	if key == "" {
		return nil, fmt.Errorf("no PR to decide on")
	}
	var out *Verdict
	if verdict != "" {
		out = &Verdict{Verdict: verdict, Head: head, At: time.Now()}
	}
	err := update(dir, key, func(m Mark) Mark {
		m.Verdict = out
		// A PR decided on with no review on record (its mark trimmed away) still
		// needs a time, or it is the first to go when the index is cut.
		if m.At.IsZero() {
			m.At = time.Now()
		}
		return m
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// HeadOf is the head sha a saved review was written on, or "" in a file that
// does not say.
func HeadOf(file string) string {
	head, _, _, _ := SavedParts(file)
	for _, line := range strings.Split(head, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), headLine); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
