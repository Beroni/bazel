// Command gh is a fake GitHub CLI for the e2e harness. It answers the calls
// Bazel makes (internal/gh and internal/workspace) from fixtures on disk,
// records every invocation, and never touches the network.
//
// Environment:
//
//	FAKE_GH_FIXTURES  directory with prs.json (and optional gh.json)
//	FAKE_GH_STATE     writable directory: calls.jsonl and the fixture git repos
//
// Anything it does not know exits 1 with "fake gh: unsupported", so a new
// gh call in Bazel shows up as a failing test instead of a silent pass.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// fixturePR is one PR in prs.json: the gh JSON fields plus the repo it belongs
// to and a few harness-only knobs.
type fixturePR struct {
	Repo         string `json:"repo"`
	Number       int    `json:"number"`
	Title        string `json:"title"`
	URL          string `json:"url,omitempty"`
	Body         string `json:"body"`
	IsDraft      bool   `json:"isDraft"`
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
	ChangedFiles int    `json:"changedFiles"`
	UpdatedAt    string `json:"updatedAt,omitempty"`
	UpdatedAgo   string `json:"updatedAgo,omitempty"` // "2h": now minus this
	HeadRefName  string `json:"headRefName"`
	HeadRefOid   string `json:"headRefOid"`
	BaseRefName  string `json:"baseRefName"`
	Author       struct {
		Login string `json:"login"`
	} `json:"author"`
	ReviewDecision string            `json:"reviewDecision"`
	Diff           string            `json:"diff,omitempty"`
	Files          map[string]string `json:"files,omitempty"` // what the PR branch adds
	// The PR page fields (gh pr view --json …). Missing ones are synthesized
	// from the rest: one commit at headRefOid, files from Files, empty lists.
	CreatedAt         string `json:"createdAt,omitempty"`
	Commits           []any  `json:"commits,omitempty"`
	ReviewRequests    []any  `json:"reviewRequests,omitempty"`
	LatestReviews     []any  `json:"latestReviews,omitempty"`
	Labels            []any  `json:"labels,omitempty"`
	StatusCheckRollup []any  `json:"statusCheckRollup,omitempty"`
}

// settings is gh.json: who is logged in and which calls fail on purpose.
type settings struct {
	User string `json:"user"`
	// Fail maps a call key to the stderr it should die with. Keys:
	// "search", "list:<owner/repo>", "view:<owner/repo>#<n>",
	// "comment", "clone:<owner/repo>", "checkout", "api".
	Fail map[string]string `json:"fail"`
}

var (
	fixtures = os.Getenv("FAKE_GH_FIXTURES")
	state    = os.Getenv("FAKE_GH_STATE")
	cfg      settings
	stdin    string
	// unknown marks a call the fake does not implement; the harness fails
	// the test when one shows up in the log.
	unknown bool
)

func main() {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("gh version 0.0.0-fake (e2e)")
		return
	}
	if fixtures == "" || state == "" {
		die(2, "fake gh: FAKE_GH_FIXTURES and FAKE_GH_STATE must be set")
	}
	if b, err := os.ReadFile(filepath.Join(fixtures, "gh.json")); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			die(2, "fake gh: bad gh.json: "+err.Error())
		}
	}
	if cfg.User == "" {
		cfg.User = "e2e-user"
	}
	if wantsStdin(args) {
		b, _ := io.ReadAll(os.Stdin)
		stdin = string(b)
	}
	code := dispatch(args)
	record(args, code)
	os.Exit(code)
}

func wantsStdin(args []string) bool {
	for i, a := range args {
		if (a == "--body-file" || a == "--input") && i+1 < len(args) && args[i+1] == "-" {
			return true
		}
	}
	return false
}

