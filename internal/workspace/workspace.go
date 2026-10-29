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
	"time"

	"github.com/beroni/bazel/internal/gh"
)

// Workspace é um clone temporário com o PR em checkout.
type Workspace struct {
	// Dir é a raiz do clone.
	Dir string
	// Keep preserva o diretório no Cleanup, para inspeção manual.
	Keep bool
}

// cloneTimeout limita o clone e o checkout. O timeout_seconds do agente só
// começa a contar no passo, depois do clone; sem este limite um `gh repo
// clone` pendurado na rede segura o worker até alguém cancelar na mão.
var cloneTimeout = 15 * time.Minute

// waitDelay é quanto o Run espera os pipes depois de matar o processo.
const waitDelay = 10 * time.Second

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

	ctx, cancel := context.WithTimeout(ctx, cloneTimeout)
	defer cancel()

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
	// gh repo clone só traz os refs remotos que o clone inicial configurou —
	// geralmente a branch padrão. Um PR para uma release branch precisa da base
	// explícita para que o agente consiga comparar origin/<base>...HEAD.
	if base := strings.TrimSpace(pr.BaseRefName); base != "" {
		if err := ensurePRBase(ctx, dir, base); err != nil {
			ws.Remove()
			return nil, fmt.Errorf("base %s de %s: %w", base, pr.Key(), err)
		}
	}
	return ws, nil
}

// ensurePRBase makes origin/<base> and a local <base> branch available when
// possible. A failed fetch is not fatal if the tracking ref is already there;
// git branch --force is skipped when <base> is already HEAD.
func ensurePRBase(ctx context.Context, dir, base string) error {
	refspec := "+refs/heads/" + base + ":refs/remotes/origin/" + base
	if err := run(ctx, dir, "git", "fetch", "origin", refspec); err != nil {
		if verify := run(ctx, dir, "git", "rev-parse", "--verify", "--quiet", "origin/"+base); verify != nil {
			return fmt.Errorf("buscando: %w", err)
		}
	}
	head, err := gitOutput(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	if head == base {
		return nil
	}
	if err := run(ctx, dir, "git", "branch", "--force", base, "origin/"+base); err != nil {
		return fmt.Errorf("criando branch local: %w", err)
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// O git que o gh dispara herda o stderr: sem WaitDelay, matar o gh no
	// timeout não solta o Run enquanto o neto pendurado segura o pipe.
	cmd.WaitDelay = waitDelay
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%s", lastLines(msg, 3))
		}
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
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
	// O git que o gh dispara herda o stderr: sem WaitDelay, matar o gh no
	// timeout não solta o Run enquanto o neto pendurado segura o pipe.
	cmd.WaitDelay = waitDelay
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
