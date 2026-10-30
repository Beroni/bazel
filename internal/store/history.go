package store

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// History is a PR's story as the History tab draws it. It is parsed from the
// markdown the history skill writes, in this shape (the contract agreed with
// the skill that produces it):
//
//	Commits read: 4
//
//	## What it does
//	One paragraph on what the PR changes and why.
//
//	## Decisions
//	### 1. Parse grok's stream instead of wrapping the CLI output
//	- Commit: a1f3c9e · description
//	Grok already emits JSON events; reading them keeps tool calls structured.
//	- Rejected: Plain-text adapter (like plain.go) — loses tool calls.
//
//	## Still open
//	- What happens when two concurrent jobs share the same GROK_HOME?
//
// "Commits read" is optional, so are Rejected lines (any number) and Still
// open. Headings match ignoring case. Anything the parser does not recognize
// stays in Rest, so a report in another shape still shows.
type History struct {
	CommitsRead int        `json:"commits_read,omitempty"`
	WhatItDoes  string     `json:"what_it_does"`
	Decisions   []Decision `json:"decisions"`
	StillOpen   []string   `json:"still_open"`
	Rest        string     `json:"rest,omitempty"`
}

// Decision is one numbered decision of the PR's story.
type Decision struct {
	N        int      `json:"n"`
	Title    string   `json:"title"`
	Sha      string   `json:"sha,omitempty"`
	Source   string   `json:"source,omitempty"`
	Body     string   `json:"body"`
	Rejected []string `json:"rejected"`
}

var (
	reCommitsRead = regexp.MustCompile(`(?im)^\s*commits read:\s*(\d+)\s*$`)
	reDecision    = regexp.MustCompile(`^###\s+(?:(\d+)[.)]\s*)?(.+?)\s*$`)
	reCommitLine  = regexp.MustCompile(`(?i)^[-*]\s*commit:\s*` + "`?" + `([0-9a-f]{4,40})` + "`?" + `\s*(?:[·—-]\s*(.+))?$`)
	reRejected    = regexp.MustCompile(`(?i)^[-*]\s*rejected:\s*(.+)$`)
	reBullet      = regexp.MustCompile(`^\s*[-*]\s+(.+)$`)
)

// ParseHistory reads the history skill's markdown into a History.
func ParseHistory(md string) History {
	h := History{Decisions: []Decision{}, StillOpen: []string{}}
	if m := reCommitsRead.FindStringSubmatch(md); m != nil {
		h.CommitsRead, _ = strconv.Atoi(m[1])
		md = strings.Replace(md, m[0], "", 1)
	}
	var (
		section string
		what    []string
		rest    []string
		cur     *Decision
		body    []string
	)
	closeDecision := func() {
		if cur != nil {
			cur.Body = strings.TrimSpace(strings.Join(body, "\n"))
			h.Decisions = append(h.Decisions, *cur)
		}
		cur, body = nil, nil
	}
	for _, line := range strings.Split(md, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "## ") || trim == "##" {
			closeDecision()
			switch strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trim, "##"))) {
			case "what it does":
				section = "what"
			case "decisions":
				section = "decisions"
			case "still open":
				section = "open"
			default:
				section = "rest"
				rest = append(rest, line)
			}
			continue
		}
		switch section {
		case "what":
			what = append(what, line)
		case "decisions":
			if m := reDecision.FindStringSubmatch(trim); m != nil {
				closeDecision()
				n, _ := strconv.Atoi(m[1])
				if n == 0 {
					n = len(h.Decisions) + 1
				}
				cur = &Decision{N: n, Title: m[2], Rejected: []string{}}
				continue
			}
			if cur == nil {
				continue
			}
			if m := reCommitLine.FindStringSubmatch(trim); m != nil && cur.Sha == "" {
				cur.Sha, cur.Source = m[1], strings.TrimSpace(m[2])
				continue
			}
			if m := reRejected.FindStringSubmatch(trim); m != nil {
				cur.Rejected = append(cur.Rejected, strings.TrimSpace(m[1]))
				continue
			}
			body = append(body, line)
		case "open":
			if m := reBullet.FindStringSubmatch(line); m != nil {
				h.StillOpen = append(h.StillOpen, strings.TrimSpace(m[1]))
			} else if trim != "" {
				h.StillOpen = append(h.StillOpen, trim)
			}
		default:
			rest = append(rest, line)
		}
	}
	closeDecision()
	h.WhatItDoes = strings.TrimSpace(strings.Join(what, "\n"))
	h.Rest = strings.TrimSpace(strings.Join(rest, "\n"))
	return h
}

// historyDir is where the generated histories live, one file per PR and head.
func historyDir(dir string) string { return filepath.Join(dir, "history") }

// historyName is the file of one PR's history at one head sha.
func historyName(repo string, number int, sha string) string {
	if len(sha) > 12 {
		sha = sha[:12]
	}
	if sha == "" {
		sha = "nohead"
	}
	return fmt.Sprintf("%s-%d-%s.md", sanitize(repo), number, sanitize(sha))
}

// SaveHistory keeps the history generated for a PR at head sha, replacing an
// older one for the same head: it is the same story, regenerated.
func SaveHistory(dir, repo string, number int, sha, md string) (string, error) {
	if err := os.MkdirAll(historyDir(dir), 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(historyDir(dir), historyName(repo, number, sha))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(md), 0o644); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// LoadHistory returns the history of a PR: the one generated at head sha when
// there is one, else the newest at any head, with stale=true. found=false
// means none was ever generated.
func LoadHistory(dir, repo string, number int, sha string) (md string, stale, found bool) {
	if data, err := os.ReadFile(filepath.Join(historyDir(dir), historyName(repo, number, sha))); err == nil {
		return string(data), false, true
	}
	prefix := fmt.Sprintf("%s-%d-", sanitize(repo), number)
	items, err := os.ReadDir(historyDir(dir))
	if err != nil {
		return "", false, false
	}
	type cand struct {
		name string
		mod  int64
	}
	var cs []cand
	for _, it := range items {
		if it.IsDir() || !strings.HasPrefix(it.Name(), prefix) || !strings.HasSuffix(it.Name(), ".md") {
			continue
		}
		if info, err := it.Info(); err == nil {
			cs = append(cs, cand{it.Name(), info.ModTime().UnixNano()})
		}
	}
	if len(cs) == 0 {
		return "", false, false
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].mod > cs[j].mod })
	data, err := os.ReadFile(filepath.Join(historyDir(dir), cs[0].name))
	if err != nil {
		return "", false, false
	}
	return string(data), true, true
}
