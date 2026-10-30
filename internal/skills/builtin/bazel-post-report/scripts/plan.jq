# Joins the report, the diff and what is on the PR. Rules that need no judgment
# are settled here; the only thing left for the model is whether a comment near a
# finding is about the same root cause, and for that it gets `candidates`.

($pr[0].author.login // "") as $author
| ($me[0]) as $me
| ($report[0]) as $r
| ($comments[0] | map(select(.in_reply_to_id != null)) | group_by(.in_reply_to_id)
    | map({key: (.[0].in_reply_to_id | tostring), value: .}) | from_entries) as $replies
| ($threads[0] | map({key: (.root | tostring), value: .}) | from_entries) as $thread
| ($comments[0] | map(select(.in_reply_to_id == null))
    # A comment absorbs a finding only if it comes from someone who can speak for
    # the repository, or from an earlier run of this same account: never from the
    # PR's author or a passer-by, who could use it to make a finding disappear.
    | map(select(.user.login == $me or
        ((.author_association // "NONE" | IN("OWNER", "MEMBER", "COLLABORATOR")) and .user.login != $author)))) as $roots
| def commentable($f):
    $f.path != null and $f.line != null and $r.sha_proven
    and ($diff[0][$f.path][$f.side][$f.line | tostring] // false)
    and ($f.start_line == null or ($diff[0][$f.path][$f.start_side // $f.side][$f.start_line | tostring] // false));
  def candidates($f):
    if $f.path == null or $f.line == null then [] else
      [$roots[] | select(.path == $f.path and ((.line // .original_line // -100) - $f.line | fabs) <= 5)
       | (.id | tostring) as $id
       | ($thread[$id].isResolved // false) as $resolved
       | ($replies[$id] // []) as $rs
       | {comment_id: .id, user: .user.login, url: .html_url, excerpt: (.body // "")[0:400],
          settled: ($resolved or ($rs != [])),
          settled_by: (if $resolved then ($thread[$id].resolvedBy // "unknown")
                       elif $rs != [] then $rs[-1].user.login else null end)}]
    end;
  $r + {
    pr_url: ($pr[0].url // ""),
    me: $me,
    own_bodies: ([$issue[0][], $reviews[0][] | select(.user.login == $me) | .body // ""]),
    findings: [$r.findings[] | . + {commentable: commentable(.), candidates: candidates(.)}]}
