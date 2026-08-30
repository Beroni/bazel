package server

import (
	"bytes"
	"html"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// md converte markdown em HTML. Sem goldmark.WithUnsafe(): HTML cru no
// markdown sai escapado, não executado.
var md = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM,
		extension.Footnote,
	),
)

// policy limpa o HTML depois de convertido.
//
// O markdown não vem só do agente: a descrição do PR é escrita por quem abriu
// o PR. goldmark escapa HTML cru, mas ainda emite o link que o markdown pediu
// — e `[clica](javascript:...)` num servidor que dispara review e comenta no
// GitHub no seu nome não é um link qualquer. UGCPolicy corta esquema que não
// seja http/https/mailto.
var policy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	// Checkbox de task list do GFM, que o UGCPolicy tiraria.
	p.AllowAttrs("type", "checked", "disabled").Matching(bluemonday.Paragraph).OnElements("input")
	// Sem isto o class="language-go" some e o bloco de código perde a marca.
	p.AllowAttrs("class").OnElements("code", "pre", "span", "div", "li")
	return p
}()

// renderMarkdown devolve o HTML seguro para jogar no innerHTML da página.
func renderMarkdown(src string) string {
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		return "<pre>" + html.EscapeString(src) + "</pre>"
	}
	return string(policy.SanitizeBytes(buf.Bytes()))
}
