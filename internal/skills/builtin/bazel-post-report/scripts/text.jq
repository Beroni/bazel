# Text from the report on its way into a body the script assembles. On screen each
# part stands alone; on the PR they are one document, and an unclosed `<!--` or
# fence in one would hide every part after it.

def safe: gsub("<!--"; "&lt;!--");

# A blockquote ends where it ends: nothing opened inside it reaches the next part.
def quoted: safe | split("\n") | map("> " + .) | join("\n");
