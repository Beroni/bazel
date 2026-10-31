package workspace

import (
	"context"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beroni/bazel/internal/gh"
)

func TestPrepareFetchesThePRBaseBranch(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "commands.log")
	writeCommand(t, bin, "gh", `
printf 'gh %s\n' "$*" >> "$BAZEL_TEST_LOG"
`)
	writeCommand(t, bin, "git", `
printf 'git %s\n' "$*" >> "$BAZEL_TEST_LOG"
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BAZEL_TEST_LOG", log)

	ws, err := Prepare(context.Background(), gh.PR{
		Repo:        "acme/api",
		Number:      42,
		BaseRefName: "release/v1.2",
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer ws.Remove()

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	want := "git fetch origin +refs/heads/release/v1.2:refs/remotes/origin/release/v1.2"
	if !strings.Contains(got, want) {
		t.Errorf("base fetch missing:\n%s\nwant %s", got, want)
	}
	if want := "git update-ref refs/heads/release/v1.2 refs/remotes/origin/release/v1.2"; !strings.Contains(got, want) {
		t.Errorf("local base branch missing:\n%s\nwant %s", got, want)
	}
}

func TestPrepareSkipsEmptyBaseBranch(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "commands.log")
	writeCommand(t, bin, "gh", `printf 'gh %s\n' "$*" >> "$BAZEL_TEST_LOG"`)
	writeCommand(t, bin, "git", `printf 'git %s\n' "$*" >> "$BAZEL_TEST_LOG"`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BAZEL_TEST_LOG", log)

	ws, err := Prepare(context.Background(), gh.PR{Repo: "acme/api", Number: 42})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer ws.Remove()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "git fetch") {
		t.Errorf("empty base should not fetch:\n%s", data)
	}
}

func TestPrepareKeepsExistingBaseWhenFetchFails(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "commands.log")
	writeCommand(t, bin, "gh", `printf 'gh %s\n' "$*" >> "$BAZEL_TEST_LOG"`)
	writeCommand(t, bin, "git", `
printf 'git %s\n' "$*" >> "$BAZEL_TEST_LOG"
case " $* " in
  *" fetch "*) echo 'could not fetch' >&2; exit 1 ;;
  *" rev-parse --verify --quiet origin/release/v1.2 "*) exit 0 ;;
  *" rev-parse --abbrev-ref HEAD "*) printf 'feature\n'; exit 0 ;;
esac
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BAZEL_TEST_LOG", log)

	ws, err := Prepare(context.Background(), gh.PR{
		Repo:        "acme/api",
		Number:      42,
		BaseRefName: "release/v1.2",
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer ws.Remove()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "git update-ref refs/heads/release/v1.2 refs/remotes/origin/release/v1.2") {
		t.Errorf("should still create local base after a failed fetch:\n%s", got)
	}
}

func TestPrepareSkipsLocalBaseWhenHeadIsTheBase(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "commands.log")
	writeCommand(t, bin, "gh", `printf 'gh %s\n' "$*" >> "$BAZEL_TEST_LOG"`)
	writeCommand(t, bin, "git", `
printf 'git %s\n' "$*" >> "$BAZEL_TEST_LOG"
case " $* " in
  *" rev-parse --abbrev-ref HEAD "*) printf 'release/v1.2\n'; exit 0 ;;
esac
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BAZEL_TEST_LOG", log)

	ws, err := Prepare(context.Background(), gh.PR{
		Repo:        "acme/api",
		Number:      42,
		BaseRefName: "release/v1.2",
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer ws.Remove()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "git update-ref") {
		t.Errorf("should not force-create base when it is HEAD:\n%s", data)
	}
}

func writeCommand(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The clone and the checkout write the PR's files before Neutralize runs: a
// filter or LFS configured on the machine must not run over them.
func TestPrepareIgnoresTheMachinesGitConfig(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "commands.log")
	script := `
printf '%s global=%s nosystem=%s\n' "$0" "$GIT_CONFIG_GLOBAL" "$GIT_CONFIG_NOSYSTEM" >> "$BAZEL_TEST_LOG"
`
	writeCommand(t, bin, "gh", script)
	writeCommand(t, bin, "git", script)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BAZEL_TEST_LOG", log)

	ws, err := Prepare(context.Background(), gh.PR{Repo: "acme/api", Number: 42, BaseRefName: "main"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer ws.Remove()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected the clone, the checkout and the fetch: %q", lines)
	}
	want := "global=" + os.DevNull + " nosystem=1"
	for _, line := range lines {
		if !strings.HasSuffix(line, want) {
			t.Errorf("%q does not end in %q", line, want)
		}
	}
}

// A blobless clone fetches blobs from the checkout, after gh has handed over: the
// real thing, against a server that asks for a password, and a gh that gives its
// credentials only to its own git command, as the real one does.
func TestPrepareClonaRepoPrivadoBlobless(t *testing.T) {
	gitPath := func(args ...string) string {
		out, err := exec.Command("git", args...).Output()
		if err != nil {
			t.Skipf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(root, "init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(work, "add", "-A")
	git(work, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "commit", "-qm", "init")
	bare := filepath.Join(root, "repos", "acme", "api.git")
	git(root, "clone", "-q", "--bare", work, bare)
	git(bare, "config", "uploadpack.allowFilter", "true")
	git(bare, "config", "uploadpack.allowAnySHA1InWant", "true")

	backend := &cgi.Handler{
		Path: filepath.Join(gitPath("--exec-path"), "git-http-backend"),
		Env:  []string{"GIT_PROJECT_ROOT=" + filepath.Join(root, "repos"), "GIT_HTTP_EXPORT_ALL=1"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "x-access-token" || pass != "s3cret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	defer srv.Close()

	bin := t.TempDir()
	writeCommand(t, bin, "gh", `
case "$1 $2" in
"repo clone")
  exec git -c credential.helper= -c 'credential.helper=!gh auth git-credential' clone -q --no-checkout --filter=blob:none "$BAZEL_TEST_URL" "$4" ;;
"pr checkout")
  exec git checkout -q main ;;
"auth git-credential")
  cat >/dev/null
  printf 'username=x-access-token\npassword=s3cret\n' ;;
esac
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+".."+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BAZEL_TEST_URL", srv.URL+"/acme/api.git")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ws, err := Prepare(ctx, gh.PR{Repo: "acme/api", Number: 1, BaseRefName: "main"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer ws.Remove()
	if data, err := os.ReadFile(filepath.Join(ws.Dir, "main.go")); err != nil || !strings.Contains(string(data), "package main") {
		t.Errorf("the blob did not arrive: %q %v", data, err)
	}
}

// A relative PATH entry would resolve inside the clone.
func TestCloneEnvTiraPathRelativo(t *testing.T) {
	t.Setenv("PATH", strings.Join([]string{"/usr/bin", ".", "bin", "/bin"}, string(os.PathListSeparator)))
	for _, kv := range cloneEnv() {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok && v != strings.Join([]string{"/usr/bin", "/bin"}, string(os.PathListSeparator)) {
			t.Errorf("PATH = %q", v)
		}
	}
}

// A read-only folder an agent left does not leave the clone on disk.
func TestRemoveLevaPastaSoLeitura(t *testing.T) {
	dir := t.TempDir()
	locked := filepath.Join(dir, "clone", "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	ws := &Workspace{Dir: filepath.Join(dir, "clone")}
	ws.Remove()
	if _, err := os.Stat(ws.Dir); !os.IsNotExist(err) {
		t.Errorf("still there: %v", err)
	}
}
