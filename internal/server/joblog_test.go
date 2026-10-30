package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beroni/bazel/internal/agent"
)

// With the option on, the file has every line whole, past both limits of the
// window on screen, and the job says where it is.
func TestJobLogNoDisco(t *testing.T) {
	t.Setenv("BAZEL_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "logs")
	lines := maxLogLines + 50
	long := strings.Repeat("x", maxLogLineRunes+100)
	cfg := cfgFor(t, "sh", "-c", fmt.Sprintf(`cat >/dev/null; i=0; while [ $i -lt %d ]; do echo "line $i"; i=$((i+1)); done; echo %s; echo oops >&2`, lines, long))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := New(ctx, cfg, "beroni", Options{Addr: "127.0.0.1:0", Concurrency: 1, LogDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ch := srv.jobs.hub.Subscribe()
	defer srv.jobs.hub.Unsubscribe(ch)
	view, err := srv.jobs.Enqueue(testPR(482), false, cfg.DefaultChoice())
	if err != nil {
		t.Fatal(err)
	}
	done := waitFor(t, ch, view.ID, StateDone)
	if want := filepath.Join(dir, view.ID+"-acme-api-core-482.log"); done.LogFile != want {
		t.Fatalf("log file %q, want %q", done.LogFile, want)
	}
	data, err := os.ReadFile(done.LogFile)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"] line 0\n", fmt.Sprintf("] line %d\n", lines-1), "] " + long + "\n", " stderr] oops\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %.40q", want)
		}
	}
	info, err := os.Stat(done.LogFile)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode %o: it holds whatever the agent wrote", perm)
	}
}

// Off by default: no file, no path in the job, no folder.
func TestJobLogDesligado(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BAZEL_HOME", home)
	cfg := cfgFor(t, "sh", "-c", "cat >/dev/null; echo report")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := New(ctx, cfg, "beroni", Options{Addr: "127.0.0.1:0", Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	ch := srv.jobs.hub.Subscribe()
	defer srv.jobs.hub.Unsubscribe(ch)
	view, err := srv.jobs.Enqueue(testPR(482), false, cfg.DefaultChoice())
	if err != nil {
		t.Fatal(err)
	}
	if done := waitFor(t, ch, view.ID, StateDone); done.LogFile != "" {
		t.Errorf("log file %q with the option off", done.LogFile)
	}
	if _, err := os.Stat(filepath.Join(home, "logs")); !os.IsNotExist(err) {
		t.Errorf("logs folder: %v", err)
	}
}

// A folder that cannot be used costs the logs, not the server.
func TestJobLogPastaInvalida(t *testing.T) {
	t.Setenv("BAZEL_HOME", t.TempDir())
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := New(ctx, cfgFor(t, "echo"), "beroni", Options{Addr: "127.0.0.1:0", Concurrency: 1, LogDir: file})
	if err != nil {
		t.Fatal(err)
	}
	if srv.jobs.logDir != "" {
		t.Errorf("logDir = %q", srv.jobs.logDir)
	}
}

// The file going away mid-job, or a closed log, does not disturb anything.
func TestJobLogNilESemArquivo(t *testing.T) {
	var none *jobLog
	none.write(agent.Event{Kind: agent.EventLog, Text: "x"})
	none.close()
	if l, path := openJobLog("", "ab12-j1", "a/b#1"); l != nil || path != "" {
		t.Errorf("no dir: %v %q", l, path)
	}
	if l, path := openJobLog(filepath.Join(t.TempDir(), "missing"), "ab12-j1", "a/b#1"); l != nil || path != "" {
		t.Errorf("missing dir: %v %q", l, path)
	}
	dir := t.TempDir()
	l, path := openJobLog(dir, "ab12-j1", "../../evil#1")
	if filepath.Dir(path) != dir {
		t.Errorf("path left the folder: %q", path)
	}
	if !logNameRe.MatchString(filepath.Base(path)) {
		t.Errorf("a PR key must not shape the path: %q", path)
	}
	l.close()
	l.write(agent.Event{Kind: agent.EventLog, Text: "after close"})
}

// Retention keeps the newest files and leaves everything else in the folder.
func TestPruneJobLogs(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i := range 5 {
		path := filepath.Join(dir, fmt.Sprintf("ab12-j%d-acme-api-1.log", i))
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		at := now.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	for _, other := range []string{"notes.log", "ab12-j1.txt", "server.log"} {
		if err := os.WriteFile(filepath.Join(dir, other), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "ab12-j9-dir.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := pruneJobLogs(dir, 2); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	want := "ab12-j1.txt ab12-j3-acme-api-1.log ab12-j4-acme-api-1.log ab12-j9-dir.log notes.log server.log"
	if got := strings.Join(left, " "); got != want {
		t.Errorf("left %q, want %q", got, want)
	}
}
