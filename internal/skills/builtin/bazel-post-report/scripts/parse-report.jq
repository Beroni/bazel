# Reads a review Bazel saved (jq -Rs) into what publish.sh acts on. Anything it
# cannot prove stays out of the metadata: a default-deny reader, because the file
# quotes someone else's PR. `deny` lists why the review cannot be published.

def strip: gsub("^\\s+|\\s+$"; "");
def lower: ascii_downcase;

def attrs: [scan("([a-z_]+)=(\\S+)")] | map({key: .[0], value: .[1]}) | from_entries;

def flush:
  if .cur then .findings += [.cur] | .cur = null else . end;

def emit($l):
  if .cur then .cur.prose += [$l]
  elif .section == "human" then .human += [$l]
  elif ($l | strip) != "" then .extra += [$l]
  else . end;

def section_of($title):
  ($title | lower | sub("^[0-9. ]+"; "")) as $t
  | if ($t | startswith("finding")) then "findings"
    elif ($t | startswith("cut")) then "cuts"
    elif ($t | startswith("needs human")) then "human"
    else null end;

def line($l):
  ($l | strip) as $t
  | if .mode == "header" then
      if $t == "-->" then .headers += [.hdr] | .hdr = null | .mode = "body" | .after_header = true
      else (try ($l | capture("^(?<k>[a-z_]+): (?<v>.*)$")) catch null) as $kv
        | if $kv then .hdr[$kv.k] = ($kv.v | strip) else .deny += ["a header line is not `key: value`"] end
      end
    elif .fence then
      emit($l) | if ($t | startswith(.fence)) then .fence = null else . end
    elif $t == "<!-- review-fleet:v1" then
      flush | .section = null | .expect = false | .mode = "header" | .hdr = {}
    elif .expect then
      if $t == "" or $t == "---" then .
      elif ($t | test("^## ")) then .step = ($t | sub("^## +"; ""))
      elif ($t | startswith("> ✗ failed")) then .failed += [.step // "a step"]
      else .deny += ["there is text where a report header should be"] | .expect = false | emit($l) end
    elif $t == "---" then
      flush | .section = null | .expect = true | .after_header = false
    elif ($t | startswith("```")) or ($t | startswith("~~~")) then
      .after_header = false | .fence = $t[0:3] | emit($l)
    elif .after_header and ($t == "" or ($t | test("^(# |\\*\\*Verdict:\\*\\*|\\*\\*Scope:\\*\\*|\\*\\*Lenses:\\*\\*)"))) then .
    else
      .after_header = false
      | if ($t | test("^## ")) then
          flush | .section = section_of($t | sub("^## +"; ""))
          | if .section == null then emit($l) else . end
        elif ($t | test("^### ")) and (.section == "findings" or .section == "cuts") then
          flush | .cur = {title: ($t | sub("^### +"; "")), cut: (.section == "cuts"), meta: null, prose: []}
        elif .cur and .cur.meta == null and .cur.prose == [] and ($t | test("^<!-- finding .* -->$")) then
          .cur.meta = ($t | sub("^<!-- finding "; "") | sub(" -->$"; "") | attrs)
        elif .section == "human" and $t == "<!-- needs-human -->" then .
        else emit($l) end
    end;

(. | split("\n")) as $all
| ($all | index("---")) as $sep
| ($all[0] // "") as $first
| (try ($first | capture("^# (?<repo>[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#(?<n>[0-9]+)( |$)")) catch null) as $target
| ([$all[0:($sep // 0)][] | select(startswith("- Head sha: ")) | sub("^- Head sha: "; "") | strip] | first // "") as $sha
| reduce ($all[(($sep // -1) + 1):][]) as $l (
    {mode: "body", expect: true, after_header: false, fence: null, section: null, cur: null, step: null,
     hdr: null, headers: [], findings: [], human: [], extra: [], failed: [], deny: []};
    line($l))
| flush
| if .mode == "header" then .deny += ["a report header is not closed"] else . end
| ($target.repo // "") as $repo
| ($target.n // "") as $n
| .deny += (if $target == null then ["the first line does not name owner/repo#N"] else [] end)
| .deny += (if $sep == null then ["the file has no Bazel header"] else [] end)
| .deny += (if .headers == [] then ["the report has no review-fleet header"] else [] end)
| .deny += [.headers[] as $h
    | (["repo","pr","base","head_sha","scope_cmd","files","lenses_ran","lenses_skipped","verdict"]
        | map(select(($h[.] // "") == "")) | if . != [] then "a report header lacks: \(join(", "))" else empty end),
      (if ($h.files // "" | test("^[1-9][0-9]*$")) then empty
       else "a report header says files: \($h.files // "?"), so it covered no code" end),
      (if ($h.verdict // "" | IN("request_changes","comment","approve")) then empty
       else "a report header has an unknown verdict" end),
      (if ($h.repo // "" | lower) == ($repo | lower) then empty
       else "a report header names another repository" end),
      (if ($h.pr // "") == $n then empty else "a report header names another PR" end)]
| (.headers | map(.verdict)) as $v
| {repo: $repo, number: $n, reviewed_sha: $sha, deny: .deny,
   # The sha is proven only if Bazel's header and every report agree on it.
   sha_proven: (($sha | test("^[0-9a-f]{40}$")) and all(.headers[]; .head_sha == $sha)),
   verdict: (if any($v[]; . == "request_changes") then "request_changes"
             elif any($v[]; . == "comment") then "comment" else "approve" end),
   files: ([.headers[].files | tonumber? // 0] | add // 0),
   scope: (.headers | map(.scope_cmd) | unique),
   lenses: (.headers | map(.lenses_ran) | unique),
   skipped: (.headers | map(.lenses_skipped) | map(select(. != "none")) | unique),
   failed: .failed,
   human: (.human | join("\n") | strip),
   extra: (.extra | join("\n") | strip),
   findings: (.findings | to_entries | map(.key as $i | .value as $f | ($f.meta // {}) as $m | {
     id: ($i + 1), title: $f.title, cut: $f.cut,
     severity: ($m.severity // "unknown"), class: ($m.class // "unknown"),
     path: (if ($m.path // "-") == "-" then null else $m.path end),
     line: (if ($m.line // "" | test("^[1-9][0-9]*$")) then ($m.line | tonumber) else null end),
     side: (if $m.side == "LEFT" then "LEFT" else "RIGHT" end),
     start_line: (if ($m.start_line // "" | test("^[1-9][0-9]*$")) then ($m.start_line | tonumber) else null end),
     start_side: (if $m.start_side == "LEFT" then "LEFT" elif $m.start_side == "RIGHT" then "RIGHT" else null end),
     ref: (if ($m.ref // "" | test("^[A-Za-z0-9._:,-]+$")) then $m.ref else null end),
     prose: ($f.prose | join("\n") | strip)}))}
