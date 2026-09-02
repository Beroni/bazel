// Package gh conversa com o GitHub através do CLI `gh` já autenticado.
package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PR é um pull request achatado para uso na TUI.
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
	// HeadRefOid é o commit do topo da branch do PR. É por ele que o Bazel
	// sabe se o PR mudou depois do review.
	HeadRefOid  string `json:"headRefOid"`
	BaseRefName string `json:"baseRefName"`
	Author      struct {
		Login string `json:"login"`
	} `json:"author"`
	ReviewDecision string `json:"reviewDecision"`
}

// Key identifica um PR de forma única entre repositórios.
func (p PR) Key() string { return fmt.Sprintf("%s#%d", p.Repo, p.Number) }

// Slug é o nome curto do repositório, sem o owner.
func (p PR) Slug() string {
	if i := strings.LastIndex(p.Repo, "/"); i >= 0 {
		return p.Repo[i+1:]
	}
	return p.Repo
}

// Age devolve a idade do PR em formato curto ("3h", "2d").
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

// CheckAuth confirma que o `gh` existe e está logado.
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

// CurrentUser devolve o login do usuário autenticado no `gh`.
func CurrentUser(ctx context.Context) (string, error) {
	out, err := run(ctx, "gh", "api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ListOptions filtra a busca de PRs.
type ListOptions struct {
	Authors       []string
	IncludeDrafts bool
	Limit         int
}

// RepoError registra a falha de um repositório específico sem derrubar o resto.
type RepoError struct {
	Repo string
	Err  error
}

// ListPRs busca os PRs abertos de todos os repos em paralelo.
// Repositórios que falham viram RepoError em vez de abortar a listagem.
func ListPRs(ctx context.Context, repos []string, opts ListOptions) ([]PR, []RepoError) {
	if opts.Limit <= 0 {
		opts.Limit = 100
	}

	authors := lowerSet(opts.Authors)

	// Se há múltiplos repos, tenta uma busca única com `gh search prs`.
	// Se falhar, cai de volta para o fan-out por repo (preserva compatibilidade).
	if len(repos) > 1 {
		prs, err := searchRepoPRs(ctx, repos, opts)
		if err == nil {
			all := make([]PR, 0, len(prs))
			for _, pr := range prs {
				if pr.IsDraft && !opts.IncludeDrafts {
					continue
				}
				if len(authors) > 0 && !authors[strings.ToLower(pr.Author.Login)] {
					continue
				}
				all = append(all, pr)
			}
			sort.Slice(all, func(i, j int) bool {
				return all[i].UpdatedAt.After(all[j].UpdatedAt)
			})
			return all, nil
		}
	}

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

			prs, err := listRepoPRs(ctx, repo, opts.Limit)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, RepoError{Repo: repo, Err: err})
				return
			}
			for _, pr := range prs {
				if pr.IsDraft && !opts.IncludeDrafts {
					continue
				}
				if len(authors) > 0 && !authors[strings.ToLower(pr.Author.Login)] {
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

func searchRepoPRs(ctx context.Context, repos []string, opts ListOptions) ([]PR, error) {
	args := []string{"search", "prs", "--state", "open", "--json", prFields}
	if opts.Limit > 0 {
		args = append(args, "--limit", strconv.Itoa(opts.Limit))
	}
	query := ""
	for _, repo := range repos {
		if query != "" {
			query += " "
		}
		query += "repo:" + repo
	}
	args = append(args, "--")
	if query != "" {
		args = append(args, query)
	}
	out, err := run(ctx, "gh", args...)
	if err != nil {
		return nil, err
	}
	var prs []PR
	if err := json.Unmarshal([]byte(out), &prs); err != nil {
		return nil, fmt.Errorf("unexpected response from gh search prs: %w", err)
	}
	for i := range prs {
		if prs[i].Repo == "" {
			prs[i].Repo = deriveRepoFromURL(prs[i].URL)
		}
	}
	return prs, nil
}

func listRepoPRs(ctx context.Context, repo string, limit int) ([]PR, error) {
	out, err := run(ctx, "gh", "pr", "list",
		"--repo", repo,
		"--state", "open",
		"--limit", strconv.Itoa(limit),
		"--json", prFields,
	)
	if err != nil {
		return nil, err
	}
	var prs []PR
	if err := json.Unmarshal([]byte(out), &prs); err != nil {
		return nil, fmt.Errorf("resposta inesperada do gh: %w", err)
	}
	for i := range prs {
		prs[i].Repo = repo
	}
	return prs, nil
}

// Diff devolve o diff unificado do PR, truncado em maxBytes.
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

// Comment publica um comentário no PR.
func Comment(ctx context.Context, repo string, number int, body string) error {
	cmd := exec.CommandContext(ctx, "gh", "pr", "comment", strconv.Itoa(number),
		"--repo", repo, "--body-file", "-")
	cmd.Stdin = strings.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh pr comment: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// ParseRef entende "owner/repo#12", "owner/repo 12" ou a URL do PR.
func ParseRef(ref string) (repo string, number int, err error) {
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

// Get busca um PR específico.
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

func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
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

func deriveRepoFromURL(urlStr string) string {
	if urlStr == "" {
		return ""
	}
	urlStr = strings.TrimPrefix(urlStr, "https://github.com/")
	urlStr = strings.TrimPrefix(urlStr, "http://github.com/")
	parts := strings.Split(urlStr, "/")
	if len(parts) >= 2 {
		return parts[0] + "/" + parts[1]
	}
	return ""
}
