# One account, however GitHub spells it. A bot is `app/<name>` in what `gh pr view`
# says, `<name>[bot]` in the REST API and `<name>` in GraphQL; logins ignore case.
# Every comparison of who wrote something goes through this, or the PR's own bot
# would not be the PR's author.
def login: ascii_downcase | sub("^app/"; "") | sub("\\[bot\\]$"; "");
