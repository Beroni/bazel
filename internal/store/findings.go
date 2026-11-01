package store

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// Part is a piece of the review: either a finding, or the text between findings.
//
// A finding is the block of a `###` inside a `## Findings` or `## Cuts`
// section — from the heading to the next heading — or any `###` followed by
// the `<!-- finding ... -->` comment that review-fleet writes. A section that
// doesn't use `###` and numbers findings in bold (`**1. major · …**`, the
// format of older reviews) has each numbered paragraph as a finding, up to the
// next number or heading. In `## Needs human verification` each list item is
// one too: a doubt you have already resolved doesn't need to go to the PR. It
// is the unit the page lets you tick: a false positive drops out of what goes
// to the PR without the rest of the review changing.
// maxNesting is how many quotes and list items a single line may open. The
// parser spends quadratic time on them: 96 KB of `>` took 3 s, on every GET of a
// review.
const maxNesting = 1 << 10

// TooNested says some line opens more blocks than is worth parsing.
func TooNested(body string) bool {
	depth := 0
	opens := func(i int) bool {
		return strings.IndexByte("-+*.)", body[i]) >= 0 && i+1 < len(body) && (body[i+1] == ' ' || body[i+1] == '\t')
	}
	for i := 0; i < len(body); i++ {
		switch c := body[i]; {
		case c == '\n':
			depth = 0
		case depth < 0, c == ' ', c == '\t', c >= '0' && c <= '9':
		case c == '>', opens(i):
			if depth++; depth > maxNesting {
				return true
			}
		case strings.IndexByte("-+*.)", c) < 0:
			depth = -1
		}
	}
	return false
}

type Part struct {
	Text    string
	Finding bool
}

// SplitFindings cuts the review into its findings, in the order they appear.
// Concatenating the parts gives back the original text: the cut only marks
// where each finding starts and ends, so the page can number them and publish
// can drop the unticked ones.
func SplitFindings(body string) []Part {
	if TooNested(body) {
		return []Part{{Text: body}}
	}
	lines := strings.SplitAfter(body, "\n")
	headings, literal, paragraphs := blockLines(body, lines)
	var (
		parts   []Part
		cur     strings.Builder
		finding bool
		section bool // inside a `##` of findings/cuts/human verification
		human   bool // and it is the human verification one, which lists instead of `###`
		h3Seen  bool // the current section numbers findings with `###`
		bold    bool // the item open in the human section is a `**N. …**`
		blank   bool // the previous line was blank
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
	nextIsStep := func(i int) bool {
		for j := i + 1; j < len(lines); j++ {
			if t := strings.TrimSpace(lines[j]); t != "" {
				return headings[j] && strings.HasPrefix(t, "## ")
			}
		}
		return false
	}
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		// Code, quoted HTML and a fence's own lines are never structure.
		if literal[i] || strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			blank = false
			cur.WriteString(line)
			continue
		}
		level, title := 0, ""
		if headings[i] {
			level, title = headingOf(trim)
		}
		// The line Bazel puts between two steps of a pipeline belongs to neither:
		// dropping a step's last finding must leave the next step behind it. Bazel
		// writes it after a blank line: right under text it is that text's underline.
		if trim == "---" && blank && nextIsStep(i) {
			flush()
			finding, bold, section, human, blank = false, false, false, false, false
		}
		switch {
		case level == 0:
			wasBlank := blank
			blank = trim == ""
			// Numbered bold finding, in sections that do not use `###`.
			if section && !h3Seen && paragraphs[i] && boldNumbered(trim) {
				flush()
				finding = true
				bold = human
			}
			// A list item at the start of the line opens another one. An
			// indented or blank line stays in the item it shows up in, and a
			// bold item runs to the next number, item or heading, with its
			// paragraphs. In a list item, a line that continues the text with
			// no blank line in between (lazy continuation) stays too; only a
			// paragraph after a blank line closes the item.
			if human && !h3Seen && trim != "" && line[0] != ' ' && line[0] != '\t' {
				switch {
				case listItem(trim):
					flush()
					// "- None." says there is no doubt: it is not one to uncheck.
					finding = !emptyItem(trim)
					bold = false
				case !boldNumbered(trim) && finding && !bold && wasBlank:
					flush()
					finding = false
				}
			}
			cur.WriteString(line)
			continue
		case level <= 2:
			// A bigger heading closes the finding that was open, and a `##`
			// says whether what comes next is a findings section.
			flush()
			finding, bold, blank = false, false, false
			human = level == 2 && isHumanSection(title)
			section = level == 2 && (isFindingsSection(title) || human)
			h3Seen = false
		case level == 3:
			flush()
			bold, blank = false, false
			h3Seen = true
			finding = section || strings.HasPrefix(nextNonBlank(i), "<!-- finding")
		default:
			// `####` and below stay inside the finding where they appear.
			blank = false
		}
		cur.WriteString(line)
	}
	flush()
	return parts
}

