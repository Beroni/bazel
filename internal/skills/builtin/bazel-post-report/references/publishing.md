# Publishing commands

The `gh` commands for the `bazel-post-report` skill, the only built-in skill that writes to
GitHub. `bazel-review-fleet` and the three lenses produce a review; Bazel hands it to
`bazel-post-report` when a person clicks publish.

The flow and the gates live in `bazel-post-report/SKILL.md`. What is here are the commands and
the exact shape of everything that goes to the PR.

## Survey what is already on the PR

Before assembling any payload — this is what decides between posting, reacting and standing
down. **Three calls, all three required**, because no single one of them sees everything.

```bash
# 1. inline comments, anchored to a line — this is where the fleet posts
gh api repos/:owner/:repo/pulls/<N>/comments --paginate \
  --jq '.[] | {id, in_reply_to_id, path, line: (.line // .original_line), user: .user.login, body}'

# 2. comments on the PR body — where the fallback and the all-clear land
gh api repos/:owner/:repo/issues/<N>/comments --paginate \
  --jq '.[] | {id, user: .user.login, body}'

# 3. thread state — resolution exists ONLY in GraphQL, never in the REST payload above.
#    --paginate walks every page through $endCursor: a PR can have more than 100 threads.
gh api graphql --paginate -F owner=<owner> -F repo=<repo> -F number=<N> -f query='
query($owner:String!, $repo:String!, $number:Int!, $endCursor:String) {
  repository(owner:$owner, name:$repo) {
    pullRequest(number:$number) {
      reviewThreads(first:100, after:$endCursor) {
        pageInfo { hasNextPage endCursor }
        nodes { isResolved isOutdated resolvedBy { login } comments(first:1) { nodes { databaseId } } }
      } } } }' \
  --jq '.data.repository.pullRequest.reviewThreads.nodes[]
        | {root: .comments.nodes[0].databaseId, isResolved, isOutdated, resolvedBy: .resolvedBy.login}'
```

The `:owner` / `:repo` placeholders that work in the REST paths above **do not work in
GraphQL** — there they are read literally and the call fails with
`Could not resolve to a Repository with the name ':owner/:repo'`. Pass them as typed variables
with `-F`, as shown (`-F` sends `number` as an integer; `-f` would send it as a string and
`Int!` would reject it).

Three things in that payload decide everything downstream, and two of them are easy to miss:

- **`.line` comes back `null`** on a comment that went stale (the line left the diff), which is
  why the `// .original_line` is there. Without that fallback you lose exactly the comments
  from previous runs — the ones most likely to be duplicates.
- **`in_reply_to_id` is what makes a reply a reply.** Replies come back in the *same flat list*
  as top-level comments, carrying the same `path` and the same `line`. Without selecting this
  field, `"That's a good idea, let's do it after this ships"` at `command/pr.go:297` is
  indistinguishable from an original finding at `command/pr.go:297`. The field is **absent**
  on a top-level comment and carries the thread root's `id` on a reply — so
  `in_reply_to_id == null` means "this is a thread root".
- **Resolution is GraphQL-only.** `/pulls/<N>/comments` returns nothing about it: a comment
  sitting in a thread somebody resolved looks exactly like one nobody touched. Join call 3 to
  call 1 on `databaseId == id` — they are the same number.

## A 👍 on a duplicate

When a finding is already on the PR, and the rules in `bazel-post-report/SKILL.md` say to react:

```bash
# duplicate of an inline review comment
gh api -X POST repos/:owner/:repo/pulls/comments/<id>/reactions -f content=+1

# duplicate of a comment on the PR body
gh api -X POST repos/:owner/:repo/issues/comments/<id>/reactions -f content=+1
```

Two **different endpoints**: an `id` from one list returns 404 on the other, and you know the
type from the list it came from. Reacting is idempotent (200 instead of 201 when the 👍 is
already there).

## A settled finding — who settled it

From the survey above, with no extra call:

```bash
#   resolved  → its root id appears in call 3 with isResolved: true; who → resolvedBy
#   discussed → some comment in call 1 has in_reply_to_id == its root id; who → that comment's user
#   by the PR's author → compare user with `gh pr view <N> --json author --jq .author.login`
```

## A single review with inline comments

```bash
cat > /tmp/review.json <<'JSON'
{
  "body": "## Review Fleet\n\nverdict: request_changes · 4 findings\n\nScope: `git diff origin/develop...HEAD` — 12 files · head `a1b2c3d`\n\n<summary>",
  "event": "COMMENT",
  "comments": [
    {
      "path": "src/api/transfer.ts",
      "line": 88,
      "side": "RIGHT",
      "body": "**critical · authz** (senior-code-reviewer + exploit-digger)\n\nTransfer reads `accountId` from the request body without checking ownership.\n\n**Trigger:** `POST /api/transfer` with another user's `accountId` debits their account.\n\n**Fix:** resolve the account from the session, or assert `account.ownerId === session.userId` before debiting.\n\nCWE-639,OWASP-A01"
    }
  ]
}
JSON

gh api -X POST repos/:owner/:repo/pulls/<N>/reviews --input /tmp/review.json
```

- `path`, `line`, `side` come from the finding's `<!-- finding … -->`; `side: "LEFT"` for a
  removed line; a range adds `start_line` (and `start_side`).
- `body` is the finding's prose verbatim — it already opens with its level line (see
  `.claude/skills/bazel-review-fleet/references/report-format.md`) — plus `ref` as a trailing
  line when there is one.
- `event` is `COMMENT`, always.
- One comment on an invalid line makes the API reject the **whole** review: drop that comment,
  send the rest, and say which one fell out.

## Fallback: a single comment on the PR

```bash
gh pr comment <N> --body-file /tmp/report.md
```

With a sha mismatch the body opens with:

```markdown
> Reviewed at `a1b2c3d`; the PR has moved since, so findings are not line-anchored.
```

## Zero findings — the all-clear

```bash
gh pr comment <N> --body-file /tmp/all-clear.md
```

```markdown
## Review Fleet — no findings

3 lenses · 0 findings · verdict: approve
Scope: `git diff origin/develop...HEAD` — 12 files · head `a1b2c3d`
Lenses: senior-code-reviewer, exploit-digger, lazy-senior-dev

<!-- review-fleet: all-clear @ a1b2c3d -->
```

- The coverage lines are the report's header — `scope_cmd`, `files`, `lenses_ran` (and
  `lenses_skipped` with its reasons when it is not `none`), verbatim.
- The marker carries the PR's HEAD (`gh pr view <N> --json headRefOid --jq .headRefOid`).
  Before posting, look for it among the body comments, and for the marker earlier Bazel
  versions posted, `<!-- bazel: all-clear @ <sha> -->`: same sha → 👍 it and post nothing;
  different sha → post a new one, and never edit or delete the old.
