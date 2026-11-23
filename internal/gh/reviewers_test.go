package gh

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// One PR the way GitHub answers it: requests pending (people, a team, a bot),
// then latest reviews (one dismissed, one pending, the author's own, a bot's
// and someone also asked).
const reviewersAnswer = `{"data":{
 "p0":{"pullRequest":{
  "author":{"login":"maria"},
  "reviewRequests":{"nodes":[
   {"requestedReviewer":{"__typename":"User","login":"fbsobreira"}},
   {"requestedReviewer":{"__typename":"Team","slug":"core","organization":{"login":"klever-io"}}},
   {"requestedReviewer":{"__typename":"Bot","login":"copilot"}},
   {"requestedReviewer":{"__typename":"User","login":"Me"}},
   {"requestedReviewer":null}]},
  "latestReviews":{"nodes":[
   {"state":"APPROVED","author":{"__typename":"User","login":"RomuloSiebra"}},
   {"state":"COMMENTED","author":{"__typename":"User","login":"FBSobreira"}},
   {"state":"DISMISSED","author":{"__typename":"User","login":"gone"}},
   {"state":"PENDING","author":{"__typename":"User","login":"drafting"}},
   {"state":"COMMENTED","author":{"__typename":"User","login":"maria"}},
   {"state":"COMMENTED","author":{"__typename":"Bot","login":"coderabbitai"}},
   {"state":"COMMENTED","author":{"__typename":"User","login":"renovate[bot]"}},
   {"state":"CHANGES_REQUESTED","author":{"__typename":"User","login":"lucas"}}]}}},
 "p1":{"pullRequest":null}},
 "errors":[{"message":"Could not resolve to a PullRequest with the number of 99."}]}`

func TestReviewersFiltraEOrdena(t *testing.T) {
	got, err := parseReviewers(reviewersAnswer, []string{"klever-io/klever-go#206", "o/r#99"}, "me")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"fbsobreira", "klever-io/core", "RomuloSiebra", "lucas"}
	if !reflect.DeepEqual(got["klever-io/klever-go#206"], want) {
		t.Errorf("reviewers = %q, want %q", got["klever-io/klever-go#206"], want)
	}
	if r, ok := got["o/r#99"]; !ok || len(r) != 0 {
		t.Errorf("a PR GitHub did not answer for: %q %v", r, ok)
	}
}

// The PRs go in as variables, in one call, and gh failing is an error.
func TestFetchReviewersEmLote(t *testing.T) {
	answer := filepath.Join(t.TempDir(), "a.json")
	if err := os.WriteFile(answer, []byte(reviewersAnswer), 0o600); err != nil {
		t.Fatal(err)
	}
	log := fakeGH(t, "cat "+answer)
	got, err := FetchReviewers(context.Background(), []string{"klever-io/klever-go#206", "o/r#99", "not a key", "o/r#x"}, "me")
	if err != nil {
		t.Fatal(err)
	}
	if len(got["klever-io/klever-go#206"]) != 4 {
		t.Errorf("%v", got)
	}
	data, _ := os.ReadFile(log)
	call := string(data)
	if n := strings.Count(call, "api graphql"); n != 1 {
		t.Fatalf("%d gh calls: %s", n, call)
	}
	for _, a := range []string{"query bazelReviewers(", "-f o0=klever-io", "-f n0=klever-go", "-F p0=206", "-f o1=o", "-f n1=r", "-F p1=99"} {
		if !strings.Contains(call, a) {
			t.Errorf("missing %q in %s", a, call)
		}
	}
	if strings.Contains(call, "not a key") || strings.Contains(call, "p2=") {
		t.Errorf("a bad key went to gh: %s", call)
	}

	fakeGH(t, "echo 'HTTP 502: Bad Gateway' >&2; exit 1")
	if _, err := FetchReviewers(context.Background(), []string{"o/r#1"}, "me"); err == nil {
		t.Error("gh failed and FetchReviewers said nothing")
	}
}

func TestReviewersQueryUsaSoVariaveis(t *testing.T) {
	q := reviewersQuery(2)
	for _, want := range []string{"$o0: String!", "$p1: Int!", "p1: repository(owner: $o1, name: $n1)", "fragment bazelReviewersPR"} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %q:\n%s", want, q)
		}
	}
}
