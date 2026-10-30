# Turns the plan and the model's --same pairs into what gets written: one review,
# or nothing. The body is built here, the prose goes verbatim, and the event is a
# constant, so no text in the report can reach the shell or the payload's shape.

def code: gsub("`"; "'") | gsub("[\\r\\n]"; " ");
def where: if .path then "`\(.path | code)`\(if .line then ":\(.line)" else "" end)" else "no file" end;
def level: "\(.severity | code) · \(.class | code)";
def plural($n; $w): "\($n) \($w)\(if $n == 1 then "" else "s" end)";
def section($t; $items): if $items == [] then [] else ["### \($t)", ($items | join("\n\n"))] end;

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
| def comment:
    {path, line, side, body: ((if .prose == "" then .title else .prose end) + (if .ref then "\n\n\(.ref)" else "" end))}
    + (if .start_line then {start_line, start_side: (.start_side // .side)} else {} end);
  def coverage:
    "**Scope:** `\($p.scope | map(code) | join("`, `"))` — \(plural($p.files; "file"))\n**Lenses:** \($p.lenses | map(code) | join(", "))"
    + (if $p.skipped == [] then "" else " (skipped: \($p.skipped | map(code) | join(", ")))" end);
  (if $moved then "\n\n> Reviewed at `\($sha[0:7])`; the PR has moved to `\($head[0:7])` since, so nothing here is line-anchored and this says nothing about the newer commits."
   elif ($p.sha_proven | not) then "\n\n> The reviewed commit could not be proven, so nothing here is line-anchored."
   else "" end) as $note
| ( if $clean and $p.verdict == "approve" and $anchors then
      {kind: "all-clear",
       body: "## Automated review — Bazel · no findings\n\n\(coverage)\n\nReviewed at `\($sha[0:7])`.\n\n<!-- review-fleet: all-clear @ \($sha) -->"}
    elif $clean and $p.verdict == "approve" then
      {kind: "review", body: "## Automated review — Bazel\n\nNo findings at `\($sha[0:7] | code)`.\n\n\(coverage)\(if $moved or ($p.sha_proven | not) then $note else "" end)"}
    elif $clean then
      {kind: "nothing", why: "the report's verdict is \($p.verdict) and no finding, cut or question is left in the file, so there is nothing to post and no all-clear to give"}
    else
      {kind: "review", body: (
        (["## Automated review — Bazel\(if $note != "" then $note else "" end)",
          "**Verdict in the report:** \($p.verdict) · \(plural($all | length; "finding")) in this review",
          coverage]
         + section("Findings that could not be anchored";
             $bodied | map("#### \(.title | code)\n\((if .prose == "" then .title else .prose end))\(if .ref then "\n\n\(.ref)" else "" end)\n\n_Location in the report: \(where)_"))
         + section("Already on the PR"; $dups | map("- \(where) (\(level)) — [comment](\(.match.url)) by @\(.match.user | code)"))
         + section("Already settled on the PR"; $settled | map("- \(where) (\(level)) — already settled on the PR (by @\(.match.settled_by | code)), [thread](\(.match.url))"))
         + section("Open questions"; if $p.human == "" then [] else [$p.human] end)
         + section("Steps that failed"; $p.failed | map("- \(. | code)"))
         + section("Other text in the report"; if $p.extra == "" then [] else [$p.extra] end)
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