// blockParser reads a report's blocks as the renderer does, so that both agree on
// what is a heading. With no inline parsers: they are what takes quadratic time
// on some text, and this runs over the whole report, however long.
var blockParser = parser.NewParser(parser.WithBlockParsers(parser.DefaultBlockParsers()...))

// blockLines says, line by line, which are headings at the top of the document,
// which are text at that level, and which belong to a code or HTML block. A line that merely looks like a
// heading, such as `# install it` in indented code or in a list item, must not
// close a finding: what came after it would survive an untick and go to the PR.
func blockLines(body string, lines []string) (headings, literal, paragraphs map[int]bool) {
	starts := make([]int, len(lines))
	at := 0
	for i, l := range lines {
		starts[i] = at
		at += len(l)
	}
	lineOf := func(offset int) int {
		i, found := slices.BinarySearch(starts, offset)
		if !found {
			i--
		}
		return i
	}
	headings, literal, paragraphs = map[int]bool{}, map[int]bool{}, map[int]bool{}
	src := []byte(body)
	markLines := func(seg *text.Segments) {
		for i := 0; i < seg.Len(); i++ {
			s := seg.At(i)
			for l := lineOf(s.Start); l >= 0 && l < len(lines) && starts[l] < max(s.Stop, s.Start+1); l++ {
				literal[l] = true
			}
		}
	}
	doc := blockParser.Parse(text.NewReader(src))
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch b := n.(type) {
		case *ast.Paragraph:
			// Every line of a paragraph at the top: `**2. …**` right under
			// `**1. …**`, with no blank line, is markdown's same paragraph and
			// still the next finding. In a list item, only its first line.
			if !topLevel(b, doc) {
				break
			}
			for i := 0; i < b.Lines().Len(); i++ {
				if i == 0 || b.Parent() == doc {
					paragraphs[lineOf(b.Lines().At(i).Start)] = true
				}
			}
		case *ast.Heading:
			if b.Parent() == doc && b.Lines().Len() > 0 {
				headings[lineOf(b.Lines().At(0).Start)] = true
			}
		case *ast.CodeBlock:
			markLines(b.Lines())
		case *ast.FencedCodeBlock:
			markLines(b.Lines())
		case *ast.HTMLBlock:
			// The comment review-fleet writes under a heading is read on its own.
			if b.Lines().Len() > 1 {
				markLines(b.Lines())
				if b.HasClosure() {
					literal[lineOf(b.ClosureLine.Start)] = true
				}
			}
		}
		return ast.WalkContinue, nil
	})
	return headings, literal, paragraphs
}

// topLevel says n is a block of the document, or the text of a top-level list
// item: where a finding can start. Deeper down it is part of the one around it.
func topLevel(n, doc ast.Node) bool {
	p := n.Parent()
	if p == doc {
		return true
	}
	if _, item := p.(*ast.ListItem); !item || p.FirstChild() != n {
		return false
	}
	return p.Parent().Parent() == doc
}

