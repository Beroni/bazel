package gh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"
)

// ReviewersBatch caps the PRs one gh call asks about: each is an alias in the
// query, and GitHub weighs the whole query.
const ReviewersBatch = 40

// reviewersFragment is what tells who reviews a PR: the requests still
// pending and the latest review of each person. Only reads.
const reviewersFragment = `fragment bazelReviewersPR on PullRequest {
  author { login }
  reviewRequests(first: 50) { nodes { requestedReviewer { __typename
    ... on User { login } ... on Bot { login } ... on Mannequin { login }
    ... on Team { slug organization { login } } } } }
  latestReviews(first: 50) { nodes { state author { __typename login } } }
}`

// repoPartRe is what an owner or a repository name may hold. They go in as
// variables, never into the query text; this only skips what cannot be a PR.
var repoPartRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// reviewersQuery asks for n PRs, aliased p0…p<n-1>, each by its own variables.
func reviewersQuery(n int) string {
	var vars, body strings.Builder
	for i := range n {
		if i > 0 {
			vars.WriteString(", ")
		}
		fmt.Fprintf(&vars, "$o%d: String!, $n%d: String!, $p%d: Int!", i, i, i)
		fmt.Fprintf(&body, "  p%d: repository(owner: $o%d, name: $n%d) { pullRequest(number: $p%d) { ...bazelReviewersPR } }\n", i, i, i, i)
	}
	return "query bazelReviewers(" + vars.String() + ") {\n" + body.String() + "}\n" + reviewersFragment
}

type reviewersPR struct {
	Author         Actor `json:"author"`
	ReviewRequests struct {
		Nodes []struct {
			RequestedReviewer *struct {
				Typename     string `json:"__typename"`
				Login        string `json:"login"`
				Slug         string `json:"slug"`
				Organization *struct {
					Login string `json:"login"`
				} `json:"organization"`
			} `json:"requestedReviewer"`
		} `json:"nodes"`
	} `json:"reviewRequests"`
	LatestReviews struct {
		Nodes []struct {
			State  string `json:"state"`
			Author Actor  `json:"author"`
		} `json:"nodes"`
	} `json:"latestReviews"`
}

// FetchReviewers reads who reviews each PR (keys "owner/repo#n"), in as few gh
// calls as it takes: the people and teams asked and still pending, then those
// who approved, asked for changes or commented, in GitHub's order, each once.
// The author, me and bots stay out, and so does someone whose only review was
// dismissed. A PR GitHub does not answer for comes back with nobody.
func FetchReviewers(ctx context.Context, keys []string, me string) (map[string][]string, error) {
	out := map[string][]string{}
	var batch []string
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		got, err := fetchReviewers(ctx, batch, me)
		batch = batch[:0]
		maps.Copy(out, got)
		return err
	}
	for _, key := range keys {
		if _, _, ok := splitKey(key); !ok {
			continue
		}
		if batch = append(batch, key); len(batch) == ReviewersBatch {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

func fetchReviewers(ctx context.Context, keys []string, me string) (map[string][]string, error) {
	args := []string{"api", "graphql", "-f", "query=" + reviewersQuery(len(keys))}
	for i, key := range keys {
		repo, number, _ := splitKey(key)
		owner, name, _ := strings.Cut(repo, "/")
		args = append(args, "-f", fmt.Sprintf("o%d=%s", i, owner), "-f", fmt.Sprintf("n%d=%s", i, name), "-F", fmt.Sprintf("p%d=%d", i, number))
	}
	raw, err := run(ctx, "gh", args...)
	if err != nil {
		return nil, err
	}
	return parseReviewers(raw, keys, me)
}

func parseReviewers(raw string, keys []string, me string) (map[string][]string, error) {
	var answer struct {
		Data map[string]*struct {
			PullRequest *reviewersPR `json:"pullRequest"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(raw), &answer); err != nil {
		return nil, fmt.Errorf("gh api graphql: unexpected output: %w", err)
	}
	if answer.Data == nil {
		if len(answer.Errors) > 0 {
			return nil, errors.New(answer.Errors[0].Message)
		}
		return nil, errors.New("gh api graphql: no data")
	}
	out := map[string][]string{}
	for i, key := range keys {
		repo := answer.Data["p"+strconv.Itoa(i)]
		if repo == nil || repo.PullRequest == nil {
			out[key] = []string{}
			continue
		}
		out[key] = repo.PullRequest.reviewers(me)
	}
	return out, nil
}

// reviewers lists who reviews the PR, as the summary names them.
func (p *reviewersPR) reviewers(me string) []string {
	names := []string{}
	seen := map[string]bool{}
	add := func(name string) {
		k := strings.ToLower(name)
		if name == "" || seen[k] || strings.EqualFold(name, p.Author.Login) || strings.EqualFold(name, me) {
			return
		}
		seen[k] = true
		names = append(names, name)
	}
	for _, n := range p.ReviewRequests.Nodes {
		r := n.RequestedReviewer
		switch {
		case r == nil:
		case r.Typename == "Team":
			if r.Slug != "" && r.Organization != nil && r.Organization.Login != "" {
				add(r.Organization.Login + "/" + r.Slug)
			}
		case r.Typename == "User" && !isBot(r.Login):
			add(r.Login)
		}
	}
	for _, rv := range p.LatestReviews.Nodes {
		switch rv.State {
		case "APPROVED", "CHANGES_REQUESTED", "COMMENTED":
			if rv.Author.Typename != "Bot" && !isBot(rv.Author.Login) {
				add(rv.Author.Login)
			}
		}
	}
	return names
}

func isBot(login string) bool {
	l := strings.ToLower(login)
	return strings.HasSuffix(l, "[bot]") || strings.HasPrefix(l, "app/")
}

// splitKey reads "owner/repo#n".
func splitKey(key string) (repo string, number int, ok bool) {
	repo, num, found := strings.Cut(key, "#")
	owner, name, slash := strings.Cut(repo, "/")
	n, err := strconv.Atoi(num)
	if !found || !slash || err != nil || n <= 0 || !repoPartRe.MatchString(owner) || !repoPartRe.MatchString(name) {
		return "", 0, false
	}
	return repo, n, true
}
