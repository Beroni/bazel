package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const relatorio = `# Review Fleet — PR #55

**Verdict:** request_changes · 2 findings

## Findings

### 1. Gas diverges by platform
**critical** · ` + "`runtime.go:265`" + `

Prose of the first finding.

` + "```go\n## not a heading\n### nor this\n```" + `

### 2. No host-side test
**major**

#### detail inside the second

Prose of the second.

## Cuts (lazy-senior-dev)

### Hand-rolled retry
<!-- finding severity=minor class=shrink lenses=lazy-senior-dev path=retry.ts line=12 side=RIGHT lines_saved=27 -->

Delete the helper.

## Needs human verification

- something to confirm
  with a second line
- another doubt

Prose after the list is not an item.

## Notes

### Not a finding: h3 outside the sections

## Coverage

- clean
`

// A concatenação das partes devolve o texto original, e só os `###` das
// seções de achados viram achado — cerca de código e `####` ficam dentro.
func TestSplitFindingsRecortaSoOsAchados(t *testing.T) {
	parts := SplitFindings(relatorio)
	var back strings.Builder
	var titulos []string
	for _, p := range parts {
		back.WriteString(p.Text)
		if p.Finding {
			titulos = append(titulos, strings.TrimSpace(strings.SplitN(p.Text, "\n", 2)[0]))
		}
	}
	if back.String() != relatorio {
		t.Fatal("juntar as partes devia devolver o review inteiro")
	}
	want := []string{"### 1. Gas diverges by platform", "### 2. No host-side test", "### Hand-rolled retry",
		"- something to confirm", "- another doubt"}
	if strings.Join(titulos, "|") != strings.Join(want, "|") {
		t.Fatalf("achados errados:\n%v\nqueria\n%v", titulos, want)
	}
	if n := Findings(relatorio); n != 5 {
		t.Errorf("Findings = %d, queria 5", n)
	}
	// O primeiro achado leva a cerca de código inteira, com os falsos headings.
	for _, p := range parts {
		if p.Finding && strings.HasPrefix(p.Text, "### 1.") && !strings.Contains(p.Text, "### nor this") {
			t.Error("o bloco de código devia ficar dentro do achado")
		}
		if p.Finding && strings.HasPrefix(p.Text, "### 2.") && !strings.Contains(p.Text, "#### detail") {
			t.Error("o #### devia ficar dentro do achado")
		}
		if p.Finding && strings.HasPrefix(p.Text, "- something") && !strings.Contains(p.Text, "with a second line") {
			t.Error("a linha recuada devia ficar dentro do item")
		}
		if p.Finding && strings.Contains(p.Text, "Prose after the list") {
			t.Error("o parágrafo depois da lista não é item")
		}
	}
}

// Sem seção `## Findings`, o comentário do review-fleet ainda marca o achado.
func TestSplitFindingsReconhecePeloComentario(t *testing.T) {
	src := "# Report\n\n## Security\n\n### IDOR on transfer\n<!-- finding severity=critical class=authz lenses=x path=a.go line=1 side=RIGHT -->\n\ntext\n\n### Just a note\n\nno comment here\n"
	var got []string
	for _, p := range SplitFindings(src) {
		if p.Finding {
			got = append(got, strings.TrimSpace(strings.SplitN(p.Text, "\n", 2)[0]))
		}
	}
	if len(got) != 1 || got[0] != "### IDOR on transfer" {
		t.Errorf("achados: %v", got)
	}
}

// Os reviews mais antigos numeram os achados em negrito, sem `###`: cada
// parágrafo numerado é um achado, e a prosa antes do primeiro fica de fora.
func TestSplitFindingsReconheceNegritoNumerado(t *testing.T) {
	src := "## Findings (2) — both pre-existing\n\nNeither is a reason to hold the merge.\n\n" +
		"**1. `major · error-handling` — panic escapes** (`routes.go:398`)\n\nprose one\n\n**Fix:** recover.\n\n" +
		"**2. major · resource — fan-out** (`routes.go:360`)\nprose two\n\n" +
		"## Cuts (−4 lines)\n\n`routes.go:401` dead check.\n\n## Coverage\n\n**1. not a finding here**\n"
	parts := SplitFindings(src)
	var got []string
	for _, p := range parts {
		if p.Finding {
			got = append(got, strings.TrimSpace(strings.SplitN(p.Text, "\n", 2)[0]))
		}
	}
	if len(got) != 2 || !strings.HasPrefix(got[0], "**1. `major") || !strings.HasPrefix(got[1], "**2. major") {
		t.Fatalf("achados: %q", got)
	}
	if !strings.Contains(parts[0].Text, "Neither is a reason") || parts[0].Finding {
		t.Error("a prosa antes do primeiro achado não é achado")
	}
	rest := DropFindings(src, []int{0})
	if strings.Contains(rest, "prose one") || strings.Contains(rest, "**Fix:** recover") || !strings.Contains(rest, "prose two") {
		t.Errorf("corte errado:\n%s", rest)
	}
	// Numeração em negrito dentro de um achado `###` não abre outro achado.
	mixed := "## Findings\n\n### Real one\n\n**1.** step\n\n**2.** step\n\n### Second\n\nx\n"
	if n := Findings(mixed); n != 2 {
		t.Errorf("com ###, o negrito numerado fica dentro: %d achados, queria 2", n)
	}
}

