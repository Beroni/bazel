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

// Concatenating the parts gives back the original text, and only the `###` of
// findings sections become findings — code fences and `####` stay inside.
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
	// The first finding takes the whole code fence, with the fake headings.
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

// Without a `## Findings` section, the review-fleet comment still marks the finding.
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

// Older reviews number findings in bold, without `###`: each numbered
// paragraph is a finding, and the prose before the first stays out.
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
	// Bold numbering inside a `###` finding does not open another finding.
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

// The copy that goes to the agent has the Save header and the body already
// cut, and lives in publish/ — out of the saved list. The original is unchanged.
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
	if filepath.Dir(path) != filepath.Join(dir, "publish") || !strings.HasPrefix(filepath.Base(path), "acme-api-482-20260909-120000-") || filepath.Ext(path) != ".md" {
		t.Errorf("caminho inesperado: %s", path)
	}
	// Each publication gets its own copy: the second does not overwrite the first.
	outra, err := SavePublishCopy(dir, orig, relatorio)
	if err != nil || outra == path {
		t.Errorf("a segunda cópia devia ter outro nome: %s (%v)", outra, err)
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

// A report pasted into a ```markdown block comes out of the block; the code
// fences inside it stay paired, and a markdown snippet without a heading is
// not unwrapped.
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

// A doubt you already resolved drops out of what goes to the PR like a
// finding: the checkbox is the same, and the rest of the section stays.
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

// A list item that wraps with no indent (markdown's lazy continuation) is still
// the same item; only a paragraph after a blank line closes it.
func TestSplitFindingsDuvidaComContinuacaoPreguicosa(t *testing.T) {
	src := "## Needs human verification\n\n- first doubt\n- second doubt that wraps\nonto a second line\n- third doubt\n\nProse after the list.\n"
	var got []string
	for _, p := range SplitFindings(src) {
		if p.Finding {
			got = append(got, p.Text)
		}
	}
	if len(got) != 3 || got[1] != "- second doubt that wraps\nonto a second line\n" {
		t.Fatalf("itens: %q", got)
	}
	if strings.Contains(got[2], "Prose after") {
		t.Errorf("o parágrafo depois da linha em branco não é do item: %q", got[2])
	}
	rest := DropFindings(src, []int{1})
	if strings.Contains(rest, "second doubt") || strings.Contains(rest, "onto a second line") {
		t.Errorf("a dúvida desmarcada devia sair inteira:\n%s", rest)
	}
	for _, keep := range []string{"- first doubt", "- third doubt", "Prose after the list."} {
		if !strings.Contains(rest, keep) {
			t.Errorf("faltou %q depois do corte:\n%s", keep, rest)
		}
	}
}

// In `## Needs human verification` an item may also come numbered in bold, with
// the explaining paragraph after it: the item runs to the next number, item or
// heading, so unticking 1 takes the text along.
func TestSplitFindingsDuvidaNegritoNumerado(t *testing.T) {
	src := "## Needs human verification\n\n" +
		"**1. major · auth — token check** (`a.go:1`)\n\nThe token is never verified on refresh.\n\n" +
		"**2. minor · cache — stale read** (`b.go:2`)\n\nThe cache is read before the lock.\n\n" +
		"- plain doubt\n\n## Coverage\n\n**1. not a finding here**\n"
	parts := SplitFindings(src)
	var got []string
	for _, p := range parts {
		if p.Finding {
			got = append(got, p.Text)
		}
	}
	if len(got) != 3 {
		t.Fatalf("achados: %q", got)
	}
	if !strings.Contains(got[0], "never verified on refresh") || strings.Contains(got[0], "**2.") {
		t.Errorf("o item 1 devia levar o parágrafo e parar no 2: %q", got[0])
	}
	if !strings.Contains(got[1], "read before the lock") || strings.Contains(got[1], "plain doubt") {
		t.Errorf("o item 2 devia parar na lista: %q", got[1])
	}
	var all strings.Builder
	for _, p := range parts {
		all.WriteString(p.Text)
	}
	if all.String() != src {
		t.Error("a concatenação das partes devia devolver o original")
	}
	rest := DropFindings(src, []int{0})
	if strings.Contains(rest, "token check") || strings.Contains(rest, "never verified") {
		t.Errorf("desmarcar o 1 devia levar o parágrafo junto:\n%s", rest)
	}
	for _, keep := range []string{"**2. minor", "read before the lock", "- plain doubt", "**1. not a finding here**"} {
		if !strings.Contains(rest, keep) {
			t.Errorf("faltou %q depois do corte:\n%s", keep, rest)
		}
	}
}

// A seção de dúvidas vazia ("- None.") não ganha caixa: não há o que desmarcar.
func TestSplitFindingsDuvidaVaziaNaoEAchado(t *testing.T) {
	for _, item := range []string{"- None.", "- **None**", "* n/a", "- Nenhuma.", "1. Nothing"} {
		body := "## Needs human verification\n\n" + item + "\n"
		if n := Findings(body); n != 0 {
			t.Errorf("%q virou %d achado(s)", item, n)
		}
		if got := DropFindings(body, []int{0}); got != body {
			t.Errorf("%q: nada devia sair, veio %q", item, got)
		}
	}
	// Uma dúvida de verdade que começa com a palavra continua sendo uma.
	if n := Findings("## Needs human verification\n\n- None of the callers check the error\n"); n != 1 {
		t.Errorf("dúvida que começa com None devia contar, veio %d", n)
	}
}
