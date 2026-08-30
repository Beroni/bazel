// Package skills descobre as skills do Claude Code instaladas na máquina.
//
// É o que permite a página mostrar os agentes disponíveis de verdade — os que
// estão em ~/.claude/skills — em vez de uma lista escrita à mão que pode
// apontar para uma skill que você nunca instalou.
package skills

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Skill é uma skill instalada.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Dir é a pasta dela, útil quando o nome do diretório e o do frontmatter
	// não batem.
	Dir string `json:"dir"`
}

// DefaultDir é onde o Claude Code guarda as skills do usuário.
func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "skills")
}

// List devolve as skills de um diretório, em ordem alfabética. Diretório
// inexistente é uma lista vazia: nem toda máquina tem skills instaladas, e
// isso não é erro.
func List(dir string) []Skill {
	if strings.TrimSpace(dir) == "" {
		dir = DefaultDir()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Skill
	for _, e := range entries {
		// Sem filtrar por IsDir: as skills costumam ser symlinks para o
		// repositório onde você as versiona, e para o os.ReadDir um symlink
		// não é diretório. Quem decide é o SKILL.md lá dentro.
		s, ok := read(filepath.Join(dir, e.Name()))
		if !ok {
			continue
		}
		if s.Name == "" {
			s.Name = e.Name()
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// read lê o frontmatter de um SKILL.md. Pasta sem SKILL.md não é skill.
func read(dir string) (Skill, bool) {
	f, err := os.Open(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return Skill{}, false
	}
	defer f.Close()

	s := Skill{Dir: dir}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		// Sem frontmatter o nome da pasta é o que temos.
		return s, true
	}

	key := ""
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		// Linha indentada continua o valor anterior: descrições longas vêm
		// quebradas em várias linhas no YAML.
		if key != "" && (strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t")) {
			if key == "description" {
				s.Description = strings.TrimSpace(s.Description + " " + strings.TrimSpace(line))
			}
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			key = ""
			continue
		}
		key = strings.TrimSpace(name)
		value = strings.TrimSpace(strings.Trim(strings.TrimSpace(value), `"'`))
		switch key {
		case "name":
			s.Name = value
		case "description":
			s.Description = value
		}
	}
	return s, true
}

// TaskSkill devolve a skill que uma task invoca — o "/review-fleet" de
// "/review-fleet {{number}} --post". Task que não começa com barra não chama
// skill nenhuma.
func TaskSkill(task string) string {
	task = strings.TrimSpace(task)
	if !strings.HasPrefix(task, "/") {
		return ""
	}
	name := strings.TrimPrefix(task, "/")
	if i := strings.IndexAny(name, " \t\n"); i >= 0 {
		name = name[:i]
	}
	return name
}

// Has diz se uma skill está entre as instaladas.
func Has(list []Skill, name string) bool {
	for _, s := range list {
		if strings.EqualFold(s.Name, name) {
			return true
		}
	}
	return false
}
