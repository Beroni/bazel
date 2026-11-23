# Who wrote something, as one key per account. A bot is `app/<name>` in what
# `gh pr view` says, `<name>[bot]` in the REST API and `<name>` in GraphQL, and
# logins ignore case. The kind of account is part of the key: `publisher[bot]` and
# a person called `publisher` are two accounts, and one must never stand for the
# other. The name alone is the last resort, when nothing says which kind it is.

def stem: ascii_downcase | sub("^app/"; "") | sub("\\[bot\\]$"; "");

# $login as given, and whatever the source says of its kind: a REST user's `type`,
# a GraphQL `__typename`, gh's `is_bot`. Spelling counts too: no person's login
# starts with app/ or ends in [bot].
def account($login; $kind):
  ($login // "") as $l
  | if $l == "" then ""
    elif (($kind // "") | tostring | ascii_downcase) as $k | ($k == "bot" or $k == "true")
      or ($l | test("^app/|\\[bot\\]$"; "i")) then "bot:" + ($l | stem)
    else "user:" + ($l | stem) end;
