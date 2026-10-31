# Turns the plan and the model's --same pairs into what gets written: one review,
# or nothing. The body is built here, the prose goes verbatim, and the event is a
# constant, so no text in the report can reach the shell or the payload's shape.

include "text";

def code: gsub("`"; "'") | gsub("[\\r\\n]"; " ");
def where: if .path then "`\(.path | code)`\(if .line then ":\(.line)" else "" end)" else "no file" end;
def level: "\(.severity | code) · \(.class | code)";
def told: if .prose == "" then .title else .prose end;
def plural($n; $w): "\($n) \($w)\(if $n == 1 then "" else "s" end)";
def section($t; $items): if $items == [] then [] else ["### \($t)", ($items | join("\n\n"))] end;
# A finding that is not a comment of its own is still all there, in case what
# stands for it on the PR goes away.
def entry($line): "#### \(.title | code | safe)\n\($line)\n\n\(told | quoted)\(if .ref then "\n\n\(.ref | code)" else "" end)";

. as $p
| ($moved == "yes") as $moved
| ($p.reviewed_sha // "") as $sha
| ($p.sha_proven and ($moved | not)) as $anchors
| [$p.findings[]
    | ($same[.id | tostring] // -1) as $wanted
    | (.candidates | map(select(.comment_id == $wanted))) as $c
    | . + {match: ($c | first)}
    | . + {destination: (
        if .match and .match.settled then "settled"
        elif .match then "duplicate"
        elif .commentable and $anchors then "inline"
        else "body" end)}] as $all
| ($all | map(select(.destination == "inline"))) as $inline
| ($all | map(select(.destination == "body"))) as $bodied
| ($all | map(select(.destination == "duplicate"))) as $dups
| ($all | map(select(.destination == "settled"))) as $settled
| (($all | length) == 0 and $p.human == "" and $p.extra == "" and $p.failed == []) as $clean
# An all-clear says the whole diff was looked at and nothing came out of it. Each
# of these takes that away, and none of them shows as a finding.
| ([(if $moved then "the PR moved after the review" else empty end),
    (if $p.sha_proven | not then "the reviewed commit could not be proven" else empty end),
    (if $p.steps_proven | not then "the file does not say which steps ran" else empty end),
    (if $p.complete | not then "the report stops before its summary is complete" else empty end),
    (if $p.truncated then "the diff was truncated before the agent read it" else empty end),
    (if $p.scope_real | not then "the report names no scope" else empty end),
    (if $p.lenses_real | not then "the report names no lens that ran" else empty end),
    (if any($p.skipped[] | split(", ")[]; endswith("(single-lens run)") | not) then "a lens was skipped" else empty end)
   ]) as $unproven
| def comment:
    {path, line, side, body: ((told | safe) + (if .ref then "\n\n\(.ref)" else "" end))}
    + (if .start_line then {start_line, start_side: (.start_side // .side)} else {} end);
  def coverage:
    "**Scope:** `\($p.scope | map(code) | join("`, `"))` — \(plural($p.files; "file"))\n**Lenses:** \($p.lenses | map(code) | join(", "))"
    + (if $p.skipped == [] then "" else " (skipped: \($p.skipped | map(code) | join(", ")))" end);
  (if $moved then "\n\n> Reviewed at `\($sha[0:7])`; the PR has moved to `\($head[0:7])` since, so nothing here is line-anchored and this says nothing about the newer commits."
   elif ($p.sha_proven | not) then "\n\n> The reviewed commit could not be proven, so nothing here is line-anchored."
   else "" end) as $note
| ( if $clean and $p.verdict == "approve" and ($p.trimmed | not) and $unproven == [] then
      {kind: "all-clear",
       body: "## Automated review — Bazel · no findings\n\n\(coverage)\n\nReviewed at `\($sha[0:7])`.\n\n<!-- review-fleet: all-clear @ \($sha) -->"}
    elif $clean and ($p.verdict != "approve" or $p.trimmed) then
      {kind: "nothing", why: "the report's verdict is \($p.verdict) and no finding, cut or question is left in the file, so there is nothing to post and no all-clear to give"}
    elif $clean then
      {kind: "review", body: "## Automated review — Bazel\n\nThe report lists no findings, but that does not clear the PR: \($unproven | join("; ")).\n\n\(coverage)"}
    else
      {kind: "review", body: (
        (["## Automated review — Bazel\(if $note != "" then $note else "" end)",
          "**Verdict in the report:** \($p.verdict) · \(plural($all | length; "finding")) in this review",
          coverage]
         + section("Findings that could not be anchored";
             $bodied | map(entry("_Location in the report: \(where)_")))
         + section("Already on the PR"; $dups | map(entry("\(where) (\(level)) — [comment](\(.match.url)) by @\(.match.user | code)")))
         + section("Already settled on the PR"; $settled | map(entry("\(where) (\(level)) — settled by @\(.match.settled_by | code), [thread](\(.match.url))")))
         + section("Open questions"; if $p.human == "" then [] else [$p.human | quoted] end)
         + section("Steps that failed"; $p.failed | map("- \(. | code)"))
         + section("Unverified text that came with the report"; if $p.extra == "" then [] else ["_Outside the report's format: Bazel does not vouch for it._", ($p.extra | quoted)] end)
         + (if $sha == "" then [] else ["Reviewed at `\($sha[0:7] | code)`."] end))
        | join("\n\n"))}
    end) as $what
| $what + {
    comments: ($inline | map(comment)),
    bodied: ($bodied | map({id, title, path, line})),
    reactions: ($dups | map(.match.comment_id)),
    counts: {inline: ($inline | length), body: ($bodied | length), duplicates: ($dups | length), settled: ($settled | length)},
    settled: ($settled | map({path, line, level: level, by: .match.settled_by})),
    duplicates: ($dups | map({path, line, user: .match.user}))}