func dispatch(args []string) int {
	switch {
	case len(args) == 0:
		return unsupported(args)
	case is(args, "auth", "status"):
		fmt.Fprintf(os.Stderr, "github.com\n  ✓ Logged in to github.com account %s (fake)\n", cfg.User)
		return 0
	case is(args, "api", "user"):
		fmt.Println(cfg.User)
		return 0
	case is(args, "api"):
		if failed("api") {
			return 1
		}
		fmt.Println(`{"id":1,"html_url":"https://github.invalid/fake"}`)
		return 0
	case is(args, "search", "prs"):
		return searchPRs(args)
	case is(args, "pr", "list"):
		return listPRs(args)
	case is(args, "pr", "view"):
		return viewPR(args)
	case is(args, "pr", "diff"):
		return diffPR(args)
	case is(args, "pr", "comment"):
		if failed("comment") {
			return 1
		}
		repo := flag(args, "--repo")
		fmt.Printf("https://github.invalid/%s/pull/%s#issuecomment-1\n", repo, args[2])
		return 0
	case is(args, "repo", "clone"):
		return cloneRepo(args)
	case is(args, "pr", "checkout"):
		return checkoutPR(args)
	}
	return unsupported(args)
}

func is(args []string, words ...string) bool {
	if len(args) < len(words) {
		return false
	}
	for i, w := range words {
		if args[i] != w {
			return false
		}
	}
	return true
}

