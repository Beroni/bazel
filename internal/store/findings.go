package store

import (
	"os"
	"path/filepath"
	"strings"
)

// Part é um pedaço do review: ou um achado, ou o texto entre achados.
//
// Um achado é o bloco de um `###` dentro de uma seção `## Findings` ou
// `## Cuts` — do heading até o próximo heading — ou qualquer `###` seguido do
// comentário `<!-- finding ... -->` que o review-fleet grava. Seção que não
// usa `###` e numera os achados em negrito (`**1. major · …**`, o formato dos
// reviews mais antigos) tem cada parágrafo numerado como um achado, até o
// próximo número ou heading. Em `## Needs human verification` cada item da
// lista também é um: a dúvida que você já resolveu não precisa ir ao PR. É a
// unidade que a página deixa marcar: um falso positivo sai do que vai ao PR
// sem que o resto do review mude.
type Part struct {
	Text    string
	Finding bool
}

// SplitFindings corta o review nos achados, na ordem em que aparecem. A
// concatenação das partes devolve o texto original: o corte só marca onde
// cada achado começa e termina, para a página numerá-los e o publish tirar
// os desmarcados.
func SplitFindings(body string) []Part {
	lines := strings.SplitAfter(body, "\n")
	var (
		parts   []Part
		cur     strings.Builder
		finding bool
		inFence bool
		fence   string
		section bool // dentro de um `##` de findings/cuts/human verification
		human   bool // e é a de human verification, que lista em vez de `###`
		h3Seen  bool // a seção atual numera os achados com `###`
	)
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		parts = append(parts, Part{Text: cur.String(), Finding: finding})
		cur.Reset()
	}
	nextNonBlank := func(i int) string {
		for j := i + 1; j < len(lines); j++ {
			if t := strings.TrimSpace(lines[j]); t != "" {
				return t
			}
		}
		return ""
	}
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		if inFence {
			if strings.HasPrefix(trim, fence) {
				inFence = false
			}
			cur.WriteString(line)
			continue
		}
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			inFence, fence = true, trim[:3]
			cur.WriteString(line)
			continue
		}
		level, title := headingOf(trim)
		switch {
		case level == 0:
			// Achado em negrito numerado, nas seções que não usam `###`.
			if section && !h3Seen && boldNumbered(trim) {
				flush()
				finding = true
			}
			// Item de lista no começo da linha abre outro; parágrafo solto
			// fecha o item e não é de ninguém. Linha recuada ou em branco
			// fica no item em que aparece.
			if human && !h3Seen && trim != "" && line[0] != ' ' && line[0] != '\t' {
				switch {
				case listItem(trim):
					flush()
					finding = true
				case !boldNumbered(trim) && finding:
					flush()
					finding = false
				}
			}
			cur.WriteString(line)
			continue
		case level <= 2:
			// Um heading maior fecha o achado que estava aberto, e um `##`
			// diz se o que vem agora é uma seção de achados.
			flush()
			finding = false
			human = level == 2 && isHumanSection(title)
			section = level == 2 && (isFindingsSection(title) || human)
			h3Seen = false
		case level == 3:
			flush()
			h3Seen = true
			finding = section || strings.HasPrefix(nextNonBlank(i), "<!-- finding")
		default:
			// `####` e abaixo ficam dentro do achado em que aparecem.
		}
		cur.WriteString(line)
	}
	flush()
	return parts
}

// Findings conta os achados de um review.
func Findings(body string) int {
	n := 0
	for _, p := range SplitFindings(body) {
		if p.Finding {
			n++
		}
	}
	return n
}

// DropFindings devolve o review sem os achados de índice em skip — a
// numeração é a mesma do SplitFindings, e é a que a página usa nas caixas.
// Índice fora da faixa é ignorado.
func DropFindings(body string, skip []int) string {
	if len(skip) == 0 {
		return body
	}
	out := make(map[int]bool, len(skip))
	for _, i := range skip {
		out[i] = true
	}
	var b strings.Builder
	idx := 0
	for _, p := range SplitFindings(body) {
		if p.Finding {
			if out[idx] {
				idx++
				continue
			}
			idx++
		}
		b.WriteString(p.Text)
	}
	return b.String()
}

