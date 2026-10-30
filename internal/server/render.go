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
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// md converts markdown to HTML. No goldmark.WithUnsafe(): raw HTML in the
// markdown comes out escaped, not executed.
var md = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM,
		extension.Footnote,
	),
	goldmark.WithRendererOptions(
		renderer.WithNodeRenderers(util.Prioritized(imageAsLink{}, 100)),
	),
)

// reviewMD converts a review that may go to the PR. Raw HTML there is not
// omitted as in md: the text goes to the PR verbatim, so it would vanish from
// the screen and still be published. rawHTMLAsText shows it as escaped text
// instead, and none of it becomes an element. That does not make the screen the
// whole of the text: see hiddenText. The PR description keeps md, since it is
// the author's text and GitHub hides its raw HTML anyway.
var reviewMD = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM,
		extension.Footnote,
	),
	goldmark.WithRendererOptions(
		renderer.WithNodeRenderers(
			util.Prioritized(rawHTMLAsText{}, 100),
			util.Prioritized(imageAsLink{}, 100),
		),
	),
)

// imageAsLink draws a markdown image as a link to it. An <img> would be
// fetched straight from this machine as soon as the page shows it, with no
// proxy like GitHub's camo in between: whoever put it in a PR description, or
// got an agent to write it, would learn the user's IP and when the page was
// read.
type imageAsLink struct{}

func (imageAsLink) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindImage, renderImage)
}

func renderImage(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	img := n.(*ast.Image)
	// The alt is written as text: a link inside it, or this one inside a link
	// (the [![badge](x)](url) of a README), would nest anchors and lose the
	// outer one.
	label := "image"
	if alt := plainText(n, source); alt != "" {
		label += ": " + alt
	}
	if len(img.Title) > 0 {
		label += " (" + string(img.Title) + ")"
	}
	inLink := false
	for p := n.Parent(); p != nil; p = p.Parent() {
		inLink = inLink || p.Kind() == ast.KindLink
	}
	if inLink {
		w.WriteString(html.EscapeString(label))
		return ast.WalkSkipChildren, nil
	}
	w.WriteString(`<a href="`)
	w.Write(util.EscapeHTML(util.URLEscape(img.Destination, true)))
	w.WriteString(`">`)
	w.WriteString(html.EscapeString(label))
	w.WriteString("</a>")
	return ast.WalkSkipChildren, nil
}

// plainText is the text under n, without its markup.
func plainText(n ast.Node, source []byte) string {
	var b strings.Builder
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			switch t := c.(type) {
			case *ast.Text:
				b.Write(t.Segment.Value(source))
			case *ast.String:
				b.Write(t.Value)
			}
		}
		return ast.WalkContinue, nil
	})
	return b.String()
}

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
	return validFindingMeta(strings.TrimSpace(text))
}

// metaValues is the shape of each attribute of that comment, as the fleet's
// report format defines it: a closed set of keys, each value from a vocabulary
// or a narrow form. Only path has room for words strung together, since a path
// can be anything; the server cannot tell whether it exists.
var metaValues = map[string]*regexp.Regexp{
	"severity": regexp.MustCompile(`^(critical|major|minor|nit|high|medium|low|info)$`),
	"class": regexp.MustCompile(`^(injection|authz|validation|secrets|crypto|web3|logic|error-handling|regression|test-gap|test-validity|assumption|` +
		`perf|resource|architecture|quality|migration|idiom|deps|a11y|docs|hygiene|pr-mismatch|delete|reuse|stdlib|native|drop-dep|yagni|shrink|out-of-scope|` +
		`compatibility|concurrency|correctness|dead-code|lifecycle|observability|race|supply-chain|test-brittleness|test-coverage|test-quality)$`),
	"lenses":      regexp.MustCompile(`^(senior-code-reviewer|exploit-digger|lazy-senior-dev)(\+(senior-code-reviewer|exploit-digger|lazy-senior-dev))*$`),
	"path":        regexp.MustCompile(`^(-|[A-Za-z0-9_./@+-]{1,200})$`),
	"line":        regexp.MustCompile(`^(-|[0-9]{1,7})$`),
	"start_line":  regexp.MustCompile(`^[0-9]{1,7}$`),
	"side":        regexp.MustCompile(`^(LEFT|RIGHT)$`),
	"start_side":  regexp.MustCompile(`^(LEFT|RIGHT)$`),
	"confidence":  regexp.MustCompile(`^(10|[0-9]|high|medium|low)$`),
	"lines_saved": regexp.MustCompile(`^[0-9]{1,6}$`),
	"ref":         regexp.MustCompile(`^` + refID + `(,` + refID + `){0,3}$`),
}