func flag(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func failed(key string) bool {
	if msg, ok := cfg.Fail[key]; ok {
		fmt.Fprintln(os.Stderr, msg)
		return true
	}
	return false
}

func unsupported(args []string) int {
	unknown = true
	fmt.Fprintf(os.Stderr, "fake gh: unsupported: gh %s\n", strings.Join(args, " "))
	return 1
}

func die(code int, msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(code)
}

// record appends the call and its exit code to calls.jsonl under an exclusive
// lock: reviews run in parallel and two gh processes may write at once.
func record(args []string, code int) {
	cwd, _ := os.Getwd()
	line, _ := json.Marshal(map[string]any{
		"argv":        args,
		"stdin":       stdin,
		"exit":        code,
		"unsupported": unknown,
		"cwd":         cwd,
		"at":          time.Now().UTC().Format(time.RFC3339Nano),
	})
	_ = os.MkdirAll(state, 0o755)
	f, err := os.OpenFile(filepath.Join(state, "calls.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		die(2, "fake gh: cannot record call: "+err.Error())
	}
	defer f.Close()
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	f.Write(append(line, '\n'))
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// --- PRs ---

func loadPRs() []fixturePR {
	b, err := os.ReadFile(filepath.Join(fixtures, "prs.json"))
	if err != nil {
		die(2, "fake gh: "+err.Error())
	}
	var prs []fixturePR
	if err := json.Unmarshal(b, &prs); err != nil {
		die(2, "fake gh: bad prs.json: "+err.Error())
	}
	for i := range prs {
		p := &prs[i]
		if p.URL == "" {
			p.URL = fmt.Sprintf("https://github.com/%s/pull/%d", p.Repo, p.Number)
		}
		if p.UpdatedAgo != "" {
			d, err := time.ParseDuration(p.UpdatedAgo)
			if err != nil {
				die(2, "fake gh: bad updatedAgo: "+p.UpdatedAgo)
			}
			p.UpdatedAt = time.Now().Add(-d).UTC().Format(time.RFC3339)
		}
		if p.UpdatedAt == "" {
			p.UpdatedAt = "2026-01-01T00:00:00Z"
		}
		if p.BaseRefName == "" {
			p.BaseRefName = "main"
		}
	}
	return prs
}

// out writes the PRs keeping only the fields asked for with --json, like gh.
func out(args []string, prs []fixturePR) int {
	b, _ := json.Marshal(rows(args, prs))
	fmt.Println(string(b))
	return 0
}

func rows(args []string, prs []fixturePR) []map[string]any {
	fields := strings.Split(flag(args, "--json"), ",")
	out := make([]map[string]any, 0, len(prs))
	for _, p := range prs {
		raw, _ := json.Marshal(p)
		var all map[string]any
		_ = json.Unmarshal(raw, &all)
		row := map[string]any{}
		for _, f := range fields {
			if v, ok := synthesized(p, f); ok {
				row[f] = v
			} else if v, ok := all[f]; ok {
				row[f] = v
			}
		}
		out = append(out, row)
	}
	return out
}

// synthesized answers the PR page fields a fixture left out, in gh's shape.
// `files` is always gh's list: the fixture's files map is the branch content.
func synthesized(p fixturePR, field string) (any, bool) {
	switch field {
	case "files":
		out := []map[string]any{}
		for _, path := range sortedKeys(p.Files) {
			out = append(out, map[string]any{"path": path, "additions": strings.Count(p.Files[path], "\n") + 1, "deletions": 0})
		}
		return out, true
	case "createdAt":
		if p.CreatedAt == "" {
			return p.UpdatedAt, true
		}
	case "commits":
		if p.Commits == nil {
			return []map[string]any{{
				"oid": p.HeadRefOid, "messageHeadline": p.Title, "committedDate": p.UpdatedAt,
				"authors": []map[string]string{{"login": p.Author.Login, "name": p.Author.Login}},
			}}, true
		}
	case "reviewRequests", "latestReviews", "labels", "statusCheckRollup":
		raw, _ := json.Marshal(p)
		var all map[string]any
		_ = json.Unmarshal(raw, &all)
		if _, ok := all[field]; !ok {
			return []any{}, true
		}
	}
	return nil, false
}

func searchPRs(args []string) int {
	if failed("search") {
		return 1
	}
	repos := map[string]bool{}
	for _, tok := range strings.Fields(args[len(args)-1]) {
		if r, ok := strings.CutPrefix(tok, "repo:"); ok {
			repos[strings.ToLower(r)] = true
		}
	}
	var hit []fixturePR
	for _, p := range loadPRs() {
		if repos[strings.ToLower(p.Repo)] {
			hit = append(hit, p)
		}
	}
	return out(args, limit(args, hit))
}

func listPRs(args []string) int {
	repo := flag(args, "--repo")
	if failed("list:" + repo) {
		return 1
	}
	var hit []fixturePR
	for _, p := range loadPRs() {
		if strings.EqualFold(p.Repo, repo) {
			hit = append(hit, p)
		}
	}
	return out(args, limit(args, hit))
}

func limit(args []string, prs []fixturePR) []fixturePR {
	sort.SliceStable(prs, func(i, j int) bool { return prs[i].UpdatedAt > prs[j].UpdatedAt })
	if n, err := strconv.Atoi(flag(args, "--limit")); err == nil && n > 0 && len(prs) > n {
		return prs[:n]
	}
	return prs
}

func find(repo string, number int) (fixturePR, bool) {
	for _, p := range loadPRs() {
		if strings.EqualFold(p.Repo, repo) && p.Number == number {
			return p, true
		}
	}
	return fixturePR{}, false
}

func viewPR(args []string) int {
	repo := flag(args, "--repo")
	n, _ := strconv.Atoi(args[2])
	if failed(fmt.Sprintf("view:%s#%d", repo, n)) {
		return 1
	}
	p, ok := find(repo, n)
	if !ok {
		fmt.Fprintf(os.Stderr, "GraphQL: Could not resolve to a PullRequest with the number of %d. (repository.pullRequest)\n", n)
		return 1
	}
	// gh pr view prints an object, not a list.
	b, _ := json.Marshal(rows(args, []fixturePR{p})[0])
	fmt.Println(string(b))
	return 0
}

func diffPR(args []string) int {
	n, _ := strconv.Atoi(args[2])
	p, ok := find(flag(args, "--repo"), n)
	if !ok {
		fmt.Fprintln(os.Stderr, "fake gh: no such PR")
		return 1
	}
	if p.Diff != "" {
		fmt.Print(p.Diff)
		return 0
	}
	for _, path := range sortedKeys(p.Files) {
		fmt.Printf("diff --git a/%s b/%s\n--- /dev/null\n+++ b/%s\n@@ -0,0 +1 @@\n+%s\n", path, path, path, p.Files[path])
	}
	return 0
}

// --- clone ---

var originRe = regexp.MustCompile(`repos/([^/]+)/([^/]+)\.git/?$`)

// origin builds (once, atomically) a bare repo for the fixture repository:
// main with a README, plus one branch per PR adding its files.
func origin(repo string) (string, error) {
	final := filepath.Join(state, "repos", repo+".git")
	if _, err := os.Stat(final); err == nil {
		return final, nil
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(filepath.Dir(final), ".build-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	src := filepath.Join(work, "src")
	steps := [][]string{{"init", "-q", "-b", "main", src}}
	if err := gitAll("", steps); err != nil {
		return "", err
	}
	write(filepath.Join(src, "README.md"), "# "+repo+"\n")
	if err := gitAll(src, [][]string{{"add", "-A"}, {"commit", "-q", "-m", "init"}}); err != nil {
		return "", err
	}
	for _, p := range loadPRs() {
		if !strings.EqualFold(p.Repo, repo) || p.HeadRefName == "" {
			continue
		}
		if err := gitAll(src, [][]string{{"checkout", "-q", "-B", p.HeadRefName, "main"}}); err != nil {
			return "", err
		}
		files := p.Files
		if len(files) == 0 {
			files = map[string]string{fmt.Sprintf("pr-%d.txt", p.Number): p.Title}
		}
		for _, path := range sortedKeys(files) {
			write(filepath.Join(src, path), files[path]+"\n")
		}
		if err := gitAll(src, [][]string{{"add", "-A"}, {"commit", "-q", "-m", p.Title}}); err != nil {
			return "", err
		}
	}
	bare := filepath.Join(work, "bare.git")
	if err := gitAll("", [][]string{{"clone", "-q", "--bare", src, bare}}); err != nil {
		return "", err
	}
	if err := os.Rename(bare, final); err != nil {
		if _, statErr := os.Stat(final); statErr == nil {
			return final, nil // another clone won the race
		}
		return "", err
	}
	return final, nil
}

func cloneRepo(args []string) int {
	if len(args) < 4 {
		return unsupported(args)
	}
	repo, dir := args[2], args[3]
	if failed("clone:" + repo) {
		return 1
	}
	src, err := origin(repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake gh: building origin:", err)
		return 1
	}
	gitArgs := []string{"clone", "-q"}
	for i, a := range args {
		if a == "--" {
			gitArgs = append(gitArgs, args[i+1:]...)
			break
		}
	}
	gitArgs = append(gitArgs, "file://"+src, dir)
	if err := gitAll("", [][]string{gitArgs}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func checkoutPR(args []string) int {
	if failed("checkout") {
		return 1
	}
	n, _ := strconv.Atoi(args[2])
	url, err := exec.Command("git", "config", "remote.origin.url").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake gh: not inside a clone")
		return 1
	}
	m := originRe.FindStringSubmatch(strings.TrimSpace(string(url)))
	if m == nil {
		fmt.Fprintln(os.Stderr, "fake gh: origin is not a fixture repo:", string(url))
		return 1
	}
	p, ok := find(m[1]+"/"+m[2], n)
	if !ok {
		fmt.Fprintf(os.Stderr, "fake gh: no PR #%d in %s/%s\n", n, m[1], m[2])
		return 1
	}
	if err := gitAll("", [][]string{{"checkout", "-q", "-B", p.HeadRefName, "origin/" + p.HeadRefName}}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func gitAll(dir string, steps [][]string) error {
	for _, s := range steps {
		cmd := exec.Command("git", s...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=e2e", "GIT_AUTHOR_EMAIL=e2e@example.invalid",
			"GIT_COMMITTER_NAME=e2e", "GIT_COMMITTER_EMAIL=e2e@example.invalid")
		if b, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %v: %s", strings.Join(s, " "), err, strings.TrimSpace(string(b)))
		}
	}
	return nil
}

func write(path, content string) {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		die(2, "fake gh: "+err.Error())
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
