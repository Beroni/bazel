#!/usr/bin/env bash
# Runs a built-in skill the way Bazel runs it, over a real PR, and checks what
# came out. It costs tokens: a smoke test before shipping a skill change, not a
# unit test.
#
#   scripts/bazel-check.sh <owner/repo> <pr> <skill> [model]
#   scripts/bazel-check.sh beroni/bazel 25 bazel-review-fleet sonnet
#
# It never publishes: the prompt is Bazel's default one, which forbids writing
# to GitHub, and the run has no Write/Edit beyond what the skill grants itself.
# Do not point it at bazel-post-report.
set -uo pipefail

[ $# -ge 3 ] || { sed -n '2,10p' "$0" >&2; exit 2; }
REPO=$1 PR=$2 SKILL=$3 MODEL=${4:-}
[ "$SKILL" != bazel-post-report ] || { echo "bazel-check.sh does not run the publishing skill" >&2; exit 2; }
ROOT=$(cd "$(dirname "$0")/.." && pwd)
BUILTIN=$ROOT/internal/skills/builtin
[ -f "$BUILTIN/$SKILL/SKILL.md" ] || { echo "no built-in skill $SKILL" >&2; exit 2; }

WORK=$(mktemp -d "${TMPDIR:-/tmp}/bazel-check.XXXXXX")
CLONE=$WORK/clone
echo "workdir: $WORK"

# 1. The clone, as internal/workspace prepares it.
gh repo clone "$REPO" "$CLONE" -- --filter=blob:none -q || exit 1
(cd "$CLONE" && gh pr checkout "$PR" >/dev/null 2>&1) || { echo "gh pr checkout $PR failed" >&2; exit 1; }

# 2. The built-in skills, as skills.Materialize writes them.
mkdir -p "$CLONE/.claude/skills" "$CLONE/.claude/agents"
cp -R "$BUILTIN"/. "$CLONE/.claude/skills/"
cp "$BUILTIN"/*/agents/*.md "$CLONE/.claude/agents/"
echo ".claude/" >> "$CLONE/.git/info/exclude"

# 3. The prompt, as config.defaultPrompt fills it.
META=$(cd "$CLONE" && gh pr view "$PR" --json title,author,url,headRefName,baseRefName,body)
field() { jq -r "$1" <<<"$META"; }
BODY=$(field .body); [ -n "$BODY" ] || BODY="(no description)"
cat > "$WORK/prompt.md" <<PROMPT
/$SKILL $PR

The repository $REPO is cloned in this directory ($CLONE), with PR
#$PR already checked out on branch \`$(field .headRefName)\` over base \`$(field .baseRefName)\`.
This is a throwaway clone: read freely, but do not edit files, do not commit,
and do not publish anything to GitHub.

PR: $(field .title) — @$(field .author.login)
$(field .url)

PR description:
$BODY

Write only the final report to stdout, in Markdown.
PROMPT

# 4. The agent, with Bazel's default args.
ARGS=(-p --output-format stream-json --verbose --allowedTools Read,Grep,Glob,Bash,Agent)
[ -n "$MODEL" ] && ARGS+=(--model "$MODEL")
START=$(date +%s)
(cd "$CLONE" && claude "${ARGS[@]}" < "$WORK/prompt.md" > "$WORK/stream.jsonl" 2> "$WORK/stderr.log")
echo "agent exit: $? in $(( $(date +%s) - START ))s"
jq -r 'select(.type=="result") | .result' "$WORK/stream.jsonl" > "$WORK/report.md"
jq -r 'select(.type=="result") | "cost: $\(.total_cost_usd) · turns: \(.num_turns)"' "$WORK/stream.jsonl"

# 5. The checks.
FAIL=0
check() { if eval "$2"; then echo "  ok   $1"; else echo "  FAIL $1"; FAIL=1; fi; }
echo "checks:"
check "report is not empty" '[ -s "$WORK/report.md" ]'
check "skill never asked a question" '! grep -q "\"name\":\"AskUserQuestion\"" "$WORK/stream.jsonl"'
check "report starts with its header, no preamble" 'head -1 "$WORK/report.md" | grep -qE "^(<!-- (review-fleet|history-pr):v1|# )"'
check "no trailing file path or chat line" '! tail -3 "$WORK/report.md" | grep -qE "(→|\.reviews/|\.pr-history/)"'
check "not wrapped in a markdown fence" '! head -1 "$WORK/report.md" | grep -q "^\`\`\`"'
if head -1 "$WORK/report.md" | grep -q '^<!-- review-fleet:v1'; then
  check "validate-report.sh passes" 'bash "$BUILTIN/bazel-review-fleet/references/validate-report.sh" "$WORK/report.md"'
fi
(cd "$ROOT" && BAZEL_CHECK_REPORT=$WORK/report.md go test -count=1 -run TestCheckReport -v ./internal/skills/ | grep -E 'finding|SplitFindings')
echo "report: $WORK/report.md"
exit $FAIL
