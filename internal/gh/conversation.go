package gh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
)

// ConversationBatch caps the PRs one gh call reads the conversation of: each
// brings up to a hundred threads with their replies, and GitHub weighs the
// whole query. The server cuts its reads by it too.
const ConversationBatch = 10

// Limits of one read. A PR past any of them comes back with Complete false.
const (
	convThreads  = 100
	convReplies  = 50
	convComments = 100
	convReviews  = 50
)

var conversationFragment = fmt.Sprintf(`fragment bazelConversationPR on PullRequest {
  author { login __typename }
  reviewThreads(first: %d) { pageInfo { hasNextPage } nodes {
    isResolved isOutdated path line originalLine startLine diffSide
    resolvedBy { login __typename }
    comments(first: %d) { totalCount nodes { author { login __typename } authorAssociation body url createdAt } }
    lastComment: comments(last: 1) { nodes { author { login __typename } authorAssociation body url createdAt } } } }
  comments(first: %d) { pageInfo { hasNextPage } nodes { author { login __typename } authorAssociation body url createdAt } }
  reviews(first: %d) { pageInfo { hasNextPage } nodes { author { login __typename } authorAssociation state body url submittedAt } }
}`, convThreads, convReplies, convComments, convReviews)

// threadTagsFragment is the least that tells whose threads are open: who
// opened each thread, its last word and how many replies, no bodies but the
// last. GitHub charges a query by the nodes it may bring, and this one is
// read for every PR of the list.
const threadTagsFragment = `fragment bazelThreadTagsPR on PullRequest {
  author { login __typename }
  reviewThreads(first: 100) { pageInfo { hasNextPage } nodes {
    isResolved isOutdated path line originalLine startLine
    comments(first: 1) { totalCount nodes { author { login __typename } url createdAt } }
    lastComment: comments(last: 1) { nodes { author { login __typename } body createdAt } } } }
}`

// rateLimitField is asked with every read: what it cost, and what is left.
const rateLimitField = "  rateLimit { cost remaining resetAt }\n"

// prsQuery asks for n PRs, aliased p0…p<n-1>, each by its own variables, with
// fragment, as reviewersQuery does.
func prsQuery(name, fragment, fragmentName string, n int) string {
	var vars, body strings.Builder
	for i := range n {
		if i > 0 {
			vars.WriteString(", ")
		}
		fmt.Fprintf(&vars, "$o%d: String!, $n%d: String!, $p%d: Int!", i, i, i)
		fmt.Fprintf(&body, "  p%d: repository(owner: $o%d, name: $n%d) { pullRequest(number: $p%d) { ...%s } }\n", i, i, i, i, fragmentName)
	}
	return "query " + name + "(" + vars.String() + ") {\n" + body.String() + rateLimitField + "}\n" + fragment
}

func conversationQuery(n int) string {
	return prsQuery("bazelConversation", conversationFragment, "bazelConversationPR", n)
}

func threadTagsQuery(n int) string {
	return prsQuery("bazelThreadTags", threadTagsFragment, "bazelThreadTagsPR", n)
}

// RateLimit is what GitHub says of its GraphQL budget after a read: the points
// it cost, those left, and when the hour starts again.
type RateLimit struct {
	Cost      int       `json:"cost"`
	Remaining int       `json:"remaining"`
	ResetAt   time.Time `json:"resetAt"`
}

// Who is someone who wrote on a PR. Type is "Bot" for an app; Association is
// how they relate to the repository (OWNER, MEMBER, COLLABORATOR, NONE…), on
// what they wrote.
type Who struct {
	Login       string `json:"login"`
	Type        string `json:"type,omitempty"`
	Association string `json:"association,omitempty"`
}

// IsBot says the account is an app's.
func (w Who) IsBot() bool { return w.Type == "Bot" || isBot(w.Login) }

// Said is one thing written on a PR: a reply in a thread, a comment, a review.
type Said struct {
	Author Who       `json:"author"`
	Body   string    `json:"body"`
	URL    string    `json:"url,omitempty"`
	At     time.Time `json:"at"`
	State  string    `json:"state,omitempty"` // a review's
	// Cut says Body was cut short; Bazel writes it, gh does not.
	Cut bool `json:"cut,omitempty"`
}

