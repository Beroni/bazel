# Reads a unified diff (jq -Rs) into the lines GitHub accepts a comment on:
# {"path": {"RIGHT": {"88": true}, "LEFT": {...}}}. A path git quoted because of
# its bytes matches nothing here, and its findings go to the review body.

def mark($side; $n): .out[.file][$side][$n | tostring] = true;

reduce (split("\n")[]) as $l ({file: null, old: null, o: 0, n: 0, ol: 0, nl: 0, out: {}};
  if .ol > 0 or .nl > 0 then
    ($l[0:1]) as $c
    | if $c == "+" then (if .file then mark("RIGHT"; .n) else . end) | .n += 1 | .nl -= 1
      elif $c == "-" then (if .file then mark("LEFT"; .o) else . end) | .o += 1 | .ol -= 1
      elif $c == "\\" then .
      else (if .file then mark("RIGHT"; .n) | mark("LEFT"; .o) else . end)
        | .n += 1 | .o += 1 | .nl -= 1 | .ol -= 1 end
  elif ($l | startswith("--- a/")) then .old = ($l | ltrimstr("--- a/") | rtrimstr("\t"))
  elif ($l | startswith("--- ")) then .old = null
  elif ($l | startswith("+++ b/")) then .file = ($l | ltrimstr("+++ b/") | rtrimstr("\t"))
  elif ($l | startswith("+++ ")) then .file = .old
  elif ($l | startswith("@@ ")) then
    ($l | capture("^@@ -(?<o>[0-9]+)(,(?<ol>[0-9]+))? \\+(?<n>[0-9]+)(,(?<nl>[0-9]+))? @@")) as $h
    | .o = ($h.o | tonumber) | .n = ($h.n | tonumber)
    | .ol = (($h.ol // "1") | tonumber) | .nl = (($h.nl // "1") | tonumber)
  else . end)
| .out
