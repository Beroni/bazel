# Text from the report on its way into a body the script assembles. On screen each
# part stands alone; on the PR they are one document, so no part may open anything
# the next one ends up inside: a comment, a <details>, a fence, or the end of
# whatever holds it.

# A line of markdown the script writes, with report text in it. It stays markdown,
# but no raw HTML: `<` is the only way into that.
def safe: gsub("<"; "&lt;");

# A whole text from the report, as it is. Nothing inside a code block is parsed, no
# tag, mention or image, and a fence longer than any run of backticks in the text
# cannot be closed from inside it. Escaping instead would show `Vec&lt;u8>` in code.
def verbatim:
  (([scan("`+") | length] | max // 0) + 1 | if . < 3 then 3 else . end) as $n
  | ("`" * $n) as $fence
  | "\($fence)text\n\(.)\n\($fence)";
