#!/usr/bin/env bash
# Takes a review Bazel saved to the PR. The agent never assembles a command or a
# payload out of the report: everything comes from files, through jq, and all the
# agent passes back are numeric ids. See ../SKILL.md.
#
#   publish.sh prepare <review-file>
#       Checks the file and surveys the PR; prints the plan, with its work folder.
#   publish.sh publish <work-folder> [--same <finding>=<comment>]...
#       Writes the review. --same says a finding is the comment's root cause.
#
# Exit 0: done, or nothing to do. 3: a gate refused, nothing was written. 1: failed.
set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
PLUGIN=$(cd "$HERE/../../.." && pwd)

# What happened goes in a file Bazel reads, as well as to the agent: Bazel reports
# this, not what a model says about it. "published" is never overwritten: a second
# run that finds the review already there must not turn it into a failure.
outcome() {
  [ "$(head -1 "$PLUGIN/publish-outcome" 2>/dev/null)" = published ] && return 0
  printf '%s\n%s\n' "$1" "$2" > "$PLUGIN/publish-outcome"
}

refuse() {
  printf 'REFUSED, nothing was published: %s\n' "$1"
  outcome refused "Not published: $1"
  exit 3
}
fail() {
  printf 'FAILED: %s\n' "$1"
  outcome failed "Publishing failed: $1"
  exit 1
}
nothing() {
  printf 'Nothing published: %s\n' "$1"
  outcome nothing "Nothing published: $1"
}

# A report in no format this skill can trust still has a way to the PR, as text.
PASTE='To post it as it is, use "or paste as a comment".'

for tool in jq gh git; do
  command -v "$tool" >/dev/null || refuse "$tool is not installed, and publishing needs it. Install it and try again. $PASTE"
done

# The clone's own repository, from its remote; the PR's text has no say in this.
clone_repo() {
  git remote get-url origin 2>/dev/null | jq -Rr 'sub("\\.git$"; "") | capture("(?<r>[^/:]+/[^/:]+)$").r // ""'
}

# gh's paginated lists come as one array per page: flatten, and take only arrays.
flatten() { jq -s '[.[] | if type == "array" then .[] else empty end]'; }

prepare() {
  local file=${1:-}
  [ -f "$file" ] || refuse "no review file at the path given"

  # One folder per run, inside the plugin folder Bazel gives each job and removes
  # with it: two publishes never share a file.
  local work
  work=$(mktemp -d "$PLUGIN/publish.XXXXXX") || fail "cannot create a work folder"

  jq -Rs -f "$HERE/parse-report.jq" "$file" > "$work/report.json" || fail "cannot read the review file"
  # A reader that printed nothing proved nothing: that is a refusal too.
  jq -e '.deny | type == "array"' "$work/report.json" >/dev/null 2>&1 ||
    refuse "this is not a report Bazel can check: it could not be read. $PASTE"
  local deny
  deny=$(jq -r '.deny | unique | join("; ")' "$work/report.json")
  [ -z "$deny" ] || refuse "this is not a report Bazel can check: $deny. $PASTE"

  local repo number
  repo=$(jq -r .repo "$work/report.json")
  number=$(jq -r .number "$work/report.json")
  case "$number" in ''|*[!0-9]*) refuse "the PR number is not a number" ;; esac
  local here
  here=$(clone_repo)
  [ "$(printf %s "$here" | tr '[:upper:]' '[:lower:]')" = "$(printf %s "$repo" | tr '[:upper:]' '[:lower:]')" ] ||
    refuse "the review is for another repository than this clone"

  gh pr view "$number" --repo "$repo" --json author,url > "$work/pr.json" || fail "cannot read the PR"
  gh api user --jq .login > "$work/me.txt" || fail "cannot tell who is signed in"
  jq -Rs 'split("\n")[0]' "$work/me.txt" > "$work/me.json"
  gh pr diff "$number" --repo "$repo" > "$work/diff.txt" || fail "cannot read the PR's diff"
  jq -Rs -f "$HERE/diff-lines.jq" "$work/diff.txt" > "$work/diff.json" || fail "cannot read the diff's lines"

  gh api "repos/$repo/pulls/$number/comments" --paginate | flatten > "$work/comments.json" || fail "cannot read the inline comments"
  gh api "repos/$repo/issues/$number/comments" --paginate | flatten > "$work/issue.json" || fail "cannot read the PR's comments"
  gh api "repos/$repo/pulls/$number/reviews" --paginate | flatten > "$work/reviews.json" || fail "cannot read the PR's reviews"
  gh api graphql --paginate -f owner="${repo%%/*}" -f repo="${repo##*/}" -F number="$number" -f query='
    query($owner:String!, $repo:String!, $number:Int!, $endCursor:String) {
      repository(owner:$owner, name:$repo) { pullRequest(number:$number) {
        reviewThreads(first:100, after:$endCursor) {
          pageInfo { hasNextPage endCursor }
          nodes { isResolved resolvedBy { login } comments(first:1) { nodes { databaseId } } }
    } } } }' |
    jq -s '[.[] | .data.repository.pullRequest.reviewThreads.nodes[]?
      | {root: .comments.nodes[0].databaseId, isResolved, resolvedBy: .resolvedBy.login}]' > "$work/threads.json" ||
    fail "cannot read the review threads"

  jq -n -f "$HERE/plan.jq" --slurpfile report "$work/report.json" --slurpfile diff "$work/diff.json" \
    --slurpfile comments "$work/comments.json" --slurpfile threads "$work/threads.json" \
    --slurpfile issue "$work/issue.json" --slurpfile reviews "$work/reviews.json" \
    --slurpfile pr "$work/pr.json" --slurpfile me "$work/me.json" > "$work/plan.json" || fail "cannot build the plan"

  jq --arg work "$work" '{work_folder: $work, verdict, sha_proven, failed,
    findings: [.findings[] | {id, title, severity, class, path, line, commentable,
      candidates: [.candidates[] | {comment_id, user, settled, excerpt}]}]}' "$work/plan.json"
}

