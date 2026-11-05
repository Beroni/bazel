package gh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// NeedsLimit is how many PRs each of the three searches brings back.
const NeedsLimit = 50

// needsQuery reads, in one call, the open PRs where the user is asked to
// review, has reviewed, or is the author, with what tells whether they need
// the user now: requests, reviews, comments, threads, the last push, checks
// and conflicts. Only reads.
const needsQuery = `query bazelNeedsYou($requested: String!, $reviewed: String!, $authored: String!) {
  requested: search(query: $requested, type: ISSUE, first: 50) { issueCount nodes { ...needsPR } }
  reviewed: search(query: $reviewed, type: ISSUE, first: 50) { issueCount nodes { ...needsPR } }
  authored: search(query: $authored, type: ISSUE, first: 50) { issueCount nodes { ...needsPR } }
}
fragment needsPR on PullRequest {
  number title url isDraft createdAt updatedAt additions deletions changedFiles
  headRefName baseRefName headRefOid reviewDecision mergeable
  author { login }
  repository { nameWithOwner }
  commits(last: 1) { nodes { commit { committedDate statusCheckRollup { state } } } }
  forcePushes: timelineItems(last: 1, itemTypes: [HEAD_REF_FORCE_PUSHED_EVENT]) { nodes { ... on HeadRefForcePushedEvent { createdAt } } }
  requests: timelineItems(last: 10, itemTypes: [REVIEW_REQUESTED_EVENT]) { nodes { ... on ReviewRequestedEvent { createdAt requestedReviewer { __typename ... on User { login } ... on Team { slug } } } } }
  reviews(last: 30) { nodes { id author { login } state submittedAt } }
  comments(last: 20) { nodes { author { __typename login } createdAt } }
  reviewThreads(last: 50) { nodes { isResolved resolvedBy { login }
    firstComment: comments(first: 1) { nodes { author { login } createdAt pullRequestReview { id } } }
    lastComment: comments(last: 1) { nodes { author { __typename login } createdAt } } } }
}`

// Actor is who did something on GitHub.
type Actor struct {
	Login    string `json:"login"`
	Typename string `json:"__typename"`
}