// refID is one identifier a lens may cite, in the forms of the bodies that issue
// them, so that a ref cannot spell anything else.
const refID = `(CWE-[0-9]{1,5}|CVE-[0-9]{4}-[0-9]{4,7}|GHSA-[0-9a-z]{4}-[0-9a-z]{4}-[0-9a-z]{4}|OWASP-(A|API|M)[0-9]{1,2}(:[0-9]{4})?)`

// validFindingMeta reports whether text is that comment and nothing more: each
// key once, each value in its shape, plain ASCII. Anything else may be text
// someone wants the post agent to read, so it stays on screen.
func validFindingMeta(text string) bool {
	inner, ok := strings.CutPrefix(text, "<!-- finding")
	if !ok || len(text) > 400 {
		return false
	}
	inner, ok = strings.CutSuffix(inner, " -->")
	if !ok {
		return false
	}
	if inner == "" {
		return true
	}
	attrs, ok := strings.CutPrefix(inner, " ")
	if !ok {
		return false
	}
	seen := map[string]bool{}
	for _, f := range strings.Split(attrs, " ") {
		key, value, _ := strings.Cut(f, "=")
		shape, known := metaValues[key]
		if !known || seen[key] || !shape.MatchString(value) {
			return false
		}
		seen[key] = true
	}
	return true
}

// policy cleans the HTML after it is converted.
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
	// A click on a link must not tell the other site this page's address.
	p.RequireNoReferrerOnLinks(true)
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
	note := hiddenNote(parts)
	if len(parts) <= 1 {
		return note + renderReviewMarkdown(body)
	}
	var b strings.Builder
	b.WriteString(note)
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

// footnoteDefRe is one footnote definition, with its indented continuation.
var footnoteDefRe = regexp.MustCompile(`(?m)^ {0,3}\[\^([^\]\n]+)\]:.*(?:\n[ \t]+.*)*`)

// hiddenText flags some of what the markdown of one part carries and no screen
// shows, but GitHub receives and the post agent reads. It is best effort: this
// is a list of known forms, and markdown has more ways to hide text than any
// list (a table's extra cell, a fence's info string, invisible characters), so
// no warning does not mean nothing is hidden. The forms: link reference
// definitions that no
// link here uses, such as the `[//]: # (comment)` idiom, the titles of those
// that are used, and footnotes nothing refers to. Each part is rendered alone,
// so a definition used only by another part counts as hidden here: the screen
// shows that link as plain brackets.
func hiddenText(src string) []string {
	ctx := parser.NewContext()
	doc := reviewMD.Parser().Parse(text.NewReader([]byte(src)), parser.WithContext(ctx))
	used := map[string]bool{}
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if l, ok := n.(*ast.Link); ok && entering {
			used[string(l.Destination)] = true
		}
		return ast.WalkContinue, nil
	})
	var out []string
	for _, r := range ctx.References() {
		if used[string(r.Destination())] && len(r.Title()) == 0 {
			continue
		}
		def := fmt.Sprintf("[%s]: %s", r.Label(), r.Destination())
		if len(r.Title()) > 0 {
			def += fmt.Sprintf(" %q", r.Title())
		}
		out = append(out, def)
	}
	body := footnoteDefRe.ReplaceAllString(src, "")
	for _, m := range footnoteDefRe.FindAllStringSubmatch(src, -1) {
		if !strings.Contains(body, "[^"+m[1]+"]") {
			out = append(out, strings.TrimSpace(m[0]))
		}
	}
	return out
}

// hiddenNote warns, above the report, about the hidden text hiddenText found,
// and shows it. It is our HTML, with the agent's text escaped.
func hiddenNote(parts []store.Part) string {
	var items []string
	for _, p := range parts {
		items = append(items, hiddenText(p.Text)...)
	}
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<aside class="hidden-text" data-testid="review-hidden"><p>Hidden text found in this report's markdown. If the report is published it goes along. The post agent reads the report as data, and there may be more that Bazel did not flag:</p>`)
	for _, it := range items {
		b.WriteString("<pre>" + html.EscapeString(it) + "</pre>")
	}
	b.WriteString("</aside>")
	return b.String()
}