post_review() { # payload-file output-file
  gh api -X POST "repos/$REPO/pulls/$NUMBER/reviews" --input - < "$1" > "$2" 2>&1
}

publish() {
  local work=${1:-}
  [ -f "$work/plan.json" ] || refuse "no plan in the folder given: run prepare first"
  shift

  local same='{}' pair
  for pair in "$@"; do
    [ "$pair" = --same ] && continue
    case "$pair" in
      *[!0-9=]* | =* | *= | *=*=*) refuse "--same takes <finding>=<comment>, as numbers" ;;
    esac
    same=$(jq -c --arg k "${pair%%=*}" --argjson v "${pair##*=}" '.[$k] = $v' <<<"$same")
    jq -e --arg k "${pair%%=*}" --argjson v "${pair##*=}" \
      'any(.findings[]; (.id | tostring) == $k and any(.candidates[]; .comment_id == $v))' "$work/plan.json" >/dev/null ||
      refuse "--same $pair is not one of the plan's candidates"
  done

  REPO=$(jq -r .repo "$work/plan.json")
  NUMBER=$(jq -r .number "$work/plan.json")
  local reviewed head moved=no
  reviewed=$(jq -r .reviewed_sha "$work/plan.json")

  # The last read before writing: a push after the review moves the lines.
  head=$(gh pr view "$NUMBER" --repo "$REPO" --json headRefOid | jq -r .headRefOid) || fail "cannot read the PR's head"
  [ "$head" = "$reviewed" ] || moved=yes

  jq --argjson same "$same" --arg moved "$moved" --arg head "$head" -f "$HERE/decide.jq" "$work/plan.json" > "$work/decided.json" ||
    fail "cannot decide what to publish"

  local kind
  kind=$(jq -r .kind "$work/decided.json")
  case "$kind" in
    nothing)
      nothing "$(jq -r .why "$work/decided.json")."
      return 0 ;;
    all-clear)
      if jq -e --arg sha "$reviewed" 'any(.own_bodies[]; contains("all-clear @ " + $sha))' "$work/plan.json" >/dev/null; then
        nothing "this account already posted an all-clear for this commit."
        return 0
      fi ;;
  esac

  # The same review on the same commit is not published twice.
  local print marker
  print=$(jq -c '[.body, .comments]' "$work/decided.json" | git hash-object --stdin) || fail "cannot fingerprint the review"
  marker="<!-- bazel-review: $reviewed $print -->"
  if jq -e --arg m "$marker" 'any(.own_bodies[]; contains($m))' "$work/plan.json" >/dev/null; then
    nothing "this account already posted this same review for this commit."
    return 0
  fi

  jq --arg marker "$marker" --arg sha "$reviewed" --arg head "$head" '
    {event: "COMMENT", body: (.body + "\n\n" + $marker), comments: .comments}
    + (if ($sha | test("^[0-9a-f]{40}$")) then {commit_id: $sha} else {commit_id: $head} end)' \
    "$work/decided.json" > "$work/payload.json" || fail "cannot build the payload"

  local rejected=0
  if ! post_review "$work/payload.json" "$work/response.txt"; then
    # GitHub refuses the whole review over one line. Every finding still reaches
    # the PR, in the body, and the summary says so.
    if grep -q 'HTTP 422' "$work/response.txt" && [ "$(jq '.comments | length' "$work/payload.json")" -gt 0 ]; then
      rejected=$(jq '.comments | length' "$work/payload.json")
      jq -f "$HERE/refused.jq" "$work/payload.json" > "$work/fallback.json"
      post_review "$work/fallback.json" "$work/response.txt" ||
        fail "GitHub refused the review, with and without its inline comments: $(head -c 300 "$work/response.txt")"
    else
      fail "GitHub refused the review: $(head -c 300 "$work/response.txt")"
    fi
  fi

  outcome published "Published."
  local failed_reactions=0 id
  for id in $(jq -r '.reactions[]' "$work/decided.json"); do
    gh api -X POST "repos/$REPO/pulls/comments/$id/reactions" -f content=+1 >/dev/null 2>&1 ||
      failed_reactions=$((failed_reactions + 1))
  done

  jq -r --argjson rejected "$rejected" --argjson rf "$failed_reactions" --arg moved "$moved" '
    "Published: \(.counts.inline - $rejected) inline, \(.counts.body + $rejected) in the review body, \(.counts.duplicates) already on the PR (👍), \(.counts.settled) already settled.",
    (if $rejected > 0 then "GitHub refused the inline anchors, so \($rejected) finding(s) went to the body instead." else empty end),
    (if $rf > 0 then "\($rf) 👍 could not be added." else empty end),
    (if $moved == "yes" then "The PR moved after the review: nothing was anchored." else empty end),
    (.settled[] | "Already settled on the PR (by @\(.by)): \(.path):\(.line) (\(.level))"),
    (.bodied[] | "In the body: \(.path // "no file"):\(.line // "-") — \(.title)")' "$work/decided.json"
}

case "${1:-}" in
  prepare) shift; prepare "$@" ;;
  publish) shift; publish "$@" ;;
  *) echo "usage: publish.sh prepare <review-file> | publish <work-folder> [--same <finding>=<comment>]..." >&2; exit 2 ;;
esac
