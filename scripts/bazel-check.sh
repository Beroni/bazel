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

# 2. The built-in skills, as skills.Materialize writes them: a plugin in a
#    folder of its own, outside the clone. Nothing is written into the clone.
PLUGIN=$WORK/plugin
mkdir -p "$PLUGIN/.claude-plugin" "$PLUGIN/skills" "$PLUGIN/agents"
printf '{\n  "name": "bazel",\n  "description": "Skills that ship inside Bazel"\n}\n' > "$PLUGIN/.claude-plugin/plugin.json"
cp -R "$BUILTIN"/. "$PLUGIN/skills/"
cp "$BUILTIN"/*/agents/*.md "$PLUGIN/agents/"

# 3. The PR's agent configuration leaves the working tree, as workspace.Neutralize
#    does it: at any depth, in any case, no symlink followed.
(cd "$CLONE" && find . -name .git -prune -o \
  \( -iname .claude -o -iname .codex -o -iname .grok -o -iname .mcp.json \) -prune -exec rm -rf {} +)

# 4. The prompt, as config.defaultPrompt fills it.
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

# 5. The agent, with the flags confineClaude adds ahead of Bazel's default args.
ARGS=(--setting-sources user --strict-mcp-config --plugin-dir "$PLUGIN"
  -p --output-format stream-json --verbose --allowedTools Read,Grep,Glob,Bash,Agent)
[ -n "$MODEL" ] && ARGS+=(--model "$MODEL")
START=$(date +%s)
(cd "$CLONE" && claude "${ARGS[@]}" < "$WORK/prompt.md" > "$WORK/stream.jsonl" 2> "$WORK/stderr.log")
echo "agent exit: $? in $(( $(date +%s) - START ))s"
# Bazel keeps the last result event (internal/agent/stream.go); so does this.
jq -rs 'map(select(.type=="result")) | last | .result // ""' "$WORK/stream.jsonl" > "$WORK/report.md"
RESULTS=$(jq -s 'map(select(.type=="result")) | length' "$WORK/stream.jsonl")
jq -rs 'map(select(.type=="result")) | last | "cost: $\(.total_cost_usd) · turns: \(.num_turns)"' "$WORK/stream.jsonl"

# 6. The checks.
FAIL=0
check() { if eval "$2"; then echo "  ok   $1"; else echo "  FAIL $1"; FAIL=1; fi; }
echo "checks:"
check "report is not empty" '[ -s "$WORK/report.md" ]'
check "the built-in skill loaded from the plugin" 'jq -e "select(.type==\"system\" and .subtype==\"init\") | .skills | map(tostring) | any(. == \"bazel:$SKILL\" or . == \"$SKILL\")" "$WORK/stream.jsonl" >/dev/null'
check "nothing written into the clone" '! git -C "$CLONE" status --porcelain --ignored -uall | grep -vq "^ D "'
check "one turn, no progress notes before the report ($RESULTS results)" '[ "$RESULTS" = 1 ]'
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
