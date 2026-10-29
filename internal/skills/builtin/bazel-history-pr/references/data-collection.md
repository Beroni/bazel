# Data collection

Every read this skill makes, and the rules that keep the result complete. All of it is
read-only; nothing here writes to GitHub.

Set these once and reuse them — hardcoding the repo in one command and taking it from the
cwd in the next is how a report ends up mixing two PRs:

```bash
R=owner/repo     # from the URL/argument, or `gh repo view --json nameWithOwner --jq .nameWithOwner`
N=123
OUT="${TMPDIR:-/tmp}/bazel-history-pr-$N"   # mkdir -p it; outside the clone
```

`gh` reads any repo you have access to with `--repo`/`repos/$R/…`. **Nothing here needs the clone**
beyond knowing which repo it is.

## The seven reads

They are independent — issue them in one message so they run concurrently.

### 1. The PR itself

```bash
gh pr view "$N" --repo "$R" --json \
  number,title,body,state,isDraft,author,createdAt,updatedAt,mergedAt,closedAt,\
baseRefName,headRefName,headRefOid,url,additions,deletions,changedFiles,labels,\
milestone,reviewDecision,mergedBy,closingIssuesReferences \
  > "$OUT/pr.json"
```

`closingIssuesReferences` is the linked issue — the "about" layer usually lives there rather
than in the body. `reviewDecision` is the current verdict, not the history of verdicts.

### 2. Commits

```bash
gh api "repos/$R/pulls/$N/commits" --paginate > "$OUT/commits.json"
```

Per commit you want: `sha`, `commit.message`, `commit.author.date`, `commit.committer.date`,
`author.login`, `parents` (two parents = merge commit from the base branch, not an attempt).

```bash
jq -r '.[] | [.commit.committer.date, .commit.author.date, (.sha[0:7]),
              (.author.login // .commit.author.name),
              (.commit.message | split("\n")[0])] | @tsv' "$OUT/commits.json"
```

Only the head branch's current commits are here. See the force-push section below.

### 3. Review verdicts

```bash
gh api "repos/$R/pulls/$N/reviews" --paginate > "$OUT/reviews.json"
```

`state` is `APPROVED` / `CHANGES_REQUESTED` / `COMMENTED` / `DISMISSED`, `submitted_at` is
the timeline key, and `id` is what ties inline comments to their review
(`pull_request_review_id`). A `PENDING` review is someone's unsubmitted draft — invisible to
you, and a normal reason for a thread to have no verdict.

### 4. Inline review comments

```bash
gh api "repos/$R/pulls/$N/comments" --paginate > "$OUT/review_comments.json"
```

The review→fix loop lives here. Fields that matter:

| Field | Use |
|---|---|
| `path`, `line`, `start_line`, `side` | where the thread is anchored |
| `in_reply_to_id` | replies; the comment without it starts the thread |
| `original_commit_id` | the commit the comment was written against |
| `commit_id` | where the anchor currently sits |
| `pull_request_review_id` | which review batch it belongs to |
| `created_at`, `user.login`, `body` | the timeline row |

Group into threads by `in_reply_to_id` (root = a comment with none; a reply chain's members
all point at the root).

### 5. Conversation comments

```bash
gh api "repos/$R/issues/$N/comments" --paginate > "$OUT/issue_comments.json"
```

A different endpoint from #4 and a different comment system — see trap 1 in the SKILL. Both
are needed: high-level pushback ("this whole approach breaks X") is usually here, while the
line-level asks are in #4.

### 6. Timeline events

```bash
gh api "repos/$R/issues/$N/timeline" --paginate > "$OUT/timeline.json"
```

The only source for events that are not commits or comments. The ones worth rows:

| `event` | Means |
|---|---|
| `head_ref_force_pushed` | history rewritten — **always a row** |
| `ready_for_review`, `convert_to_draft` | the author's own confidence signal |
| `review_requested`, `review_request_removed` | who was pulled in, and when |
| `renamed` | the PR's scope changed mid-flight |
| `cross-referenced`, `connected` | related PRs/issues |
| `labeled`, `unlabeled` | often the process state (`needs-qa`, `blocked`) |
| `merged`, `closed`, `reopened` | the end, and the false ends |

### 7. Thread state (GraphQL only)

REST does not expose resolved/outdated. This does:

```bash
gh api graphql --paginate -F owner="${R%%/*}" -F repo="${R##*/}" -F pr="$N" -f query='
query($owner:String!,$repo:String!,$pr:Int!,$endCursor:String){
  repository(owner:$owner,name:$repo){
    pullRequest(number:$pr){
      reviewThreads(first:50, after:$endCursor){
        pageInfo{ hasNextPage endCursor }
        nodes{
          isResolved isOutdated isCollapsed path line startLine
          resolvedBy{ login }
          comments(first:50){ nodes{
            databaseId author{ login } createdAt outdated
            originalCommit{ oid } body
          }}
        }
      }
    }
  }
}' > "$OUT/threads.json"
```

`--paginate` needs both the `$endCursor` variable and the `pageInfo` block in the query; drop
either and you get page one with a clean exit.

## What changed, for the "about" layer

```bash
gh api "repos/$R/pulls/$N/files" --paginate \
  --jq '.[] | [.status, .additions, .deletions, .filename] | @tsv' > "$OUT/files.tsv"
```

Read the *shape* of it — which directories, which layer of the app, test files present or
not — not the diff line by line. The stated intent in the PR body and the actual footprint of
the change are two different claims, and the report carries both.

## Verifying a review→fix link

The question is always "did this commit touch what that thread was about?":

```bash
gh api "repos/$R/commits/$SHA" --jq '.files[] | [.filename, .patch != null] | @tsv'
```

Same path → **inferred**. Message names the thread/reviewer, or the patch hunks cover the
thread's `line`/`start_line` → **confirmed**. Nothing after the thread touches that path →
**unanswered**. The levels are defined in the SKILL; this is how you evidence them.

## Force-push: what survives and what does not

After `head_ref_force_pushed`, the pre-push commits are usually unreachable:

```bash
gh api "repos/$R/commits/$ORIGINAL_COMMIT_ID" >/dev/null 2>&1 || echo "gone: $ORIGINAL_COMMIT_ID"
```

A 422/404 there means the commit a thread was written against no longer exists. Handle it as
information, not as an error: put the force-push in the timeline, attach the orphaned threads
to it, and say plainly that what the author tried before that point cannot be recovered from
the API. Do not drop the thread and do not pretend the comment was about current code.

## Counting checks

```bash
gh pr checks "$N" --repo "$R" 2>/dev/null || true
gh api "repos/$R/commits/$(jq -r .headRefOid "$OUT/pr.json")/check-runs" \
  --jq '.check_runs[] | [.name, .conclusion, .completed_at] | @tsv'
```

Only the current head's checks are cheap to get. Historical red→green cycles per commit are
one API call per commit — do that only if the user asks for the CI story specifically.

## Rules

- **`--paginate` on every list call.** 30 per page is the default, and truncation exits 0.
- **Cross-check counts** against `gh pr view --json commits,reviews,comments` before writing
  numbers into the report.
- **Cache to files.** You will re-read these fields several times while building the axis.
- **A 403 is not an empty PR.** Private repo, missing scope (`gh auth status`), or an org
  policy. Say which read failed instead of reporting the PR as quiet.
- **Bots** (`type: "Bot"`, or logins ending in `[bot]`) get collapsed, never dropped: one row
  per batch, so the human story stays legible.
