// Package store salva os reviews em markdown no disco.
package store

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/beroni/bazel/internal/agent"
)

// Save grava o review em markdown e devolve o caminho do arquivo.
func Save(dir string, res agent.Result) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%d-%s.md",
		sanitize(res.PR.Repo),
		res.PR.Number,
		time.Now().Format("20060102-150405"),
	)
	path := filepath.Join(dir, name)

	var b strings.Builder
	fmt.Fprintf(&b, "# %s#%d — %s\n\n", res.PR.Repo, res.PR.Number, res.PR.Title)
	fmt.Fprintf(&b, "- Autor: @%s\n", res.PR.Author.Login)
	fmt.Fprintf(&b, "- Branch: `%s`\n", res.PR.HeadRefName)
	fmt.Fprintf(&b, "- URL: %s\n", res.PR.URL)
	fmt.Fprintf(&b, "- Diff: +%d −%d em %d arquivo(s)\n", res.PR.Additions, res.PR.Deletions, res.PR.ChangedFiles)
	fmt.Fprintf(&b, "- Revisado em: %s (levou %s)\n", time.Now().Format("2006-01-02 15:04"), res.Duration.Round(time.Second))
	if res.Agent != "" {
		fmt.Fprintf(&b, "- Agente: %s\n", agentLine(res))
	}
	if u := res.Usage.String(); u != "" {
		fmt.Fprintf(&b, "- Gasto: %s\n", u)
	}
	if res.Posts {
		b.WriteString("- ⇧ Este agente publicou o review no PR por conta própria.\n")
	}
	if res.Truncated {
		b.WriteString("- ⚠️ O diff foi truncado antes de ir para o agente.\n")
	}
	b.WriteString("\n---\n\n")
	b.WriteString(res.Body)
	b.WriteString("\n")

	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// CommentBody monta o texto publicado no PR.
func CommentBody(res agent.Result) string {
	var b strings.Builder
	b.WriteString("## 🥚 Review automático — Bazel\n\n")
	b.WriteString(res.Body)
	b.WriteString("\n\n---\n")
	fmt.Fprintf(&b, "<sub>Gerado por [Bazel](https://github.com/beroni/bazel) em %s", time.Now().Format("2006-01-02 15:04"))
	if res.Agent != "" {
		fmt.Fprintf(&b, " · %s", res.Agent)
	}
	if res.Truncated {
		b.WriteString(" · diff truncado")
	}
	b.WriteString("</sub>\n")
	return b.String()
}

// agentLine descreve quem rodou: o nome da escolha e, quando ela encadeia mais
// de um agente, o tempo de cada passo.
func agentLine(res agent.Result) string {
	if len(res.Steps) < 2 {
		return res.Agent
	}
	parts := make([]string, 0, len(res.Steps))
	for _, s := range res.Steps {
		part := fmt.Sprintf("%s (%s)", s.Name, s.Duration.Round(time.Second))
		if s.Err != nil {
			part += " ✗"
		}
		parts = append(parts, part)
	}
	return res.Agent + " — " + strings.Join(parts, " → ")
}

func sanitize(s string) string {
	return strings.NewReplacer("/", "-", " ", "-", ":", "-").Replace(s)
}

// Entry é um review já salvo em disco.
type Entry struct {
	Name    string    `json:"name"`
	Title   string    `json:"title"`
	ModTime time.Time `json:"mod_time"`
	Size    int64     `json:"size"`
}

// List devolve os reviews salvos, do mais novo para o mais velho. Diretório
// inexistente é uma lista vazia, não um erro: ninguém revisou nada ainda.
func List(dir string) ([]Entry, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Entry
	for _, it := range items {
		if it.IsDir() || !strings.HasSuffix(it.Name(), ".md") {
			continue
		}
		info, err := it.Info()
		if err != nil {
			continue
		}
		out = append(out, Entry{
			Name:    it.Name(),
			Title:   heading(filepath.Join(dir, it.Name())),
			ModTime: info.ModTime(),
			Size:    info.Size(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

// Read devolve o markdown de um review salvo. name é só o nome do arquivo —
// qualquer coisa com separador de caminho é recusada para não deixar a
// interface web ler fora do diretório de reviews.
func Read(dir, name string) (string, error) {
	if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") {
		return "", fmt.Errorf("nome de review inválido: %q", name)
	}
	if !strings.HasSuffix(name, ".md") {
		return "", fmt.Errorf("nome de review inválido: %q", name)
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// heading lê o título do review (a primeira linha "# ..."), sem carregar o
// arquivo inteiro.
func heading(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for i := 0; sc.Scan() && i < 5; i++ {
		if line := strings.TrimSpace(sc.Text()); strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return ""
}