// Thread is a review thread, its replies in order.
type Thread struct {
	URL        string `json:"url,omitempty"`
	Path       string `json:"path,omitempty"`
	Line       int    `json:"line,omitempty"`
	StartLine  int    `json:"start_line,omitempty"`
	Side       string `json:"side,omitempty"` // LEFT or RIGHT
	IsResolved bool   `json:"isResolved"`
	IsOutdated bool   `json:"isOutdated"`
	ResolvedBy *Who   `json:"resolvedBy"`
	Comments   []Said `json:"comments"`
	// Total is how many comments the thread has, and Last the newest of them,
	// read on its own: past the first ones read, Comments is not all of it.
	Total int   `json:"-"`
	Last  *Said `json:"-"`
}

// Conversation is what a PR has discussed: its review threads, its comments
// and its reviews' bodies. Complete is false when a list was longer than one
// read brings.
type Conversation struct {
	Author   Who      `json:"author"`
	Complete bool     `json:"complete"`
	Threads  []Thread `json:"threads"`
	Comments []Said   `json:"comments"`
	Reviews  []Said   `json:"reviews"`
}

type rawActor struct {
	Login    string `json:"login"`
	Typename string `json:"__typename"`
}

func (a *rawActor) who() Who {
	if a == nil {
		return Who{}
	}
	w := Who{Login: a.Login}
	if a.Typename == "Bot" {
		w.Type = "Bot"
	}
	return w
}

type rawSaid struct {
	Author            *rawActor `json:"author"`
	AuthorAssociation string    `json:"authorAssociation"`
	Body              string    `json:"body"`
	URL               string    `json:"url"`
	CreatedAt         time.Time `json:"createdAt"`
	SubmittedAt       time.Time `json:"submittedAt"`
	State             string    `json:"state"`
}

func (r rawSaid) said() Said {
	at := r.CreatedAt
	if at.IsZero() {
		at = r.SubmittedAt
	}
	w := r.Author.who()
	w.Association = r.AuthorAssociation
	return Said{Author: w, Body: r.Body, URL: r.URL, At: at, State: r.State}
}

type rawConversation struct {
	Author        *rawActor `json:"author"`
	ReviewThreads struct {
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
		Nodes []struct {
			IsResolved   bool      `json:"isResolved"`
			IsOutdated   bool      `json:"isOutdated"`
			Path         string    `json:"path"`
			Line         *int      `json:"line"`
			OriginalLine *int      `json:"originalLine"`
			StartLine    *int      `json:"startLine"`
			DiffSide     string    `json:"diffSide"`
			ResolvedBy   *rawActor `json:"resolvedBy"`
			Comments     struct {
				TotalCount int       `json:"totalCount"`
				Nodes      []rawSaid `json:"nodes"`
			} `json:"comments"`
			LastComment struct {
				Nodes []rawSaid `json:"nodes"`
			} `json:"lastComment"`
		} `json:"nodes"`
	} `json:"reviewThreads"`
	Comments struct {
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
		Nodes []rawSaid `json:"nodes"`
	} `json:"comments"`
	Reviews struct {
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
		Nodes []rawSaid `json:"nodes"`
	} `json:"reviews"`
}

