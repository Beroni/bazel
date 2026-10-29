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

# 3. thread state — resolution exists ONLY in GraphQL, never in the REST payload above
gh api graphql -F owner=<owner> -F repo=<repo> -F number=<N> -f query='
query($owner:String!, $repo:String!, $number:Int!) {
  repository(owner:$owner, name:$repo) {
    pullRequest(number:$number) {
      reviewThreads(first:100) {
        nodes { isResolved isOutdated comments(first:1) { nodes { databaseId } } }
      } } } }' \
  --jq '.data.repository.pullRequest.reviewThreads.nodes[]
        | {root: .comments.nodes[0].databaseId, isResolved, isOutdated}'
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

## A finding already on the PR — react, do not repeat

Running the fleet twice on the same PR must not double the number of comments, and a finding
somebody already raised does not need the fleet repeating it in different words.

The definition of "same finding", and the four destinations a finding can land in, are in
`bazel-post-report/SKILL.md` — one place, so they cannot drift. What follows here are the commands.

```bash
# duplicate of an inline review comment
gh api -X POST repos/:owner/:repo/pulls/comments/<id>/reactions -f content=+1

# duplicate of a comment on the PR body
gh api -X POST repos/:owner/:repo/issues/comments/<id>/reactions -f content=+1
```

These are **different endpoints** for the two comment types, and an `id` from one does not
exist in the other — sending it to the wrong one returns 404. You already know the type: it
came from one of the two lists above.

Reacting is idempotent. If the 👍 is already there, the API returns 200 instead of 201 and does
not duplicate — running the fleet three times on the same PR leaves one 👍, not three.

The 👍 says "I found this too, independently". It is a convergence signal, the same kind as two
lenses agreeing, and it matters most when the comment belongs to someone else. When the
duplicate is from a previous run of the fleet itself, the 👍 communicates nothing to anyone —
but it is harmless and idempotent, so react anyway and do not spend a decision on it.

Filtering duplicates is **not** filtering severity: a `critical` already on the PR still counts
toward the verdict and the report. It just does not become a second comment.

If **every** finding is a duplicate, the payload ends up with no comments at all — do not send
an empty review, and **do not post the all-clear**: the all-clear is for zero findings, and
here there are findings, just already said. Let the 👍 speak and report it in stdout.

## A finding that was already settled — stand down

A duplicate whose thread was **resolved**, or which somebody **replied to**, is not just
already said: it was already decided. It gets no comment and **no 👍** — a 👍 on a thread
someone closed with "intentional" reads as the fleet insisting, which is the opposite of what
happened.

```bash
# is this existing comment settled? (from the survey above, no extra call)
#   resolved  → its root id appears in call 3 with isResolved: true
#   discussed → some comment in call 1 has in_reply_to_id == its root id
```