func TestDropFindingsTiraSoOsDesmarcados(t *testing.T) {
	got := DropFindings(relatorio, []int{1, 99})
	if strings.Contains(got, "No host-side test") || strings.Contains(got, "Prose of the second") {
		t.Errorf("o achado 2 devia ter saído:\n%s", got)
	}
	for _, keep := range []string{"### 1. Gas diverges", "### Hand-rolled retry", "## Needs human verification", "## Coverage", "**Verdict:**"} {
		if !strings.Contains(got, keep) {
			t.Errorf("faltou %q depois do corte:\n%s", keep, got)
		}
	}
	if DropFindings(relatorio, nil) != relatorio {
		t.Error("sem skip o review não muda")
	}
	if Findings(got) != 4 {
		t.Errorf("sobraram %d achados, queria 4", Findings(got))
	}
}

// A cópia que vai ao agente tem o cabeçalho do Save e o corpo já cortado, e
// mora em publish/ — fora da lista dos salvos. O original não muda.
func TestSavePublishCopyPreservaOCabecalho(t *testing.T) {
	dir := t.TempDir()
	orig := filepath.Join(dir, "acme-api-482-20260909-120000.md")
	full := "# acme/api#482 — feat\n\n- Author: @maria\n\n---\n\n" + relatorio
	if err := os.WriteFile(orig, []byte(full), 0o644); err != nil {
		t.Fatal(err)
	}
	body := DropFindings(relatorio, []int{0})
	path, err := SavePublishCopy(dir, orig, body)
	if err != nil {
		t.Fatalf("SavePublishCopy: %v", err)
	}
	if filepath.Dir(path) != filepath.Join(dir, "publish") || filepath.Base(path) != filepath.Base(orig) {
		t.Errorf("caminho inesperado: %s", path)
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	if !strings.HasPrefix(got, "# acme/api#482 — feat\n\n- Author: @maria\n\n---\n\n") {
		t.Errorf("cabeçalho perdido:\n%s", got)
	}
	if strings.Contains(got, "Gas diverges") || !strings.Contains(got, "No host-side test") {
		t.Errorf("corpo errado:\n%s", got)
	}
	if ReviewBody(got) != strings.TrimSpace(body) {
		t.Error("ReviewBody da cópia devia ser o corpo cortado")
	}
	if back, _ := os.ReadFile(orig); string(back) != full {
		t.Error("o review original não devia mudar")
	}
	entries, _ := List(dir)
	if len(entries) != 1 {
		t.Errorf("a cópia não devia aparecer entre os salvos: %+v", entries)
	}
}

// Relatório colado num bloco ```markdown sai do bloco; as cercas de código
// de dentro dele ficam pareadas, e um trecho de markdown sem heading não é
// desembrulhado.
func TestUnwrapTiraOBlocoMarkdownDeFora(t *testing.T) {
	src := "All lenses returned.\n\n```markdown\n# Review Fleet\n\n## Findings\n\n### One\n\n```go\nx := 1\n```\n\ntext\n```\n\nNotes after.\n"
	got := Unwrap(src)
	want := "All lenses returned.\n\n# Review Fleet\n\n## Findings\n\n### One\n\n```go\nx := 1\n```\n\ntext\n\nNotes after.\n"
	if got != want {
		t.Errorf("Unwrap:\n%s\nqueria:\n%s", got, want)
	}
	if Findings(got) != 1 || Findings(src) != 0 {
		t.Error("o achado só aparece depois de desembrulhar")
	}
	quote := "### Finding\n\nThe README shows:\n\n```markdown\n- item\n- item\n```\n"
	if Unwrap(quote) != quote {
		t.Error("trecho de markdown sem heading não é o relatório")
	}
	if ReviewBody("# acme#1 — t\n\n---\n\n```markdown\n## Findings\n\n### A\n```\n") != "## Findings\n\n### A" {
		t.Error("ReviewBody devia desembrulhar")
	}
}

// A dúvida que você já resolveu sai do que vai ao PR como um achado: a caixa
// é a mesma, e o resto da seção fica.
func TestDropFindingsTiraDuvidaResolvida(t *testing.T) {
	got := DropFindings(relatorio, []int{3})
	if strings.Contains(got, "something to confirm") || strings.Contains(got, "with a second line") {
		t.Errorf("a dúvida desmarcada devia ter saído:\n%s", got)
	}
	for _, keep := range []string{"## Needs human verification", "- another doubt", "Prose after the list"} {
		if !strings.Contains(got, keep) {
			t.Errorf("faltou %q depois do corte:\n%s", keep, got)
		}
	}
}
