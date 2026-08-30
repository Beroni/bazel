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
	fmt.Print(`bazel — pull requests do time, revisados por agentes de IA

USO
  bazel                     sobe a interface web em 127.0.0.1:7777
  bazel --open              e já abre o navegador

FLAGS
  --addr <host:porta>  endereço de escuta (padrão 127.0.0.1:7777)
  --jobs <n>           reviews simultâneos (padrão 2)
  --open               abre o navegador
  --keep               não apaga os clones temporários dos PRs
  --no-splash          pula a animação de abertura
  --version            versão

AMBIENTE
  BAZEL_HOME        diretório de configuração (padrão: ~/.bazel)
  BAZEL_NO_SPLASH   desliga a animação

Repositórios, agentes e reviews se gerenciam na própria página.
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
			return fmt.Errorf("--jobs precisa ser um número maior que zero, não %q", v)
		}
		jobs = n
	}

	me, err := gh.CurrentUser(ctx)
	if err != nil {
		return fmt.Errorf("não consegui identificar seu usuário no GitHub: %w", err)
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
	fmt.Println(styBold.Render("  BAZEL") + styDim.Render("  ·  interface web"))
	fmt.Println("  " + styOK.Render(url))
	fmt.Println(styDim.Render(fmt.Sprintf("  @%s · %d repo(s) · %d review(s) em paralelo · ctrl-c para parar",
		me, len(cfg.Repos), jobs)))
	if criado {
		path, _ := config.Path()
		fmt.Println(styDim.Render("  configuração criada em " + path))
	}
	if len(cfg.Repos) == 0 {
		fmt.Println(styDim.Render("  nenhum repositório monitorado — adicione um em \"config\", na página"))
	}
	if hasFlag(args, "--open") {
		openBrowser(url)
	}

	if err := srv.Run(ctx); err != nil {
		return err
	}
	fmt.Println(styDim.Render("  servidor parado"))
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
