// Command bazel starts the web interface that gathers the pull requests of
// the watched repositories and dispatches AI agents to review them.
//
// There is no subcommand: the binary is the server. Repositories, agents and
// reviews are managed on the page.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/charmbracelet/lipgloss"

	"github.com/beroni/bazel/internal/config"
	"github.com/beroni/bazel/internal/gh"
	"github.com/beroni/bazel/internal/server"
	"github.com/beroni/bazel/internal/splash"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "0.1.0"

var (
	styErr  = lipgloss.NewStyle().Foreground(lipgloss.Color("#ef4444"))
	styOK   = lipgloss.NewStyle().Foreground(lipgloss.Color("#22c55e"))
	styDim  = lipgloss.NewStyle().Foreground(lipgloss.Color("#6b7280"))
	styBold = lipgloss.NewStyle().Foreground(lipgloss.Color("#f97316")).Bold(true)
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	args := os.Args[1:]
	args = stripFlag(args, "--no-splash", func() { os.Setenv("BAZEL_NO_SPLASH", "1") })

	switch {
	case hasFlag(args, "--help"), hasFlag(args, "-h"):
		usage()
		return
	case hasFlag(args, "--version"), hasFlag(args, "-v"):
		fmt.Println("bazel", version)
		return
	}

	if err := serve(ctx, args); err != nil {
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, styErr.Render("✗ "+err.Error()))
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`bazel — your team's pull requests, reviewed by AI agents

USAGE
  bazel                     serves the web UI on 127.0.0.1:7777
  bazel --open              and opens the browser

FLAGS
  --addr <host:port>   listen address (default 127.0.0.1:7777)
  --jobs <n>           concurrent reviews (default 2)
  --open               open the browser
  --keep               keep the throwaway PR clones
  --no-splash          skip the opening animation
  --version            version

ENVIRONMENT
  BAZEL_HOME        configuration directory (default: ~/.bazel)
  BAZEL_NO_SPLASH   turns the animation off

Repositories, agents and reviews are all managed from the page.
`)
}

// serve starts the server and only returns when ctx dies.
//
// Same machinery as always — the authenticated `gh`, the same
// ~/.bazel/config.yaml — with HTTP in front. Listens on loopback because
// whoever reaches it can order a repository cloned and an agent run with a
// shell: this is not a port to expose.
func serve(ctx context.Context, args []string) error {
	cfg, criado, err := config.LoadOrInit()
	if err != nil {
		return err
	}
	if err := gh.CheckAuth(ctx); err != nil {
		return err
	}

	addr := flagValue(args, "--addr")
	if addr == "" {
		addr = "127.0.0.1:7777"
	}
	jobs := 2
	if v := flagValue(args, "--jobs"); v != "" {
		n, convErr := strconv.Atoi(v)
		if convErr != nil || n < 1 {
			return fmt.Errorf("--jobs needs a number greater than zero, not %q", v)
		}
		jobs = n
	}

	me, err := gh.CurrentUser(ctx)
	if err != nil {
		return fmt.Errorf("could not identify your GitHub user: %w", err)
	}

	srv, err := server.New(ctx, cfg, me, server.Options{
		Addr:        addr,
		Concurrency: jobs,
		Keep:        hasFlag(args, "--keep"),
		Version:     version,
	})
	if err != nil {
		return err
	}

	splash.Play()

	// Beyond loopback there is no authentication at all: whoever reaches the
	// port clones repositories and runs agents with your `gh`. It is not
	// forbidden — some put an authenticating proxy in front —, but it cannot
	// be silent.
	if srv.Exposed() {
		fmt.Println(styErr.Render(fmt.Sprintf("  ⚠ listening on %s, beyond loopback, with no authentication —", addr)))
		fmt.Println(styErr.Render("    anyone who reaches this port can clone repos and run agents as you."))
		fmt.Println(styErr.Render("    Put an authenticating proxy in front, or use --addr 127.0.0.1:7777."))
	}

	url := srv.URL(addr)
	fmt.Println(styBold.Render("  BAZEL") + styDim.Render("  ·  web interface"))
	fmt.Println("  " + styOK.Render(url))
	fmt.Println(styDim.Render(fmt.Sprintf("  @%s · %d repo(s) · %d review(s) in parallel · ctrl-c to stop",
		me, len(cfg.Repos), jobs)))
	if criado {
		path, _ := config.Path()
		fmt.Println(styDim.Render("  configuration created at " + path))
	}
	if len(cfg.Repos) == 0 {
		fmt.Println(styDim.Render("  no repositories watched — add one under \"config\", in the page"))
	}
	// The agent list starts empty: it is built on the page, from the skills
	// Claude Code has installed on this machine.
	if len(cfg.Choices()) == 0 {
		fmt.Println(styDim.Render("  no agents configured — build the list under \"config\", out of your skills"))
	}
	if hasFlag(args, "--open") {
		openBrowser(url)
	}

	if err := srv.Run(ctx); err != nil {
		return err
	}
	fmt.Println(styDim.Render("  server stopped"))
	return nil
}

func openBrowser(url string) {
	var cmd string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd = "explorer"
	default:
		cmd = "xdg-open"
	}
	// Opening the browser is a convenience: if it fails, the URL is already
	// printed.
	_ = exec.Command(cmd, url).Start()
}

// --- flags ---

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
	}
	return ""
}

func stripFlag(args []string, name string, onFound func()) []string {
	out := args[:0]
	for _, a := range args {
		if a == name {
			onFound()
			continue
		}
		out = append(out, a)
	}
	return out
}
