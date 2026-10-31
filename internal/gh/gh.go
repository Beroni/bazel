// Package gh talks to GitHub through the already-authenticated `gh` CLI.
package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PR is a flattened pull request for use in the TUI.
type PR struct {
	Repo         string    `json:"-"`
	Number       int       `json:"number"`
	Title        string    `json:"title"`
	URL          string    `json:"url"`
	Body         string    `json:"body"`
	IsDraft      bool      `json:"isDraft"`
	Additions    int       `json:"additions"`
	Deletions    int       `json:"deletions"`
	ChangedFiles int       `json:"changedFiles"`
	UpdatedAt    time.Time `json:"updatedAt"`
	HeadRefName  string    `json:"headRefName"`
	// HeadRefOid is the commit at the tip of the PR's branch. It is how Bazel
	// knows whether the PR changed after the review.
	HeadRefOid  string `json:"headRefOid"`
	BaseRefName string `json:"baseRefName"`
	Author      struct {
		Login string `json:"login"`
	} `json:"author"`
	ReviewDecision string `json:"reviewDecision"`
}

// Key identifies a PR uniquely across repositories.
func (p PR) Key() string { return fmt.Sprintf("%s#%d", p.Repo, p.Number) }

// Slug is the short name of the repository, without the owner.
func (p PR) Slug() string {
	if i := strings.LastIndex(p.Repo, "/"); i >= 0 {
		return p.Repo[i+1:]
	}
	return p.Repo
}

// Age returns the PR's age in short form ("3h", "2d").
func (p PR) Age(now time.Time) string {
	d := now.Sub(p.UpdatedAt)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

const prFields = "number,title,url,body,isDraft,additions,deletions,changedFiles,updatedAt,headRefName,headRefOid,baseRefName,author,reviewDecision"

// CheckAuth confirms that `gh` exists and is logged in.
func CheckAuth(ctx context.Context) error {
	if _, err := exec.LookPath("gh"); err != nil {
		return fmt.Errorf("`gh` not found in PATH — install the GitHub CLI (brew install gh)")
	}
	cmd := exec.CommandContext(ctx, "gh", "auth", "status")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("`gh` is not authenticated — run `gh auth login`")
	}
	return nil
}

// CurrentUser returns the login of the user authenticated in `gh`.
func CurrentUser(ctx context.Context) (string, error) {
	out, err := run(ctx, "gh", "api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ListOptions filters the PR search.
type ListOptions struct {
	Authors       []string
	IncludeDrafts bool
	Limit         int
}

// RepoError records the failure of a specific repository without bringing down the rest.
type RepoError struct {
	Repo string
	Err  error
	// Truncated says the listing worked and stopped at the limit: what came back
	// is good, only not all of it.
	Truncated bool
}

// ListPRs fetches the open PRs of all repos in parallel, one `gh pr list` each:
// `gh search prs` does not know half of these fields. Repositories that fail
// become RepoError instead of aborting the listing, and so does one with more
// open PRs than the limit, marked Truncated.
func ListPRs(ctx context.Context, repos []string, opts ListOptions) ([]PR, []RepoError) {
	if opts.Limit <= 0 {
		opts.Limit = 100
	}
	authors := lowerSet(opts.Authors)
	var (
		mu   sync.Mutex
		all  []PR
		errs []RepoError
		wg   sync.WaitGroup
		sem  = make(chan struct{}, 6)
	)
	for _, repo := range repos {
		wg.Add(1)
		go func(repo string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			prs, full, err := listRepoPRs(ctx, repo, opts.Limit, slices.Sorted(maps.Keys(authors)))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, RepoError{Repo: repo, Err: err})
				return
			}
			if full {
				errs = append(errs, RepoError{Repo: repo, Truncated: true,
					Err: fmt.Errorf("more than %d open PRs: showing the newest %d", opts.Limit, opts.Limit)})
			}
			for _, pr := range prs {
				if pr.IsDraft && !opts.IncludeDrafts {
					continue
				}
				all = append(all, pr)
			}
		}(repo)
	}
	wg.Wait()

	sort.Slice(all, func(i, j int) bool {
		return all[i].UpdatedAt.After(all[j].UpdatedAt)
	})
	sort.Slice(errs, func(i, j int) bool { return errs[i].Repo < errs[j].Repo })
	return all, errs
}

// ListCalls is how many gh processes ListPRs runs: one per repository and, with
// a filter, per author in each. What is charged has to be what runs.
func ListCalls(repos []string, opts ListOptions) int {
	return len(repos) * max(1, len(lowerSet(opts.Authors)))
}

// listRepoPRs lists a repository's open PRs, one call per author when there is
// a filter: the limit has to apply to that author's PRs, or the filter would run
// over the newest PRs of everyone and could find none. full says a call came
// back at the limit, so there may be more.
func listRepoPRs(ctx context.Context, repo string, limit int, authors []string) (list []PR, full bool, err error) {
	if len(authors) == 0 {
		authors = []string{""}
	}
	seen := map[int]bool{}
	for _, author := range authors {
		args := []string{"pr", "list", "--repo", repo, "--state", "open", "--limit", strconv.Itoa(limit), "--json", prFields}
		if author != "" {
			args = append(args, "--author", author)
		}
		out, err := run(ctx, "gh", args...)
		if err != nil {
			return nil, false, err
		}
		var prs []PR
		if err := json.Unmarshal([]byte(out), &prs); err != nil {
			return nil, false, fmt.Errorf("resposta inesperada do gh: %w", err)
		}
		full = full || len(prs) >= limit
		for _, pr := range prs {
			// The server side filters by login; this keeps a gh that does not.
			if author != "" && !strings.EqualFold(pr.Author.Login, author) || seen[pr.Number] {
				continue
			}
			seen[pr.Number] = true
			pr.Repo = repo
			list = append(list, pr)
		}
	}
	return list, full, nil
}