Any reply is a human reply: **this skill never replies to a thread** (the 👍 is the only write
it makes to somebody else's comment), so a reply can only have come from a person.

`isOutdated` is not settlement — it only means the lines moved. Judge by `isResolved` and by
replies, and leave staleness to the sha gate.

Nothing is written to GitHub for a settled finding. It is reported in stdout and listed in the
review body, so the decision is visible without being relitigated.

## A single review with inline comments (preferred)

One review groups everything into a single notification instead of N. Assemble the payload and
send it in one go:

```bash
cat > /tmp/review.json <<'JSON'
{
  "body": "## Review Fleet\n\n3 lenses · 4 findings · verdict: request_changes\n\nScope: `git diff origin/develop...HEAD` — 12 files · head `a1b2c3d`\n\n<summary>",
  "event": "COMMENT",
  "comments": [
    {
      "path": "src/api/transfer.ts",
      "line": 88,
      "side": "RIGHT",
      "body": "**critical · authz** (senior-code-reviewer + exploit-digger)\n\nTransfer reads `accountId` from the request body without checking ownership.\n\n**Trigger:** `POST /api/transfer` with another user's `accountId` debits their account.\n\n**Fix:** resolve the account from the session, or assert `account.ownerId === session.userId` before debiting."
    }
  ]
}
JSON

gh api -X POST repos/:owner/:repo/pulls/<N>/reviews --input /tmp/review.json
```

`path`, `line` and `side` come straight from the finding's `<!-- finding ... -->`. The `body`
is the finding's prose **verbatim** — nothing is assembled here, because the prose already
opens with its level line (`**critical · authz** (…)`), written that way in the report exactly
so this step is a copy. See `references/report-format.md`.

The fields that matter:

- `event`: **`COMMENT`**, always. Do not use `REQUEST_CHANGES` — that blocks the merge in the
  user's name, a decision that is theirs, not the fleet's. The verdict goes in the `body` and
  they act on it. `APPROVE` never, not even when the report says `approve`.
- `line` + `side: "RIGHT"` — the line in the file **after** the change. To comment on a removed
  line, `side: "LEFT"`.
- Line range: `start_line` (and `start_side`) alongside `line`.

The finding's `ref` (CWE/OWASP) lives only in the metadata; append it as a trailing line on the
comment when there is one. Its `confidence` stays out of the PR.

If the API rejects one comment for an invalid line, it rejects the **entire review**. Drop the
offending comment, send the rest, and tell the user which one fell out — do not give up on
posting everything because of one, and do not swallow the loss.

## Fallback: a single comment on the PR

When anchoring to a line is not worth it (scattered findings, lines outside the diff) or is not
safe (the report's `head_sha` does not match the PR's HEAD):

```bash
gh pr comment <N> --body-file /tmp/report.md
```

This is the safest path and the one to prefer when in doubt. With a different sha, the body has
to say which commit was reviewed:

```markdown
> Reviewed at `a1b2c3d`; the PR has moved since, so findings are not line-anchored.
```

## Zero findings — post the all-clear

A silent fleet is indistinguishable from a fleet that never ran. With no findings at all, post
saying so — with what was covered attached:

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

The coverage line is the report's header — `scope_cmd`, `files` and `lenses_ran`, verbatim.
Without it the all-clear is worthless: "no problems found" on its own does not say whether the
diff was swept or ignored. Never post an all-clear without saying what was swept and by whom.

Two conditions, both mandatory and both checkable in the report header:

- **`files:` greater than zero.** An all-clear on top of a scope that never resolved is the
  worst possible comment — it tells the team the code was reviewed when nothing was read, and a
  collapsed `git diff A...B` exits 0 with zero lines, exactly like a clean diff.
- **`lenses_ran` complete**, or the comment says which were skipped and why (`lenses_skipped`
  carries the reason). "0 findings" with a lens missing claims coverage that never happened.

`gh pr comment` still applies, never `APPROVE` — the fleet says it found nothing; approving is
the user's decision.

The trailing marker carries the PR's HEAD (`gh pr view <N> --json headRefOid --jq .headRefOid`).
On the next run, look for it among the body comments — and for the marker earlier Bazel
versions posted, `<!-- bazel: all-clear @ <sha> -->`, which means the same thing:

- **Same sha** already has an all-clear → nothing changed since; **👍 it**, do not post another.
- **Different sha** → a new commit landed, and the old all-clear does not cover that code. Post
  a new one. Do not delete or edit the old one: it was true about that sha.

## The shape of each comment

**Everything posted goes out in English.** The report is already born in English
(`report-format.md`), so normally it is a straight copy. If any prose came back from a lens in
another language, translate it before assembling the payload — what reaches the PR is read by
the team and stays in the repo's history.

The comment is the finding's prose as written in the report: the level line, the description,
the `**Trigger:**` when there is one, the `**Fix:**`.

```
**critical · authz** (senior-code-reviewer + exploit-digger)
```

Two lenses on the same finding is a strong signal, and that line is where it shows. For
`lazy-senior-dev` cuts the same line carries the gain:

```
**minor · reuse · −27 lines** (lazy-senior-dev)
```

No hedging, no paragraph defending the finding. If the explanation is longer than the code, cut
the explanation.
