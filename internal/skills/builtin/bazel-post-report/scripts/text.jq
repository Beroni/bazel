# Text from the report on its way into a body the script assembles. On screen each
# part stands alone; on the PR they are one document, so no part may open anything
# the next one ends up inside: a comment, a <details>, a fence, or the end of
# whatever holds it.

# A line of markdown the script writes, with report text in it. It stays markdown,
# but no raw HTML: `<` is the only way into that.
def safe: gsub("&"; "&amp;") | gsub("<"; "&lt;") | gsub("@(?<c>[A-Za-z0-9_])"; "@\u200b\(.c)") | gsub("#(?<d>[0-9])"; "#\u200b\(.d)");

# A whole text from the report, as it is. Nothing inside a code block is parsed, no
# tag, mention or image, and a fence longer than any run of backticks in the text
# cannot be closed from inside it. Escaping instead would show `Vec&lt;u8>` in code.
def verbatim:
  (([scan("`+") | length] | max // 0) + 1 | if . < 3 then 3 else . end) as $n
  | ("`" * $n) as $fence
  | "\($fence)text\n\(.)\n\($fence)";

# Text outside a code block, made inert: no raw HTML and no entity, since every
# `&` and `<` is escaped, code spans included (`&amp;` and `&lt;` read as such there:
# ugly, never live); and no mention or reference, since a zero-width space after
# `@` or `#` before a digit keeps GitHub from notifying a person or a team, or
# linking an issue, in the user's name. This does not try to tell code from prose:
# when unsure, escape.
def inert: gsub("&"; "&amp;") | gsub("<"; "&lt;") | gsub("@(?<c>[A-Za-z0-9_])"; "@​\(.c)") | gsub("#(?<d>[0-9])"; "#​\(.d)");

# A fence this script lets stand: at the very start of the line, three or more
# backticks with no backtick after them, or three or more tildes. Markdown reads each
# of those as a fence at the top level, whatever came before; indented, a fence may
# belong to a list item that the next line ends, and its code with it. So any other
# line that looks like a fence loses its first marker to a backslash and is a fence
# for nobody, and what would have been its code is text, escaped like the rest.
def strict_fence: capture("^(?<pad>)(?<run>`{3,}(?=[^`]*$)|~{3,})");
def loose_fence: test("^[ \t]*(`{3,}|~{3,})");

# A text from the report, rendered as the markdown it is — bold, lists, its own
# code blocks — with no way out of the part it is in. Inside a fence the text is
# code and stays as written; the fence is closed at the end of the part if it was
# not. It closes on the same marker, as long or longer, then only spaces or tabs:
# exactly where markdown closes it, so the two never disagree on what is code.
def readable:
  reduce (split("\n")[]) as $l ({out: [], fence: null};
    ([$l | strict_fence] | first) as $open
    | if .fence then
        ("^ {0,3}" + (if .fence.run[0:1] == "`" then "`" else "~" end)
         + "{" + (.fence.run | length | tostring) + ",}[ \t]*$") as $close
        | .out += [$l]
        | if ($l | test($close)) then .fence = null else . end
      elif $open then .out += [$l] | .fence = $open
      elif ($l | loose_fence) then .out += [$l | sub("^(?<p>[ \t]*)"; "\(.p)\\") | inert]
      # A label the report sets at the start of a line — `**Trigger:**`, `**Fix:**` —
      # opens a paragraph of its own, whatever the line before it.
      elif ($l | test("^\\*\\*[A-Z][A-Za-z ]{0,30}:\\*\\*")) and (.out | length) > 0 and (.out[-1] | test("\\S")) then
        .out += ["", ($l | inert)]
      else .out += [$l | inert] end)
  | (.out | join("\n")) + (if .fence then "\n" + .fence.pad + .fence.run else "" end);

# A comment of its own reads the same way.
def comment_text: readable;
