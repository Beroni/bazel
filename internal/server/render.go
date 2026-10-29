package server

import (
	"bytes"
	"fmt"
	"html"
	"regexp"
	"strings"

	"github.com/beroni/bazel/internal/store"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
)

// md converts markdown to HTML. No goldmark.WithUnsafe(): raw HTML in the
// markdown comes out escaped, not executed.
var md = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM,
		extension.Footnote,
	),
)

// reviewMD converts a review that may go to the PR. Raw HTML there is not
// omitted as in md: the text goes to the PR verbatim, so it would vanish from
// the screen and still be published. rawHTMLAsText shows it as escaped text
// instead: what is on screen is what would be published, and none of it
// becomes an element. The PR description keeps md, since it is the author's
// text and GitHub hides its raw HTML anyway.
var reviewMD = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM,
		extension.Footnote,
	),
	goldmark.WithRendererOptions(
		renderer.WithNodeRenderers(util.Prioritized(rawHTMLAsText{}, 100)),
	),
)

// rawHTMLAsText writes the markdown's raw HTML, inline and block, as escaped
// text. A block goes in a <pre> so it does not run into the paragraph next to
// it. The one exception is the finding metadata (see isFindingMeta).
type rawHTMLAsText struct{}

func (rawHTMLAsText) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindRawHTML, renderRawHTML)
	reg.Register(ast.KindHTMLBlock, renderHTMLBlock)
}

func renderRawHTML(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkSkipChildren, nil
	}
	raw := n.(*ast.RawHTML)
	for i := 0; i < raw.Segments.Len(); i++ {
		seg := raw.Segments.At(i)
		w.WriteString(html.EscapeString(string(seg.Value(source))))
	}
	return ast.WalkSkipChildren, nil
}

func renderHTMLBlock(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	blk := n.(*ast.HTMLBlock)
	var raw strings.Builder
	lines := blk.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		raw.Write(seg.Value(source))
	}
	if blk.HasClosure() {
		raw.Write(blk.ClosureLine.Value(source))
	}
	text := raw.String()
	if isFindingMeta(blk, text) {
		return ast.WalkSkipChildren, nil
	}
	w.WriteString("<pre>")
	w.WriteString(html.EscapeString(text))
	w.WriteString("</pre>\n")
	return ast.WalkSkipChildren, nil
}

// isFindingMeta reports whether an HTML block is the `<!-- finding … -->`
// comment the review-fleet writes right after a finding's `###`: the same
// shape store.SplitFindings looks for. It is metadata the post agent reads and
// GitHub does not show, so the review on screen leaves it out too.
//
// Only the one-line key=value form the fleet writes is hidden. Anything else,
// such as a multi-line comment, could carry instructions to the post agent,
// so it shows in full as text.
func isFindingMeta(blk *ast.HTMLBlock, text string) bool {
	if blk.Parent() == nil || blk.Parent().Kind() != ast.KindDocument {
		return false
	}
	h, ok := blk.PreviousSibling().(*ast.Heading)
	if !ok || h.Level != 3 {
		return false
	}
	return findingMetaRe.MatchString(strings.TrimSpace(text))
}

var findingMetaRe = regexp.MustCompile(`^<!-- finding(?: [a-z_]+=[^\s<>]+)* -->$`)

// policy limpa o HTML depois de convertido.
//
// The markdown does not come only from the agent: the PR description is
// written by whoever opened the PR. goldmark escapes raw HTML, but still emits
// the link the markdown asked for — and `[clica](javascript:...)` on a server
// that fires off reviews and comments on GitHub in your name is not just any
// link. UGCPolicy cuts any scheme that is not http/https/mailto.
var policy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	// GFM task list checkbox, which UGCPolicy would strip.
	p.AllowAttrs("type", "checked", "disabled").Matching(bluemonday.Paragraph).OnElements("input")
	// Without this the class="language-go" disappears and the code block loses
	// its marker.
	p.AllowAttrs("class").OnElements("code", "pre", "span", "div", "li")
	return p
}()

// renderMarkdown returns HTML safe to drop into the page's innerHTML.
func renderMarkdown(src string) string {
	return convert(md, src)
}

// renderReviewMarkdown is renderMarkdown for a review: raw HTML shows as text.
func renderReviewMarkdown(src string) string {
	return convert(reviewMD, src)
}

// convert runs one of the converters above and sanitizes the result.
func convert(conv goldmark.Markdown, src string) string {
	var buf bytes.Buffer
	if err := conv.Convert([]byte(src), &buf); err != nil {
		return "<pre>" + html.EscapeString(src) + "</pre>"
	}
	return string(policy.SanitizeBytes(buf.Bytes()))
}

// renderReview converts a review to HTML with each finding wrapped in a
// <section class="finding" data-finding="N">. The page puts a checkbox on each
// one, and N is the same index publish receives in `skip` — both sides cut the
// markdown with the same store.SplitFindings, so the box unticked on screen is
// exactly the block that drops out of what goes to the PR.
//
// The wrapper stays outside sanitization: it is our HTML, not the agent's.
func renderReview(body string) string {
	parts := store.SplitFindings(body)
	if len(parts) <= 1 {
		return renderReviewMarkdown(body)
	}
	var b strings.Builder
	idx := 0
	for _, p := range parts {
		if !p.Finding {
			b.WriteString(renderReviewMarkdown(p.Text))
			continue
		}
		fmt.Fprintf(&b, `<section class="finding" data-finding="%d">`, idx)
		b.WriteString(renderReviewMarkdown(p.Text))
		b.WriteString("</section>")
		idx++
	}
	return b.String()
}
