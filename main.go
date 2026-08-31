// Command bazel sobe a interface web que junta os pull requests dos
// repositórios monitorados e dispara agentes de IA para revisá-los.
//
// Não há subcomando: o binário é o servidor. Repositórios, agentes e reviews
// se gerenciam na página.
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

// version é sobrescrita no build via -ldflags "-X main.version=...".
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

// serve sobe o servidor e só volta quando o ctx morre.
//
// Mesma máquina de sempre — o `gh` autenticado, o mesmo ~/.bazel/config.yaml —
// com HTTP na frente. Escuta em loopback porque quem chega aqui manda clonar
// repositório e rodar agente com shell: não é porta para expor.
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
	// A lista de agentes começa vazia: é montada na página, a partir das
	// skills que o Claude Code tem instaladas nesta máquina.
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
	// Abrir o navegador é conveniência: se falhar, a URL já está impressa.
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