func (r *rawConversation) conversation() Conversation {
	c := Conversation{
		Author:   r.Author.who(),
		Complete: !r.ReviewThreads.PageInfo.HasNextPage && !r.Comments.PageInfo.HasNextPage && !r.Reviews.PageInfo.HasNextPage,
		Threads:  []Thread{},
		Comments: []Said{},
		Reviews:  []Said{},
	}
	for _, n := range r.ReviewThreads.Nodes {
		t := Thread{Path: n.Path, Side: n.DiffSide, IsResolved: n.IsResolved, IsOutdated: n.IsOutdated, Comments: []Said{}}
		// An outdated thread has no line in the current diff: it keeps the one
		// it was written on.
		switch {
		case n.Line != nil:
			t.Line = *n.Line
		case n.OriginalLine != nil:
			t.Line = *n.OriginalLine
		}
		if n.StartLine != nil {
			t.StartLine = *n.StartLine
		}
		if n.ResolvedBy != nil {
			w := n.ResolvedBy.who()
			t.ResolvedBy = &w
		}
		for _, s := range n.Comments.Nodes {
			t.Comments = append(t.Comments, s.said())
		}
		if len(t.Comments) > 0 {
			t.URL = t.Comments[0].URL
		}
		t.Total = max(n.Comments.TotalCount, len(t.Comments))
		if len(n.LastComment.Nodes) > 0 {
			last := n.LastComment.Nodes[0].said()
			t.Last = &last
		} else if len(t.Comments) > 0 {
			t.Last = &t.Comments[len(t.Comments)-1]
		}
		if n.Comments.TotalCount > len(n.Comments.Nodes) {
			c.Complete = false
		}
		c.Threads = append(c.Threads, t)
	}
	for _, s := range r.Comments.Nodes {
		c.Comments = append(c.Comments, s.said())
	}
	for _, s := range r.Reviews.Nodes {
		c.Reviews = append(c.Reviews, s.said())
	}
	return c
}

// FetchConversations reads what each PR (keys "owner/repo#n") has discussed, in
// as few gh calls as it takes. Only reads. A PR GitHub does not answer for is
// left out of the map.
func FetchConversations(ctx context.Context, keys []string) (map[string]Conversation, error) {
	out := map[string]Conversation{}
	var batch []string
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		got, err := fetchConversations(ctx, batch)
		batch = batch[:0]
		maps.Copy(out, got)
		return err
	}
	for _, key := range keys {
		if _, _, ok := splitKey(key); !ok {
			continue
		}
		if batch = append(batch, key); len(batch) == ConversationBatch {
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

func fetchConversations(ctx context.Context, keys []string) (map[string]Conversation, error) {
	got, _, err := readPRs(ctx, conversationQuery(len(keys)), keys)
	return got, err
}

// FetchThreadTags reads, for up to ConversationBatch PRs in one gh call, only
// what the open-thread tags need, and what GitHub says of its rate limit after
// it. Only reads.
func FetchThreadTags(ctx context.Context, keys []string) (map[string]Conversation, RateLimit, error) {
	var ok []string
	for _, key := range keys {
		if _, _, valid := splitKey(key); valid {
			ok = append(ok, key)
		}
	}
	if len(ok) == 0 {
		return map[string]Conversation{}, RateLimit{}, nil
	}
	return readPRs(ctx, threadTagsQuery(len(ok)), ok)
}

func readPRs(ctx context.Context, query string, keys []string) (map[string]Conversation, RateLimit, error) {
	args := []string{"api", "graphql", "-f", "query=" + query}
	for i, key := range keys {
		repo, number, _ := splitKey(key)
		owner, name, _ := strings.Cut(repo, "/")
		args = append(args, "-f", fmt.Sprintf("o%d=%s", i, owner), "-f", fmt.Sprintf("n%d=%s", i, name), "-F", fmt.Sprintf("p%d=%d", i, number))
	}
	raw, err := run(ctx, "gh", args...)
	if err != nil {
		return nil, RateLimit{}, err
	}
	return parseConversations(raw, keys)
}

func parseConversations(raw string, keys []string) (map[string]Conversation, RateLimit, error) {
	var answer struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(raw), &answer); err != nil {
		return nil, RateLimit{}, fmt.Errorf("gh api graphql: unexpected output: %w", err)
	}
	if answer.Data == nil {
		if len(answer.Errors) > 0 {
			return nil, RateLimit{}, errors.New(answer.Errors[0].Message)
		}
		return nil, RateLimit{}, errors.New("gh api graphql: no data")
	}
	var rl RateLimit
	if r, ok := answer.Data["rateLimit"]; ok {
		_ = json.Unmarshal(r, &rl)
	}
	out := map[string]Conversation{}
	for i, key := range keys {
		var repo *struct {
			PullRequest *rawConversation `json:"pullRequest"`
		}
		if r, ok := answer.Data["p"+strconv.Itoa(i)]; ok && json.Unmarshal(r, &repo) == nil && repo != nil && repo.PullRequest != nil {
			out[key] = repo.PullRequest.conversation()
		}
	}
	return out, rl, nil
}