// Diff returns the PR's unified diff, truncated at maxBytes.
func Diff(ctx context.Context, repo string, number int, maxBytes int) (string, bool, error) {
	out, err := run(ctx, "gh", "pr", "diff", strconv.Itoa(number), "--repo", repo)
	if err != nil {
		return "", false, err
	}
	if maxBytes > 0 && len(out) > maxBytes {
		return out[:maxBytes], true, nil
	}
	return out, false, nil
}

// Comment posts a comment on the PR.
func Comment(ctx context.Context, repo string, number int, body string) error {
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "comment", strconv.Itoa(number),
		"--repo", repo, "--body-file", "-")
	cmd.WaitDelay = ghWaitDelay
	cmd.Stdin = strings.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh pr comment: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// ParseRef understands "owner/repo#12", "owner/repo 12" or the PR URL.
func ParseRef(ref string) (repo string, number int, err error) {
	repo, number, err = parseRef(ref)
	if err != nil {
		return "", 0, err
	}
	// The repository goes to `gh --repo`, which also takes a host: a reference
	// must not be able to point gh at another server.
	if !repoRe.MatchString(repo) || number <= 0 {
		return "", 0, fmt.Errorf("invalid reference: %q (use owner/repo#123 or the PR URL)", ref)
	}
	return repo, number, nil
}

// repoRe is owner/name, in the characters GitHub allows.
var repoRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*/[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

func parseRef(ref string) (repo string, number int, err error) {
	ref = strings.TrimSpace(ref)
	if u := strings.TrimPrefix(ref, "https://github.com/"); u != ref {
		parts := strings.Split(strings.Trim(u, "/"), "/")
		if len(parts) >= 4 && parts[2] == "pull" {
			n, convErr := strconv.Atoi(parts[3])
			if convErr != nil {
				return "", 0, fmt.Errorf("invalid PR number in %q", ref)
			}
			return parts[0] + "/" + parts[1], n, nil
		}
		return "", 0, fmt.Errorf("invalid PR URL: %q", ref)
	}
	if i := strings.Index(ref, "#"); i > 0 {
		n, convErr := strconv.Atoi(strings.TrimSpace(ref[i+1:]))
		if convErr != nil {
			return "", 0, fmt.Errorf("invalid PR number in %q", ref)
		}
		return strings.TrimSpace(ref[:i]), n, nil
	}
	return "", 0, fmt.Errorf("invalid reference: %q (use owner/repo#123 or the PR URL)", ref)
}

// Get fetches a specific PR.
func Get(ctx context.Context, repo string, number int) (PR, error) {
	out, err := run(ctx, "gh", "pr", "view", strconv.Itoa(number), "--repo", repo, "--json", prFields)
	if err != nil {
		return PR{}, err
	}
	var pr PR
	if err := json.Unmarshal([]byte(out), &pr); err != nil {
		return PR{}, fmt.Errorf("resposta inesperada do gh: %w", err)
	}
	pr.Repo = repo
	return pr, nil
}

// ghTimeout limits one gh call. Nothing here is worth waiting for longer, and a
// gh stuck on the network would otherwise hold a worker or the list forever.
var ghTimeout = 2 * time.Minute

// ghWaitDelay is how long a killed gh gets to let go of its pipes.
const ghWaitDelay = 5 * time.Second

func run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = ghWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", msg)
	}
	return stdout.String(), nil
}

func lowerSet(items []string) map[string]bool {
	if len(items) == 0 {
		return nil
	}
	m := make(map[string]bool, len(items))
	for _, it := range items {
		if it = strings.TrimSpace(strings.TrimPrefix(it, "@")); it != "" {
			m[strings.ToLower(it)] = true
		}
	}
	return m
}

// detailFields is what the Overview needs beyond the list: the timeline, the
// files, who reviews, labels and checks. One `gh pr view` brings all of it.
const detailFields = "headRefOid,createdAt,body,commits,files,reviewRequests,latestReviews,labels,statusCheckRollup"

// Details is a PR's page as the Overview shows it.
type Details struct {
	HeadRefOid string    `json:"headRefOid"`
	CreatedAt  time.Time `json:"createdAt"`
	Body       string    `json:"body"`
	Commits    []struct {
		Oid             string    `json:"oid"`
		MessageHeadline string    `json:"messageHeadline"`
		CommittedDate   time.Time `json:"committedDate"`
		Authors         []struct {
			Login string `json:"login"`
			Name  string `json:"name"`
		} `json:"authors"`
	} `json:"commits"`
	Files []struct {
		Path      string `json:"path"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
	} `json:"files"`
	ReviewRequests []struct {
		Login string `json:"login"`
		Name  string `json:"name"`
		Slug  string `json:"slug"`
	} `json:"reviewRequests"`
	LatestReviews []struct {
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		State string `json:"state"`
	} `json:"latestReviews"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	StatusCheckRollup []struct {
		Name       string `json:"name"`
		Context    string `json:"context"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		State      string `json:"state"`
	} `json:"statusCheckRollup"`
}

// GetDetails reads a PR's page (commits, files, reviewers, labels, checks).
func GetDetails(ctx context.Context, repo string, number int) (Details, error) {
	out, err := run(ctx, "gh", "pr", "view", strconv.Itoa(number), "--repo", repo, "--json", detailFields)
	if err != nil {
		return Details{}, err
	}
	var d Details
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		return Details{}, fmt.Errorf("gh pr view: unexpected output: %w", err)
	}
	return d, nil
}
