# The payload again, with every inline comment moved to the body: GitHub refused
# the review over a line, and a finding a person approved still reaches the PR.

.body as $b
| .body = ($b + "\n\n### Findings GitHub refused to anchor\n\n"
    + (.comments | map("#### `\(.path | gsub("`"; "'"))`:\(.line)\n\(.body)") | join("\n\n")))
| .comments = []