// SavePublishCopy grava, ao lado do review salvo, a versão dele que vai ao
// PR: o mesmo cabeçalho, mas com o corpo já sem os achados desmarcados. Fica
// em publish/ dentro do diretório de reviews, para não aparecer na lista dos
// salvos — o review de verdade continua inteiro, é só o que sai que muda.
func SavePublishCopy(dir, original, body string) (string, error) {
	pubDir := filepath.Join(dir, "publish")
	if err := os.MkdirAll(pubDir, 0o755); err != nil {
		return "", err
	}
	header := ""
	if data, err := os.ReadFile(original); err == nil {
		if head, _, ok := strings.Cut(string(data), "\n---\n"); ok {
			header = head + "\n---\n\n"
		}
	}
	path := filepath.Join(pubDir, filepath.Base(original))
	if err := os.WriteFile(path, []byte(header+strings.TrimSpace(body)+"\n"), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// boldNumbered reconhece a linha que abre um achado numerado em negrito:
// `**1. …`, `**2)** …`.
func boldNumbered(trim string) bool {
	rest := strings.TrimLeft(strings.TrimPrefix(trim, "**"), " ")
	if len(rest) == len(trim) {
		return false
	}
	n := 0
	for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
		n++
	}
	return n > 0 && n < len(rest) && (rest[n] == '.' || rest[n] == ')')
}

// headingOf devolve o nível de um heading ATX e o título, ou 0 se a linha
// não for heading.
func headingOf(trim string) (int, string) {
	n := 0
	for n < len(trim) && trim[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return 0, ""
	}
	if n == len(trim) {
		return n, ""
	}
	if trim[n] != ' ' && trim[n] != '\t' {
		return 0, ""
	}
	return n, strings.TrimSpace(trim[n:])
}

// isFindingsSection reconhece os `##` que agrupam achados — "Findings",
// "Cuts (lazy-senior-dev)", "Cuts — `lazy-senior-dev`" — pelo começo do
// título, porque o resto varia de review para review.
func isFindingsSection(title string) bool {
	t := strings.ToLower(strings.TrimLeft(title, "0123456789. "))
	return strings.HasPrefix(t, "finding") || strings.HasPrefix(t, "cut")
}

// isHumanSection reconhece o `## Needs human verification` que a frota grava
// com o que as lentes não conseguiram confirmar.
func isHumanSection(title string) bool {
	t := strings.ToLower(strings.TrimLeft(title, "0123456789. "))
	return strings.HasPrefix(t, "needs human")
}

// listItem diz se a linha abre um item de lista: `- `, `* `, `+ ` ou `1. `.
func listItem(trim string) bool {
	if strings.HasPrefix(trim, "- ") || strings.HasPrefix(trim, "* ") || strings.HasPrefix(trim, "+ ") {
		return true
	}
	n := 0
	for n < len(trim) && trim[n] >= '0' && trim[n] <= '9' {
		n++
	}
	return n > 0 && n+1 < len(trim) && (trim[n] == '.' || trim[n] == ')') && trim[n+1] == ' '
}

// Unwrap tira o review de dentro de um bloco ```markdown, quando o agente
// colou o relatório inteiro num bloco de código em vez de devolvê-lo solto.
// Dentro do bloco os headings viram texto: a página mostra tudo como código,
// o GitHub também, e nenhum achado tem onde receber caixa. Só desembrulha
// quando o bloco tem heading — um trecho de markdown citado num achado, que
// não tem, fica como está. As cercas internas do relatório (```go…```)
// continuam pareadas; o que sai é só a cerca de fora e o seu fecho.
func Unwrap(body string) string {
	lines := strings.SplitAfter(body, "\n")
	open := -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "```markdown" || t == "```md" {
			open = i
			break
		}
	}
	if open < 0 {
		return body
	}
	close, inner, heading := -1, false, false
	for i := open + 1; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		switch {
		case strings.HasPrefix(t, "```") && len(t) > 3:
			inner = true
		case t == "```" && inner:
			inner = false
		case t == "```":
			close = i
		default:
			if lvl, _ := headingOf(t); lvl > 0 && lvl <= 3 {
				heading = true
			}
			continue
		}
		if close >= 0 {
			break
		}
	}
	if !heading {
		return body
	}
	var b strings.Builder
	for i, l := range lines {
		if i == open || i == close {
			continue
		}
		b.WriteString(l)
	}
	return b.String()
}