// Findings counts the findings in a review.
func Findings(body string) int {
	n := 0
	for _, p := range SplitFindings(body) {
		if p.Finding {
			n++
		}
	}
	return n
}

// DropFindings returns the review without the findings whose index is in
// skip — the numbering is SplitFindings', and it is the one the page uses on
// the checkboxes. An out-of-range index is ignored.
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

// SavePublishCopy writes, next to the saved review, the version that goes to
// the PR: the same header, but the body already without the unticked findings.
// It lives in publish/ inside the reviews directory, so it stays out of the
// saved list — the real review stays whole, only what goes out changes.
//
// unticked says a person took findings out: the publisher has to know, because
// what is left is not what the agent concluded from, and a clean copy is no clean
// review.
func SavePublishCopy(dir, original, body string, unticked bool) (string, error) {
	pubDir := filepath.Join(dir, "publish")
	if err := os.MkdirAll(pubDir, 0o700); err != nil {
		return "", err
	}
	header := ""
	if data, err := os.ReadFile(original); err == nil {
		if head, _, ok := strings.Cut(string(data), "\n---\n"); ok {
			if unticked {
				head = strings.TrimRight(head, "\n") + "\n- Trimmed: yes\n"
			}
			header = head + "\n---\n\n"
		}
	}
	// One file per publish, never shared: two publishes of the same review with
	// different selections must not overwrite each other's copy before the post
	// agent reads it. CreateTemp picks a name nobody else has (O_EXCL).
	base := filepath.Base(original)
	ext := filepath.Ext(base)
	f, err := os.CreateTemp(pubDir, strings.TrimSuffix(base, ext)+"-*"+ext)
	if err != nil {
		return "", err
	}
	path := f.Name()
	_, err = f.WriteString(header + strings.TrimSpace(body) + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// boldNumbered recognizes the line that opens a bold numbered finding:
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

// headingOf returns the level of an ATX heading and the title, or 0 if the
// line is not a heading.
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

// isFindingsSection recognizes the `##` that group findings — "Findings",
// "Cuts (lazy-senior-dev)", "Cuts — `lazy-senior-dev`" — by the start of the
// title, because the rest varies from review to review.
func isFindingsSection(title string) bool {
	t := strings.ToLower(strings.TrimLeft(title, "0123456789. "))
	return strings.HasPrefix(t, "finding") || strings.HasPrefix(t, "cut")
}

// isHumanSection recognizes the `## Needs human verification` the fleet
// writes with what the lenses could not confirm.
func isHumanSection(title string) bool {
	t := strings.ToLower(strings.TrimLeft(title, "0123456789. "))
	return strings.HasPrefix(t, "needs human")
}

// listItem says whether the line opens a list item: `- `, `* `, `+ ` or `1. `.
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

// emptyItem says whether the list item only warns that there is nothing —
// "- None.", "- n/a", "- Nenhum" —, which the doubts section often carries
// when empty.
func emptyItem(trim string) bool {
	text := trim
	if i := strings.IndexByte(trim, ' '); i >= 0 {
		text = trim[i+1:]
	}
	text = strings.ToLower(strings.Trim(text, " .*_!"))
	switch text {
	case "none", "nothing", "n/a", "na", "nenhum", "nenhuma", "nada", "-", "—":
		return true
	}
	return false
}

// Unwrap takes the review out of a ```markdown block, when the agent pasted
// the whole report into a code block instead of returning it loose. Inside the
// block the headings become text: the page shows everything as code, GitHub
// does too, and no finding has anywhere to get a checkbox. It only unwraps
// when the block has a heading — a markdown snippet quoted in a finding, which
// has none, stays as is. The report's inner fences (```go…```) stay paired;
// all that goes is the outer fence and its closing.
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
