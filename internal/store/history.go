package store

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	// Meta is the skill's header block (`<!-- history-pr:v1 … -->`): repo,
	// pr, head_sha, commits, threads… It never shows on the page.
	Meta        map[string]string `json:"meta,omitempty"`
	CommitsRead int               `json:"commits_read,omitempty"`
	WhatItDoes  string            `json:"what_it_does"`
	Decisions   []Decision        `json:"decisions"`
	StillOpen   []string          `json:"still_open"`
	// StillOpenNote is the prose above the still-open list, if any.
	StillOpenNote string `json:"still_open_note,omitempty"`
	Rest          string `json:"rest,omitempty"`
}

// Decision is one numbered decision of the PR's story.
type Decision struct {
	N     int    `json:"n"`
	Title string `json:"title"`
	Sha   string `json:"sha,omitempty"`
	// Shas is every commit the Commit line names; Sha is the first.
	Shas     []string `json:"shas,omitempty"`
	Source   string   `json:"source,omitempty"`
	Body     string   `json:"body"`
	Rejected []string `json:"rejected"`
}

var (
	reCommitsRead = regexp.MustCompile(`(?im)^\s*commits read:\s*(\d+)\s*$`)
	reDecision    = regexp.MustCompile(`^###\s+(?:(\d+)[.)]\s*)?(.+?)\s*$`)
	reCommitLine  = regexp.MustCompile(`(?i)^[-*]\s*commit:\s*(` + "`?" + `[0-9a-f]{4,40}` + "`?" + `(?:\s*,\s*` + "`?" + `[0-9a-f]{4,40}` + "`?" + `)*)\s*(?:[·—-]\s*(.+))?$`)
	reShas        = regexp.MustCompile(`(?i)[0-9a-f]{4,40}`)
	reMetaBlock   = regexp.MustCompile(`(?s)^\s*<!--\s*history-pr:v\d+\b(.*?)-->\s*`)
	reRejected    = regexp.MustCompile(`(?i)^[-*]\s*rejected:\s*(.+)$`)
	reBullet      = regexp.MustCompile(`^\s*[-*]\s+(.+)$`)
)

// ParseHistory reads the history skill's markdown into a History.
func ParseHistory(md string) History {
	h := History{Decisions: []Decision{}, StillOpen: []string{}}
	// The skill's header block: read its key: value lines and drop it — raw
	// HTML would show escaped on the page.
	if m := reMetaBlock.FindStringSubmatch(md); m != nil {
		h.Meta = map[string]string{}
		for _, line := range strings.Split(m[1], "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) != "" {
				h.Meta[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
		md = md[len(m[0]):]
	}
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
				// reCommitLine accepted m[1] with the same alphabet, so there
				// is always a sha; an agent's text never gets to index empty.
				shas := reShas.FindAllString(strings.ToLower(m[1]), -1)
				if len(shas) == 0 {
					body = append(body, line)
					continue
				}
				cur.Sha, cur.Source = shas[0], strings.TrimSpace(m[2])
				if len(shas) > 1 {
					cur.Shas = shas
				}
				continue
			}
			if m := reRejected.FindStringSubmatch(trim); m != nil {
				cur.Rejected = append(cur.Rejected, strings.TrimSpace(m[1]))
				continue
			}
			body = append(body, line)
		case "open":
			// One item per top-level bullet; an indented line (a nested
			// bullet, a wrapped line) belongs to the item above it, and prose
			// before the first bullet is the list's note.
			indented := trim != "" && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t"))
			switch m := reBullet.FindStringSubmatch(line); {
			case trim == "":
			case indented && len(h.StillOpen) > 0:
				h.StillOpen[len(h.StillOpen)-1] += "\n" + line
			case m != nil:
				h.StillOpen = append(h.StillOpen, strings.TrimSpace(m[1]))
			case len(h.StillOpen) == 0:
				h.StillOpenNote = strings.TrimSpace(h.StillOpenNote + "\n" + trim)
			default:
				h.StillOpen[len(h.StillOpen)-1] += "\n" + trim
			}
		default:
			if !(section == "" && strings.HasPrefix(trim, "# ")) {
				rest = append(rest, line)
			}
		}
	}
	closeDecision()
	h.WhatItDoes = strings.TrimSpace(strings.Join(what, "\n"))
	h.Rest = strings.TrimSpace(strings.Join(rest, "\n"))
	return h
}

// historyPath is the file of one PR's history at one head:
// history/<owner>/<name>/<number>/<sha>.md. Owner, name and number each get a
// directory of their own, so no two PRs can share a file (acme/web-2 #10 is
// not acme/web #2, a-b/c is not a/b-c), and owner and name go in lower case,
// as GitHub compares them.
func historyPath(dir, repo string, number int, sha string) (string, error) {
	owner, name, ok := strings.Cut(strings.ToLower(repo), "/")
	if !ok || !safeSegment(owner) || !safeSegment(name) || number <= 0 {
		return "", fmt.Errorf("invalid PR %s#%d", repo, number)
	}
	sha = strings.ToLower(sha)
	if len(sha) > 12 {
		sha = sha[:12]
	}
	if sha == "" || strings.Trim(sha, "0123456789abcdef") != "" {
		sha = "nohead"
	}
	return filepath.Join(dir, "history", owner, name, strconv.Itoa(number), sha+".md"), nil
}

// safeSegment is a path segment that stays where it is put.
func safeSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`)
}

// SaveHistory keeps the history generated for a PR at head sha, replacing an
// older one for the same head: it is the same story, regenerated. The file is
// written under a temporary name of its own and renamed into place.
func SaveHistory(dir, repo string, number int, sha, md string) (string, error) {
	path, err := historyPath(dir, repo, number, sha)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".history-*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.WriteString(md); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return path, nil
}

// LoadHistory returns the history of a PR: the one generated at head sha when
// there is one, else the newest at another head of the same PR, with
// stale=true. found=false means none was ever generated.
func LoadHistory(dir, repo string, number int, sha string) (md string, stale, found bool) {
	path, err := historyPath(dir, repo, number, sha)
	if err != nil {
		return "", false, false
	}
	if data, err := os.ReadFile(path); err == nil {
		return string(data), false, true
	}
	items, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return "", false, false
	}
	newest, at := "", int64(0)
	for _, it := range items {
		if it.IsDir() || !strings.HasSuffix(it.Name(), ".md") {
			continue
		}
		if info, err := it.Info(); err == nil && info.ModTime().UnixNano() > at {
			newest, at = it.Name(), info.ModTime().UnixNano()
		}
	}
	if newest == "" {
		return "", false, false
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(path), newest))
	if err != nil {
		return "", false, false
	}
	return string(data), true, true
}
