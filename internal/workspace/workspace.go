// Package workspace prepares a disposable clone of the repository, with the pull
// request already checked out, for the agent to run inside it.
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

// Workspace is a temporary clone with the PR checked out.
type Workspace struct {
	// Dir is the root of the clone.
	Dir string
	// Keep preserves the directory on Cleanup, for manual inspection.
	Keep bool
}

// cloneTimeout limits the clone and the checkout. The agent's timeout_seconds
// only starts counting at the step, after the clone; without this limit a `gh
// repo clone` hanging on the network holds the worker until someone cancels
// by hand.
var cloneTimeout = 15 * time.Minute

// waitDelay is how long Run waits for the pipes after killing the process.
const waitDelay = 10 * time.Second

// Prepare clones the PR's repository into a temporary folder and checks out
// the PR branch. The clone is blobless (--filter=blob:none) instead of
// shallow: it downloads little but keeps the whole history — without it the
// agent's `git diff base...HEAD` cannot find the merge-base.
func Prepare(ctx context.Context, pr gh.PR) (*Workspace, error) {
	dir, err := os.MkdirTemp("", fmt.Sprintf("bazel-%s-%d-", sanitize(pr.Slug()), pr.Number))
	if err != nil {
		return nil, fmt.Errorf("criando pasta temporária: %w", err)
	}
	ws := &Workspace{Dir: dir}

	ctx, cancel := context.WithTimeout(ctx, cloneTimeout)
	defer cancel()

	// git clone accepts an existing directory as long as it is empty.
	if err := run(ctx, "", "gh", "repo", "clone", pr.Repo, dir, "--", "--filter=blob:none"); err != nil {
		ws.Remove()
		return nil, fmt.Errorf("clonando %s: %w", pr.Repo, err)
	}
	// `gh pr checkout` handles PRs coming from forks on its own.
	if err := run(ctx, dir, "gh", "pr", "checkout", strconv.Itoa(pr.Number)); err != nil {
		ws.Remove()
		return nil, fmt.Errorf("checkout de %s: %w", pr.Key(), err)
	}
	// gh repo clone only brings the remote refs the initial clone configured —
	// usually the default branch. A PR against a release branch needs the base
	// fetched explicitly so the agent can compare origin/<base>...HEAD.
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
	// The git that gh spawns inherits stderr: without WaitDelay, killing gh on
	// timeout does not release Run while the hung grandchild holds the pipe.
	cmd.WaitDelay = waitDelay
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%s", lastLines(msg, 3))
		}
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Cleanup deletes the clone, unless Keep is on.
func (w *Workspace) Cleanup() {
	if w == nil || w.Keep {
		return
	}
	w.Remove()
}

// Remove deletes the clone unconditionally.
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
	// The git that gh spawns inherits stderr: without WaitDelay, killing gh on
	// timeout does not release Run while the hung grandchild holds the pipe.
	cmd.WaitDelay = waitDelay
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s", lastLines(msg, 3))
		}
		return err
	}
	return nil
}

// lastLines trims the git/gh error output, which usually comes with a
// progress bar before the message that matters.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// sanitize keeps only what can become a temporary directory prefix.
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