// NeedsPR is one open PR as the needs-you searches bring it.
type NeedsPR struct {
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	URL            string    `json:"url"`
	IsDraft        bool      `json:"isDraft"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
	Additions      int       `json:"additions"`
	Deletions      int       `json:"deletions"`
	ChangedFiles   int       `json:"changedFiles"`
	HeadRefName    string    `json:"headRefName"`
	BaseRefName    string    `json:"baseRefName"`
	HeadRefOid     string    `json:"headRefOid"`
	ReviewDecision string    `json:"reviewDecision"`
	Mergeable      string    `json:"mergeable"`
	Author         Actor     `json:"author"`
	Repository     struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				CommittedDate     time.Time `json:"committedDate"`
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	ForcePushes struct {
		Nodes []struct {
			CreatedAt time.Time `json:"createdAt"`
		} `json:"nodes"`
	} `json:"forcePushes"`
	Requests struct {
		Nodes []struct {
			CreatedAt         time.Time `json:"createdAt"`
			RequestedReviewer *struct {
				Typename string `json:"__typename"`
				Login    string `json:"login"`
				Slug     string `json:"slug"`
			} `json:"requestedReviewer"`
		} `json:"nodes"`
	} `json:"requests"`
	Reviews struct {
		Nodes []struct {
			ID          string    `json:"id"`
			Author      Actor     `json:"author"`
			State       string    `json:"state"`
			SubmittedAt time.Time `json:"submittedAt"`
		} `json:"nodes"`
	} `json:"reviews"`
	Comments struct {
		Nodes []struct {
			Author    Actor     `json:"author"`
			CreatedAt time.Time `json:"createdAt"`
		} `json:"nodes"`
	} `json:"comments"`
	ReviewThreads struct {
		Nodes []struct {
			IsResolved   bool   `json:"isResolved"`
			ResolvedBy   *Actor `json:"resolvedBy"`
			FirstComment struct {
				Nodes []struct {
					Author            Actor     `json:"author"`
					CreatedAt         time.Time `json:"createdAt"`
					PullRequestReview *struct {
						ID string `json:"id"`
					} `json:"pullRequestReview"`
				} `json:"nodes"`
			} `json:"firstComment"`
			LastComment struct {
				Nodes []struct {
					Author    Actor     `json:"author"`
					CreatedAt time.Time `json:"createdAt"`
				} `json:"nodes"`
			} `json:"lastComment"`
		} `json:"nodes"`
	} `json:"reviewThreads"`

	// Requested, Reviewed and Authored say which search found it.
	Requested bool `json:"-"`
	Reviewed  bool `json:"-"`
	Authored  bool `json:"-"`
}

// pushedAt is when the author last moved the branch, as far as the API says:
// the last commit's date or the last force push. A commit made before a review
// and pushed after it is missed, which leaves the PR in "waiting", not wrong.
func (p NeedsPR) PushedAt() time.Time {
	var at time.Time
	for _, c := range p.Commits.Nodes {
		if c.Commit.CommittedDate.After(at) {
			at = c.Commit.CommittedDate
		}
	}
	for _, f := range p.ForcePushes.Nodes {
		if f.CreatedAt.After(at) {
			at = f.CreatedAt
		}
	}
	return at
}

// Checks is the state of the head commit's checks, or "" with none.
func (p NeedsPR) Checks() string {
	for _, c := range p.Commits.Nodes {
		if c.Commit.StatusCheckRollup != nil {
			return c.Commit.StatusCheckRollup.State
		}
	}
	return ""
}

// Repo is the PR's owner/name.
func (p NeedsPR) Repo() string { return p.Repository.NameWithOwner }

// loginRe is what GitHub allows in a login: it goes into a search query, where
// anything else could add qualifiers.
var loginRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// NeedsSearches are the three search queries, for login.
func NeedsSearches(login string) (requested, reviewed, authored string) {
	base := "is:pr is:open archived:false sort:updated-desc "
	return base + "review-requested:" + login,
		base + "reviewed-by:" + login + " -author:" + login,
		base + "author:" + login
}

// FetchNeeds runs the three searches in one gh call. Each PR comes once, with
// the searches that found it marked. truncated says a search had more than
// NeedsLimit results.
func FetchNeeds(ctx context.Context, login string) (prs []NeedsPR, truncated bool, err error) {
	if !loginRe.MatchString(login) {
		return nil, false, fmt.Errorf("invalid GitHub login %q", login)
	}
	requested, reviewed, authored := NeedsSearches(login)
	out, err := run(ctx, "gh", "api", "graphql",
		"-f", "query="+needsQuery,
		"-f", "requested="+requested, "-f", "reviewed="+reviewed, "-f", "authored="+authored)
	if err != nil {
		return nil, false, err
	}
	return parseNeeds(out)
}

type needsSearch struct {
	IssueCount int       `json:"issueCount"`
	Nodes      []NeedsPR `json:"nodes"`
}

func parseNeeds(out string) ([]NeedsPR, bool, error) {
	var answer struct {
		Data *struct {
			Requested needsSearch `json:"requested"`
			Reviewed  needsSearch `json:"reviewed"`
			Authored  needsSearch `json:"authored"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(out), &answer); err != nil {
		return nil, false, fmt.Errorf("gh api graphql: unexpected output: %w", err)
	}
	if answer.Data == nil {
		if len(answer.Errors) > 0 {
			return nil, false, errors.New(answer.Errors[0].Message)
		}
		return nil, false, errors.New("gh api graphql: no data")
	}
	var (
		out2      []NeedsPR
		at        = map[string]int{}
		truncated bool
	)
	add := func(s needsSearch, mark func(*NeedsPR)) {
		truncated = truncated || s.IssueCount > len(s.Nodes)
		for _, p := range s.Nodes {
			if p.Number == 0 || p.Repo() == "" {
				continue
			}
			key := fmt.Sprintf("%s#%d", p.Repo(), p.Number)
			i, ok := at[key]
			if !ok {
				i = len(out2)
				at[key] = i
				out2 = append(out2, p)
			}
			mark(&out2[i])
		}
	}
	add(answer.Data.Requested, func(p *NeedsPR) { p.Requested = true })
	add(answer.Data.Reviewed, func(p *NeedsPR) { p.Reviewed = true })
	add(answer.Data.Authored, func(p *NeedsPR) { p.Authored = true })
	return out2, truncated, nil
}
