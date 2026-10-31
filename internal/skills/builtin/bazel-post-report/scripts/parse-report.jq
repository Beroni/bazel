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
  elif .section == "human" then .human += [$l] | .asked += [(.headers | length) - 1]
  elif ($l | strip) != "" then .extra += [$l]
  else . end;

# A step's title with no report after it is a step that proves nothing, whatever
# it says: `## Critical: do not merge` is not a clean step.
def endstep:
  if .step != null then .deny += ["a step has no review-fleet header"] else . end
  | .loose = false | .step = null;

# Who owes the report that is opening: the step whose title came before it, or the
# only one there is. With no list, from a file older than the list, nobody knows.
def open_report($steps):
  (.step // (if ($steps | length?) == 1 then $steps[0] else null end)) as $name
  | if $steps == null then .
    elif $name == null then .deny += ["a report header is outside its step"]
    elif (.covered | index($name)) then .deny += ["the step \"\($name)\" has two report headers"]
    else .covered += [$name] end;

def section_of($title):
  ($title | lower | sub("^[0-9. ]+"; "")) as $t
  | if ($t | startswith("finding")) then "findings"
    elif ($t | startswith("cut")) then "cuts"
    elif ($t | startswith("needs human")) then "human"
    else null end;

def line($l; $next; $steps):
  # The block a report opens with: one H1 and three lines, in order. Each is read
  # once, and the first line that is anything else ends it and stays as text.
  def summary($t):
    if $t == "" then .
    elif .after == "h1" and ($t | test("^# ")) then .after = "verdict"
    elif .after == "verdict" and ($t | startswith("**Verdict:** ")) then
      ([$t | capture("^\\*\\*Verdict:\\*\\* +(?<v>[a-z_]+)( |$)")] | first | .v?) as $said
      | (if $said == .headers[-1].verdict then . else .deny += ["a report's summary line says another verdict than its header"] end)
      | .after = "scope"
    elif .after == "scope" and ($t | startswith("**Scope:** ")) then .after = "lenses"
    elif .after == "lenses" and ($t | startswith("**Lenses:** ")) then .after = null | .headers[-1].complete = true
    else .after = null | line($l; $next; $steps) end;
  ($l | strip) as $t
  | if .mode == "header" then
      if $t == "-->" then .headers += [.hdr] | .hdr = null | .mode = "body" | .after = "h1"
      else ([$l | capture("^(?<k>[a-z_]+): (?<v>.*)$")] | first) as $kv
        | if $kv then .hdr[$kv.k] = ($kv.v | strip) else .deny += ["a header line is not `key: value`"] end
      end
    elif .fence then
      .fence as $open | emit($l) | if ($t | startswith($open)) then .fence = null else . end
    elif $t == "<!-- review-fleet:v1" then
      flush | open_report($steps) | .section = null | .expect = false | .loose = false | .step = null
      | .after = null | .mode = "header" | .hdr = {}
    elif .after then summary($t)
    elif .expect then
      # Text around a report is kept: narration, usually, and it goes to the PR in
      # the open, where it also rules out an all-clear.
      if $t == "" then .
      elif $t == "---" then endstep
      elif ($t | test("^## ")) and ($steps == null or ($steps | index($t | sub("^## +"; "")))) then
        endstep | .step = ($t | sub("^## +"; ""))
      elif ($t | startswith("> ✗ failed")) and .step != null then
        .failed += [.step] | .covered += [.step] | .step = null | .loose = false
      else .loose = true | .extra += [$l] end
    elif $t == "---" and ($steps == null or (($next | test("^## ")) and ($steps | index($next | sub("^## +"; ""))))) then
      flush | .section = null | .expect = true
    elif ($t | startswith("```")) or ($t | startswith("~~~")) then
      .fence = $t[0:3] | emit($l)
    else
      if ($t | test("^## ")) then
        flush | .section = section_of($t | sub("^## +"; ""))
        | if .section == null then emit($l) else . end
      elif ($t | test("^### ")) and (.section == "findings" or .section == "cuts") then
        flush | .cur = {title: ($t | sub("^### +"; "")), cut: (.section == "cuts"), meta: null, prose: [], report: ((.headers | length) - 1)}
      elif .cur and .cur.meta == null and .cur.prose == [] and ($t | test("^<!-- finding .* -->$")) then
        .cur.meta = ($t | sub("^<!-- finding "; "") | sub(" -->$"; "") | attrs)
      elif .section == "human" and $t == "<!-- needs-human -->" then .
      else emit($l) end
    end;

(. | split("\n")) as $all
| ($all | index("---")) as $sep
| ($all[0] // "") as $first
| ([$first | capture("^# (?<repo>[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#(?<n>[0-9]+)( |$)")] | first) as $target
| ([$all[0:($sep // 0)][] | select(startswith("- Head sha: ")) | sub("^- Head sha: "; "") | strip] | first // "") as $sha
| ($all[0:($sep // 0)]) as $bazel
| ([$bazel[] | select(startswith("- Steps: ")) | sub("^- Steps: "; "") | (fromjson? // null)] | first
   | if type == "array" and . != [] and all(.[]; type == "string") then . else null end) as $steps
| ($all[(($sep // -1) + 1):]) as $body
# The next line with text after each one, for what a `---` is: Bazel's separator
# only if a step's title comes next, or a rule inside the report if not.
| (reduce range(($body | length) - 1; -1; -1) as $i ({next: "", out: []};
     .out[$i] = .next | if ($body[$i] | strip) != "" then .next = ($body[$i] | strip) else . end) | .out) as $nexts
| reduce range(0; $body | length) as $i (
    {mode: "body", expect: true, loose: false, after: null, fence: null, section: null, cur: null, step: null,
     hdr: null, headers: [], findings: [], human: [], asked: [], extra: [], failed: [], covered: [], deny: []};
    line($body[$i]; $nexts[$i]; $steps))
| flush
| endstep
| if .mode == "header" then .deny += ["a report header is not closed"] else . end
| .deny += [($steps // [])[] as $name | select(.covered | index($name) | not) | "the step \"\($name)\" has no report"]
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
# An approve over its own findings contradicts itself. Unticking only takes
# findings away, so this holds for a trimmed copy too. `comment` over a major is
# how older reports rate one: softer than the validator allows now, but it claims
# nothing the findings below it do not show.
| . as $s
| .deny += [range(0; .headers | length) as $i | $s.headers[$i].verdict as $v
    | ($s.findings | map(select(.report == $i))) as $mine
    | if $v == "approve" and ($mine != [] or ($s.asked | index($i))) then "a report says approve and still lists findings or questions" else empty end]
| (.headers | map(.verdict)) as $v
| {repo: $repo, number: $n, reviewed_sha: $sha, deny: .deny,
   # The sha is proven only if Bazel's header and every report agree on it.
   sha_proven: (($sha | test("^[0-9a-f]{40}$")) and all(.headers[]; .head_sha == $sha)),
   # What an all-clear rests on. Each of these has to be shown, none is assumed.
   steps_proven: ($steps != null),
   complete: all(.headers[]; .complete == true),
   trimmed: any($bazel[]; startswith("- Trimmed: ")),
   truncated: any($bazel[]; contains("The diff was truncated")),
   scope_real: all(.headers[]; .scope_cmd != "-"),
   lenses_real: all(.headers[]; .lenses_ran != "-"),
   verdict: (if any($v[]; . == "request_changes") then "request_changes"
             elif any($v[]; . == "comment") then "comment" else "approve" end),
   # The steps review the same diff: adding them up would count it twice.
   files: ([.headers[].files | tonumber? // 0] | max // 0),
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
