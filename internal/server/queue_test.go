package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beroni/bazel/internal/config"
)

// queueServer is a server keeping its queue in home. It is down before the test
// ends: a worker still saving the queue leaves a temporary file in home, and the
// test's own cleanup then fails to remove the folder.
func queueServer(ctx context.Context, t *testing.T, cfg *config.Config, home string) *Server {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	srv, err := New(ctx, cfg, "beroni", Options{Addr: "127.0.0.1:0", Concurrency: 1, QueueDir: home})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		srv.jobs.Wait(shutdownWait)
	})
	return srv
}

func queueStates(srv *Server) map[string]State {
	out := map[string]State{}
	for _, v := range srv.jobViews() {
		out[v.PR.Key] = v.State
	}
	return out
}

// What was pending comes back paused, running included, and nothing runs until
// a click: the agent here would write a file the moment it starts.
func TestFilaVoltaPausada(t *testing.T) {
	t.Setenv("BAZEL_HOME", t.TempDir())
	home := t.TempDir()
	mark := filepath.Join(t.TempDir(), "ran")
	cfg := cfgFor(t, "sh", "-c", "touch "+mark+"; cat >/dev/null; sleep 30")
	cfg.Agent.Prompt = "review {{number}}: do not publish anything to GitHub"
	ctx, stop := context.WithCancel(context.Background())
	srv := queueServer(ctx, t, cfg, home)
	ch := srv.jobs.hub.Subscribe()
	first, err := srv.jobs.Enqueue(testPR(1), false, cfg.DefaultChoice())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.jobs.Enqueue(testPR(2), false, cfg.DefaultChoice()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, first.ID, StateRunning)
	data, err := os.ReadFile(filepath.Join(home, queueFileName))
	if err != nil || strings.Count(string(data), `"agent"`) != 2 {
		t.Fatalf("queue file: %s %v", data, err)
	}
	stop()
	srv.jobs.Wait(shutdownWait)
	os.Remove(mark)

	ctx2, stop2 := context.WithCancel(context.Background())
	t.Cleanup(stop2)
	again := queueServer(ctx2, t, cfg, home)
	got := queueStates(again)
	if got["acme/api-core#1"] != StatePaused || got["acme/api-core#2"] != StatePaused {
		t.Fatalf("after the restart: %v", got)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("an agent ran with nobody asking")
	}

	// The click, through the API, runs that job and leaves the other waiting.
	var id string
	for _, v := range again.jobViews() {
		if v.PR.Number == 1 {
			id = v.ID
		}
	}
	w := httptest.NewRecorder()
	again.Handler().ServeHTTP(w, httptest.NewRequest("POST", "http://127.0.0.1:7777/api/jobs/"+id+"/resume", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("resume: %d %s", w.Code, w.Body)
	}
	if got := queueStates(again); got["acme/api-core#2"] != StatePaused || got["acme/api-core#1"] == StatePaused {
		t.Errorf("after one click: %v", got)
	}
	w = httptest.NewRecorder()
	again.Handler().ServeHTTP(w, httptest.NewRequest("POST", "http://127.0.0.1:7777/api/jobs/"+id+"/resume", nil))
	if w.Code != http.StatusConflict {
		t.Errorf("resume twice: %d", w.Code)
	}
}

// A publication never comes back, and neither does an agent that publishes on
// its own once it has started. One that has not started can wait for its click.
func TestPublicacaoNuncaVolta(t *testing.T) {
	m := NewManager(context.Background(), cfgFor(t, "echo"), t.TempDir(), 1, false, NewHub())
	poeJob(m, &Job{ID: "a", State: StateRunning, PR: testPR(1), publish: &publishInput{Path: "x"}})
	poeJob(m, &Job{ID: "b", State: StateQueued, PR: testPR(2), publish: &publishInput{Path: "y"}})
	declared := config.Choice{Name: "post", Posts: true, Steps: []config.ResolvedAgent{{Name: "post", Posts: true}}}
	poeJob(m, &Job{ID: "c", State: StateRunning, PR: testPR(3), Choice: declared})
	poeJob(m, &Job{ID: "d", State: StateQueued, PR: testPR(4), Choice: declared})
	poeJob(m, &Job{ID: "e", State: StateDone, PR: testPR(5)})
	// `--post` in the task with no `posts: true`: nothing declared, and it publishes.
	post := config.Choice{Name: "lens", Steps: []config.ResolvedAgent{{Name: "lens", Task: "/review {{number}} --post"}}}
	poeJob(m, &Job{ID: "f", State: StateRunning, PR: testPR(6), Choice: post})
	m.mu.Lock()
	pending := m.pendingLocked()
	m.mu.Unlock()
	if len(pending) != 1 || pending[0].PR.Number != 4 {
		t.Errorf("pending: %+v", pending)
	}
}

// The agent that may publish comes back saying so, for the click to ask first;
// one that does not comes back with nothing to ask.
func TestPausadoDizQuePublica(t *testing.T) {
	post := config.Choice{Name: "lens", Steps: []config.ResolvedAgent{{Name: "lens", Task: "/review {{number}} --post"}}}
	if v := (&Job{ID: "a", State: StatePaused, PR: testPR(1), Choice: post}).view(); !strings.Contains(v.Publishes, "--post") {
		t.Errorf("view: %+v", v)
	}
	if v := (&Job{ID: "b", State: StateQueued, PR: testPR(1), Choice: post}).view(); v.Publishes != "" {
		t.Errorf("only a paused job asks: %+v", v)
	}
}

// Cancel takes a paused job off the queue instead of calling it finished.
func TestCancelaPausado(t *testing.T) {
	m := NewManager(context.Background(), cfgFor(t, "echo"), t.TempDir(), 1, false, NewHub())
	poeJob(m, &Job{ID: "a", State: StatePaused, PR: testPR(1)})
	if err := m.Cancel("a"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jobs["a"].State != StateCanceled || len(m.pendingLocked()) != 0 {
		t.Errorf("state %s", m.jobs["a"].State)
	}
}

// Finishing, cancelling and removing take a job out of the file.
func TestFilaSaiDoArquivo(t *testing.T) {
	t.Setenv("BAZEL_HOME", t.TempDir())
	home := t.TempDir()
	cfg := cfgFor(t, "sh", "-c", "cat >/dev/null; echo report")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := queueServer(ctx, t, cfg, home)
	ch := srv.jobs.hub.Subscribe()
	view, err := srv.jobs.Enqueue(testPR(1), false, cfg.DefaultChoice())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, view.ID, StateDone)
	srv.jobs.saveQueue()
	var saved struct {
		Jobs []queuedJob `json:"jobs"`
	}
	data, err := os.ReadFile(filepath.Join(home, queueFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &saved); err != nil || len(saved.Jobs) != 0 {
		t.Errorf("file: %s %v", data, err)
	}
}

// A file that cannot be read steps aside, an agent that left the config drops
// out, and asking for a paused review is the click.
func TestFilaArquivoRuim(t *testing.T) {
	t.Setenv("BAZEL_HOME", t.TempDir())
	home := t.TempDir()
	path := filepath.Join(home, queueFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := cfgFor(t, "sh", "-c", "cat >/dev/null")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := queueServer(ctx, t, cfg, home)
	if len(srv.jobViews()) != 0 {
		t.Errorf("jobs: %v", srv.jobViews())
	}
	if aside, _ := filepath.Glob(path + ".corrupt-*"); len(aside) != 1 {
		t.Errorf("aside: %v", aside)
	}

	name := cfg.DefaultChoice().Name
	file, _ := json.Marshal(map[string]any{"jobs": []queuedJob{{Repo: testPR(1).Repo, PR: testPR(1), Agent: name}, {Repo: testPR(2).Repo, PR: testPR(2), Agent: "gone"}}})
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	t.Cleanup(cancel2)
	again := queueServer(ctx2, t, cfg, home)
	got := queueStates(again)
	if len(got) != 1 || got["acme/api-core#1"] != StatePaused {
		t.Fatalf("restored: %v", got)
	}
	view, err := again.jobs.Enqueue(testPR(1), false, cfg.DefaultChoice())
	if err != nil {
		t.Fatal(err)
	}
	if view.State == StatePaused || len(again.jobViews()) != 1 {
		t.Errorf("asking again: %+v", view)
	}
}
