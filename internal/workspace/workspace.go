// Package workspace prepara um clone descartável do repositório, com o pull
// request já em checkout, para o agente rodar dentro dele.
package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/beroni/bazel/internal/gh"
)

// Workspace é um clone temporário com o PR em checkout.
type Workspace struct {
	// Dir é a raiz do clone.
	Dir string
	// Keep preserva o diretório no Cleanup, para inspeção manual.
	Keep bool
}

// Prepare clona o repositório do PR numa pasta temporária e faz o checkout da
// branch do PR. O clone é blobless (--filter=blob:none) em vez de raso: baixa
// pouco, mas mantém o histórico inteiro — sem ele o `git diff base...HEAD` do
// agente não acha o merge-base.
func Prepare(ctx context.Context, pr gh.PR) (*Workspace, error) {
	dir, err := os.MkdirTemp("", fmt.Sprintf("bazel-%s-%d-", sanitize(pr.Slug()), pr.Number))
	if err != nil {
		return nil, fmt.Errorf("criando pasta temporária: %w", err)
	}
	ws := &Workspace{Dir: dir}

	// git clone aceita um diretório existente desde que esteja vazio.
	if err := run(ctx, "", "gh", "repo", "clone", pr.Repo, dir, "--", "--filter=blob:none"); err != nil {
		ws.Remove()
		return nil, fmt.Errorf("clonando %s: %w", pr.Repo, err)
	}
	// `gh pr checkout` resolve sozinho PR vindo de fork.
	if err := run(ctx, dir, "gh", "pr", "checkout", strconv.Itoa(pr.Number)); err != nil {
		ws.Remove()
		return nil, fmt.Errorf("checkout de %s: %w", pr.Key(), err)
	}
	return ws, nil
}

// Cleanup apaga o clone, a menos que Keep esteja ligado.
func (w *Workspace) Cleanup() {
	if w == nil || w.Keep {
		return
	}
	w.Remove()
}

// Remove apaga o clone incondicionalmente.
func (w *Workspace) Remove() {
	if w == nil || w.Dir == "" {
		return
	}
	os.RemoveAll(w.Dir)
}

func run(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s", lastLines(msg, 3))
		}
		return err
	}
	return nil
}

// lastLines corta a saída de erro do git/gh, que costuma vir com barra de
// progresso antes da mensagem que interessa.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// sanitize deixa só o que pode virar prefixo de diretório temporário.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
}
