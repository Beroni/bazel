#!/usr/bin/env bash
# Validates a review report against report-format.md, next to this script.
#
#   bash validate-report.sh "$R"     # R: the scratch file the report was written to
#
# Exits 0 and stays quiet when the file conforms; exits 1 and prints `file:line: error`
# for every deviation. A standard that does not check itself drifts again — the review skills
# run this before printing. bazel-post-report does not: the copy Bazel hands it may have an
# emptied section after unticked findings, which this rejects.

set -uo pipefail

[ $# -eq 1 ] || { echo "usage: validate-report.sh <report.md>" >&2; exit 2; }
[ -f "$1" ] || { echo "validate-report.sh: $1 does not exist" >&2; exit 2; }

awk -v FILE="$1" '
function err(line, msg) { printf "%s:%d: %s\n", FILE, line, msg; bad++ }
function close_finding() {
  if (pending_level) err(pending_level, "finding has no level line — the prose must open with `**<severity> · <class>** (<lenses>)`")
  if (pending_fix)   err(pending_fix,   "finding has no `**Fix:**` block")
  pending_level = 0; pending_fix = 0
}

BEGIN {
  split("repo pr base head_sha scope_cmd files lenses_ran lenses_skipped verdict", KEY, " ")
  NKEY = 9
  split("critical major minor nit", t, " ");        for (i in t) SEV[t[i]] = i
  split("request_changes comment approve", t, " "); for (i in t) VERDICT[t[i]] = 1
  split("senior-code-reviewer exploit-digger lazy-senior-dev", t, " ")
  for (i in t) LENS[t[i]] = 1
  split("injection authz validation secrets crypto web3 " \
        "logic error-handling regression test-gap test-validity assumption " \
        "perf resource architecture quality migration idiom deps " \
        "a11y docs hygiene pr-mismatch " \
        "delete reuse stdlib native drop-dep yagni shrink out-of-scope", t, " ")
  for (i in t) CLASS[t[i]] = 1
  split("delete reuse stdlib native drop-dep yagni shrink", t, " ")
  for (i in t) SUBTRACTION[t[i]] = 1
  SECTION_ORDER["Findings"] = 1; SECTION_ORDER["Cuts"] = 2      # canonical order
  SECTION_ORDER["Needs human verification"] = 3
  MARKER["Needs human verification"] = "<!-- needs-human -->"
  split("severity class lenses path line side start_line start_side confidence lines_saved ref", t, " ")
  for (i in t) ATTR_OK[t[i]] = 1
  ATTR_POS["severity"]=1; ATTR_POS["class"]=2; ATTR_POS["lenses"]=3; ATTR_POS["path"]=4
  ATTR_POS["line"]=5; ATTR_POS["side"]=6; ATTR_POS["start_line"]=7; ATTR_POS["start_side"]=8
  ATTR_POS["confidence"]=9; ATTR_POS["lines_saved"]=10; ATTR_POS["ref"]=11
  state = "header"; lastsec = 0
}

# ---------- header ----------
NR == 1 {
  if ($0 != "<!-- review-fleet:v1") err(NR, "line 1 must be exactly `<!-- review-fleet:v1`")
  next
}
state == "header" {
  if ($0 == "-->") {
    if (ki < NKEY) err(NR, "header closed without: " KEY[ki+1])
    state = "after-header"; next
  }
  ki++
  if (ki > NKEY) { err(NR, "extra key in the header: `" $0 "` — there are 9 and only 9"); next }
  want = KEY[ki]
  if (index($0, want ": ") != 1) {
    # is the key that arrived a later one? then the ones in between are missing
    for (j = ki + 1; j <= NKEY; j++) if (index($0, KEY[j] ": ") == 1) break
    if (j <= NKEY) {
      for (k = ki; k < j; k++) err(NR, "required key missing: `" KEY[k] "`")
      ki = j; want = KEY[ki]
    } else {
      err(NR, "unknown or out-of-order key in the header: `" $0 "`")
      ki--; next
    }
  }
  val = substr($0, length(want) + 3)
  HDR[want] = val
  if (val == "") { err(NR, "`" want "` is empty"); next }
  if (want == "repo"     && val !~ /^[^\/ ]+\/[^\/ ]+$/) err(NR, "`repo` must be owner/name")
  if (want == "pr"       && val !~ /^([0-9]+|none)$/)    err(NR, "`pr` must be a number or `none`")
  if (want == "head_sha" && val !~ /^[0-9a-f]{40}$/)     err(NR, "`head_sha` must be the full sha, 40 hex")
  if (want == "files") {
    if (val !~ /^[0-9]+$/)   err(NR, "`files` must be an integer")
    else if (val + 0 == 0)   err(NR, "`files: 0` is a scope-resolution failure, not a valid report")
  }
  if (want == "verdict" && !(val in VERDICT)) err(NR, "`verdict` outside the vocabulary: `" val "`")
  if (want == "lenses_ran") {
    n = split(val, L, ", ")
    for (i = 1; i <= n; i++) if (!(L[i] in LENS)) err(NR, "unknown lens in `lenses_ran`: `" L[i] "`")
  }
  next
}

# ---------- title and summary block ----------
state == "after-header" {
  if ($0 == "") next
  if ($0 !~ /^# (Review Fleet|Senior Code Reviewer|Exploit Digger|Lazy Senior Dev) — .+/)
    err(NR, "H1 does not match `# <Producer> — <target>`")
  state = "want-verdict"; next
}
state == "want-verdict" {
  if ($0 == "") next
  if (index($0, "**Verdict:** ") != 1) { err(NR, "expected the `**Verdict:** …` line right after the H1"); state = "body" }
  else {
    v = substr($0, 14); sub(/ .*/, "", v)
    if (v != HDR["verdict"]) err(NR, "the summary verdict (`" v "`) does not match the header (`" HDR["verdict"] "`)")
    if ($0 !~ /(no findings|[0-9]+ findings?)/) err(NR, "the Verdict line needs the finding count")
    state = "want-scope"
  }
  next
}
state == "want-scope" {
  if (index($0, "**Scope:** ") != 1) err(NR, "expected `**Scope:** …` on the next line, with no blank line in between")
  else {
    if ($0 !~ / files · base `/) err(NR, "the Scope line needs `— N files · base `<base>` · head `<sha7>``")
    if ($0 !~ /head `[0-9a-f]{7}`/) err(NR, "the Scope line needs the head as a short 7-char sha")
  }
  state = "want-lenses"; next
}
state == "want-lenses" {
  if (index($0, "**Lenses:** ") != 1) err(NR, "expected `**Lenses:** …` on the third summary line")
  else if (index($0, HDR["lenses_ran"]) == 0) err(NR, "the Lenses line does not repeat `lenses_ran` from the header")
  else if (HDR["lenses_skipped"] != "none" && index($0, "(skipped: ") == 0)
    err(NR, "`lenses_skipped` is not `none`, so the Lenses line needs the `(skipped: …)`")
  state = "body"; next
}

# ---------- body ----------
state != "body" { next }

# A fence is prose: a `# comment` in a code block is no heading.
/^ *(```|~~~)/ { infence = !infence; next }
infence { next }

/^# / { err(NR, "there may be only one H1, at the top") ; next }

/^## / {
  close_finding()
  sec = substr($0, 4)
  if (!(sec in SECTION_ORDER)) { err(NR, "section `" sec "` does not exist in the format — the divisions are Findings, Cuts and Needs human verification"); cur = ""; next }
  if (SECTION_ORDER[sec] <= lastsec) err(NR, "section `" sec "` out of order")
  if (seen[sec]++) err(NR, "section `" sec "` appears twice")
  lastsec = SECTION_ORDER[sec]; cur = sec; lastsev = 0; lastsaved = 999999
  want_marker = (sec in MARKER) ? MARKER[sec] : ""
  has_item[sec] = 0
  next
}

want_marker != "" {
  if ($0 == "") next
  if ($0 != want_marker) err(NR, "section `" cur "` needs the `" want_marker "` marker right below the heading")
  want_marker = ""; next
}

/^### / {
  close_finding()
  if (cur != "Findings" && cur != "Cuts") { err(NR, "finding outside `## Findings` / `## Cuts`"); next }
  has_item[cur] = 1
  if ($0 ~ /[.:]$/)  err(NR, "a finding title takes no trailing punctuation")
  if ($0 ~ /^### \[?(critical|major|minor|nit)/) err(NR, "severity goes in the metadata, not in the title")
  pending_meta = NR; title_line = NR
  next
}

pending_meta == NR - 1 {
  pending_meta = 0
  if ($0 !~ /^<!-- finding .* -->$/) { err(NR, "the `<!-- finding … -->` must be on the line immediately after the `###`"); next }
  meta = $0; sub(/^<!-- finding /, "", meta); sub(/ -->$/, "", meta)
  n = split(meta, A, " ")
  delete got; prev = 0
  for (i = 1; i <= n; i++) {
    eq = index(A[i], "=")
    if (eq == 0) { err(NR, "attribute with no `=`: `" A[i] "`"); continue }
    k = substr(A[i], 1, eq - 1); v = substr(A[i], eq + 1)
    if (!(k in ATTR_OK)) { err(NR, "unknown attribute: `" k "`"); continue }
    if (ATTR_POS[k] < prev) err(NR, "attribute `" k "` out of the table order")
    prev = ATTR_POS[k]; got[k] = v
    if (v ~ /["\x27]/) err(NR, "attribute `" k "` is quoted — no value in the format contains a space")
  }
  for (k = 1; k <= 6; k++) { need = (k==1?"severity":k==2?"class":k==3?"lenses":k==4?"path":k==5?"line":"side")
    if (!(need in got)) err(NR, "required attribute missing: `" need "`") }
  if (("severity" in got) && !(got["severity"] in SEV)) err(NR, "severity outside the vocabulary: `" got["severity"] "`")
  if (("class" in got) && !(got["class"] in CLASS))     err(NR, "class outside the vocabulary: `" got["class"] "`")
  if ("lenses" in got) { m = split(got["lenses"], LL, "+")
    for (i = 1; i <= m; i++) if (!(LL[i] in LENS)) err(NR, "unknown lens in `lenses`: `" LL[i] "`")
    else if (index(HDR["lenses_ran"], LL[i]) == 0) err(NR, "finding credited to `" LL[i] "`, which is not in `lenses_ran`") }
  if (("side" in got) && got["side"] !~ /^(RIGHT|LEFT)$/) err(NR, "side must be RIGHT or LEFT")
  if (("line" in got) && got["line"] !~ /^([0-9]+|-)$/)    err(NR, "line must be an integer or `-`")
  if (("path" in got) && ("line" in got) && ((got["path"] == "-") != (got["line"] == "-")))
    err(NR, "`path=-` and `line=-` always travel together")
  if (cur == "Findings" && (got["severity"] == "critical" || got["severity"] == "major")) blocker = 1
  if (cur == "Cuts") {
    if (!("lines_saved" in got)) err(NR, "a finding in `## Cuts` needs `lines_saved`")
    else if (got["lines_saved"] !~ /^[0-9]+$/) err(NR, "`lines_saved` must be an integer")
    if (("class" in got) && !(got["class"] in SUBTRACTION)) err(NR, "`## Cuts` only takes a class from the Subtraction group, got `" got["class"] "`")
    if (got["severity"] == "critical") err(NR, "a cut is never `critical` — a simplification does not block a merge")
    if (("lines_saved" in got) && got["lines_saved"] + 0 > lastsaved) err(NR, "cuts go from the biggest `lines_saved` to the smallest")
    if ("lines_saved" in got) lastsaved = got["lines_saved"] + 0
  } else if ("lines_saved" in got) err(NR, "`lines_saved` only exists in `## Cuts`")
  # Cuts are ordered by lines_saved alone; severity orders the other sections.
  if (cur != "Cuts" && ("severity" in got)) {
    if (SEV[got["severity"]] < lastsev) err(NR, "findings out of order: `" got["severity"] "` after a lower severity")
    lastsev = SEV[got["severity"]]
  }
  if (HDR["verdict"] == "approve") err(NR, "`verdict: approve` with a finding in the file")
  nfind++; pending_fix = title_line; pending_level = title_line
  # the level line the prose must open with, assembled from this same metadata
  want_level = "**" got["severity"] " · " got["class"]
  if (cur == "Cuts" && ("lines_saved" in got)) want_level = want_level " · −" got["lines_saved"] " lines"
  want_level = want_level "** ("
  n = split(got["lenses"], LL, "+"); sep = ""
  for (i = 1; i <= n; i++) { want_level = want_level sep LL[i]; sep = " + " }
  want_level = want_level ")"
  next
}

# first non-blank line after a finding metadata comment: the level line
pending_level && $0 != "" {
  if ($0 != want_level) err(NR, "the level line must be exactly `" want_level "` (assembled from the metadata)")
  pending_level = 0
  next
}

/^\*\*[A-Z]/ {
  lab = $0; sub(/:\*\*.*/, ":**", lab)
  if (cur == "Findings" || cur == "Cuts") {
    if (lab != "**Trigger:**" && lab != "**Fix:**")
      err(NR, "label `" lab "` does not exist in the format — only `**Trigger:**` and `**Fix:**`")
    if (lab == "**Fix:**") pending_fix = 0
  } else if (cur == "Needs human verification") {
    if (lab != "**To settle:**") err(NR, "in `## Needs human verification` the only label is `**To settle:**`")
  } else err(NR, "bold label outside a finding: `" lab "`")
  next
}

/^- / {
  if (cur == "Needs human verification") { has_item[cur] = 1
    if ($0 !~ /^- `[^`]+:[0-9]+` — /) err(NR, "bullet must start with `` `file:line` — ``") }
  next
}

/<!-- finding / && cur != "Findings" && cur != "Cuts" { err(NR, "`<!-- finding … -->` only lives in Findings and Cuts") }
/<!-- (trust-boundaries|out-of-scope|checked-ok) -->/ { err(NR, "marker for a section that does not exist in the format") }

END {
  close_finding()
  for (s in seen) if (!has_item[s]) err(0, "section `" s "` is empty — an empty section leaves the file")
  if (HDR["verdict"] == "approve" && ("Needs human verification" in seen))
    err(0, "`verdict: approve` with an open human-verification item")
  if (blocker && HDR["verdict"] != "request_changes")
    err(0, "a critical or major finding needs `verdict: request_changes`")
  if (!blocker && HDR["verdict"] == "request_changes")
    err(0, "`verdict: request_changes` needs a critical or major finding")
  if (HDR["verdict"] != "approve" && nfind == 0 && !("Needs human verification" in seen))
    err(0, "verdict `" HDR["verdict"] "` with no finding and no human-verification item")
  if (bad) { printf "\n%d problem(s) — %s\n", bad, FILE; exit 1 }
}
' "$1"
