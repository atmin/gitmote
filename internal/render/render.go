// Package render turns untrusted repo content into safe HTML for the browse
// UI. It renders syntax-highlighted source (chroma) and markdown (goldmark).
// It is deliberately separate from webui, so the security-critical rendering
// is unit-testable in isolation. Every byte here comes from a repo, and a
// browser shows it to whoever may read that repo.
//
// Two defenses guard the markdown path:
//   - goldmark escapes raw HTML by default (WithUnsafe is never set).
//   - The UGCPolicy of bluemonday strips what slips through: scripts,
//     event-handler attributes, and javascript: or data: URLs.
//
// The class-based output of chroma is static markup with no scripts. Its
// stylesheet is generated, not derived from the repo. So both are safe to
// embed.
package render

import (
	"bytes"
	"html/template"
	"net/url"
	"path"
	"strings"
	"sync"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"

	highlighting "github.com/yuin/goldmark-highlighting/v2"
	mermaid "go.abhg.dev/goldmark/mermaid"
)

// MaxSize caps the input that render will highlight or render as markdown.
// Above it, the caller falls back to a plain <pre>. A minified or generated
// blob shouldn't turn into pathological HTML.
const MaxSize = 512 * 1024

// highlightStyle is the chroma theme that the blob view and README code blocks
// share, so the two match. The class-based output pairs it with HighlightCSS.
// darkStyle is its dark-mode counterpart, emitted under a prefers-color-scheme
// media query. It uses the same class names. So it overrides highlightStyle
// when the OS of the viewer is in dark mode.
var (
	highlightStyle = styles.Get("github")
	darkStyle      = styles.Get("github-dark")
)

// classFormatter emits class-based highlight markup, not inline styles. So a
// single stylesheet from HighlightCSS covers every page.
var classFormatter = chromahtml.New(chromahtml.WithClasses(true), chromahtml.TabWidth(4))

// Highlight renders code as syntax-highlighted, class-based HTML, wrapped in
// the <pre> of the formatter. It chooses the lexer by filename first, then by
// content analysis, and then falls back to plain text. Highlighting is
// best-effort. On any lexer or format error, the caller falls back to a plain
// <pre>.
func Highlight(code []byte, filename string) (template.HTML, error) {
	lexer := lexers.Match(path.Base(filename))
	if lexer == nil {
		lexer = lexers.Analyse(string(code))
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	it, err := lexer.Tokenise(nil, string(code))
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := classFormatter.Format(&buf, highlightStyle, it); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil //nolint:gosec // chroma class output is static markup
}

var highlightCSS struct {
	sync.Once
	css template.CSS
}

// HighlightCSS is the stylesheet paired with the class-based output of
// Highlight. The layout includes it once. chroma generates it from a fixed
// theme, so it is trusted and not derived from the repo. It is cached after
// the first build.
func HighlightCSS() template.CSS {
	highlightCSS.Do(func() {
		var buf bytes.Buffer
		_ = classFormatter.WriteCSS(&buf, highlightStyle)
		// The dark theme reuses the same classes, wrapped in a media query. So it
		// takes effect only when the OS of the viewer is in dark mode.
		buf.WriteString("@media (prefers-color-scheme: dark) {\n")
		_ = classFormatter.WriteCSS(&buf, darkStyle)
		buf.WriteString("}\n")
		highlightCSS.css = template.CSS(buf.String())
	})
	return highlightCSS.css
}

// markdown renders CommonMark and GFM. Raw HTML is escaped. That is the
// default of goldmark, and WithUnsafe is intentionally unset. Fenced code goes
// through chroma, with the same class-based theme as the blob view.
var markdown = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM,
		highlighting.NewHighlighting(
			highlighting.WithFormatOptions(chromahtml.WithClasses(true)),
		),
		// A ```mermaid fence becomes <pre class="mermaid">…escaped source…</pre>
		// for rendering on the client. bluemonday passes the class, which is
		// already allowed on <pre>. RenderModeClient is explicit, so a stray
		// `mmdc` on PATH can't switch rendering to the server. NoScript is set
		// because the layout includes its own embedded, trusted mermaid.js. The
		// sanitizer would strip the CDN <script> of the extension anyway.
		&mermaid.Extender{RenderMode: mermaid.RenderModeClient, NoScript: true},
	),
	// Rewrite the relative links and embeds of a rendered file into repo URLs.
	// The transformer is a no-op unless the Convert call supplies a
	// *LinkContext. MarkdownLinks supplies one, and plain Markdown does not.
	goldmark.WithParserOptions(
		parser.WithASTTransformers(util.Prioritized(linkTransformer{}, 100)),
	),
)

// linkCtxKey carries the *LinkContext of one render through the parser
// context to the link transformer.
var linkCtxKey = parser.NewContextKey()

// LinkContext supplies what the markdown link rewriter needs. The rewriter
// turns the relative links and embeds of a rendered file into repo URLs. It
// needs these:
//   - the repo name
//   - the resolved ref
//   - the directory of the rendered file, which relative paths resolve against
//   - a Stat callback, which reports whether a path relative to the repo is a
//     "blob", a "tree", or absent (ok=false)
type LinkContext struct {
	Repo string
	Ref  string
	Dir  string
	Stat func(path string) (kind string, ok bool)
}

// rewrite maps one link or image destination to a repo URL, or returns it
// unchanged. It leaves these targets alone:
//   - external targets (with a scheme or host)
//   - site-absolute targets
//   - pure-anchor targets
//
// It resolves a relative path against Dir. It maps an image to raw. It maps a
// navigation link to blob or tree, by the Stat lookup. Some targets stay
// unchanged and visibly dangling, not rewritten into a wrong URL:
//   - a relative path that climbs out of the repo
//   - a navigation target that doesn't exist
func (lc *LinkContext) rewrite(dest string, isImage bool) string {
	u, err := url.Parse(dest)
	if err != nil {
		return dest
	}
	if u.Scheme != "" || u.Host != "" || u.Path == "" || path.IsAbs(u.Path) {
		return dest
	}
	target := path.Join(lc.Dir, u.Path)
	if target == ".." || strings.HasPrefix(target, "../") {
		return dest
	}
	verb := "raw"
	if !isImage {
		kind, ok := lc.Stat(target)
		if !ok {
			return dest
		}
		if kind == "tree" {
			verb = "tree"
		} else {
			verb = "blob"
		}
	}
	out := "/" + lc.Repo + "/" + verb + "/" + lc.Ref + "/" + target
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		out += "#" + u.Fragment
	}
	return out
}

// linkTransformer rewrites relative link and image destinations in the parsed
// markdown when the Convert call supplies a *LinkContext.
type linkTransformer struct{}

func (linkTransformer) Transform(doc *ast.Document, _ text.Reader, pc parser.Context) {
	lc, ok := pc.Get(linkCtxKey).(*LinkContext)
	if !ok || lc == nil {
		return
	}
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node := n.(type) {
		case *ast.Link:
			node.Destination = []byte(lc.rewrite(string(node.Destination), false))
		case *ast.Image:
			node.Destination = []byte(lc.rewrite(string(node.Destination), true))
		}
		return ast.WalkContinue, nil
	})
}

// sanitizer strips the dangerous constructs that goldmark passes through. It
// allows the class attribute that chroma needs on the code elements it emits,
// so README code stays highlighted. UGCPolicy already forbids scripts, event
// handlers, and the javascript: and data: URL schemes.
var sanitizer = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("class").OnElements("div", "pre", "code", "span")
	return p
}()

// Markdown renders src to sanitized HTML. It never returns unsafe markup. Even
// if goldmark returns an error, the empty output passes through the sanitizer.
func Markdown(src []byte) template.HTML {
	return MarkdownLinks(src, nil)
}

// MarkdownLinks renders src like Markdown. It also rewrites the relative links
// and embeds of the file into repo URLs through lc (see LinkContext). A nil lc
// disables the rewrite, so the function is then exactly Markdown.
func MarkdownLinks(src []byte, lc *LinkContext) template.HTML {
	var buf bytes.Buffer
	pc := parser.NewContext()
	if lc != nil {
		pc.Set(linkCtxKey, lc)
	}
	if err := markdown.Convert(src, &buf, parser.WithContext(pc)); err != nil {
		return ""
	}
	return template.HTML(sanitizer.SanitizeBytes(buf.Bytes())) //nolint:gosec // sanitized above
}

// Diff renders a unified diff (the patch text of git) to HTML. It gives one
// styled row per line, so the browse UI shows a colored diff instead of a
// plain <pre>. A line is one of these:
//   - a file header or a hunk header
//   - an addition
//   - a deletion
//   - context
//
// The input is the output of git itself, a trusted origin. But every line is
// HTML-escaped, so the result is safe to embed. An empty diff gives "", and
// the caller shows nothing.
func Diff(unified string) template.HTML {
	if strings.TrimSpace(unified) == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<div class="diff">`)
	lines := strings.Split(unified, "\n")
	// A trailing newline splits into a final empty element. Drop it, so it
	// doesn't render as a stray blank row.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	for _, line := range lines {
		b.WriteString(`<div class="dl `)
		b.WriteString(diffLineClass(line))
		b.WriteString(`">`)
		// An empty line still needs height. A zero-width space keeps the row.
		if line == "" {
			b.WriteString("&#8203;")
		} else {
			b.WriteString(template.HTMLEscapeString(line))
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`)
	return template.HTML(b.String()) //nolint:gosec // every line is HTML-escaped above
}

// diffLineClass maps a unified-diff line to its CSS row class by the leading
// marker. It checks the file-level headers before the bare + or -. So a header
// is never miscolored as an addition or a deletion. File-level headers include
// `diff --git`, `+++`, `---`, and the mode and rename lines.
func diffLineClass(line string) string {
	switch {
	case strings.HasPrefix(line, "@@"):
		return "dl-hunk"
	case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"),
		strings.HasPrefix(line, "diff "), strings.HasPrefix(line, "index "),
		strings.HasPrefix(line, "new file"), strings.HasPrefix(line, "deleted file"),
		strings.HasPrefix(line, "old mode"), strings.HasPrefix(line, "new mode"),
		strings.HasPrefix(line, "rename "), strings.HasPrefix(line, "similarity "),
		strings.HasPrefix(line, "copy "), strings.HasPrefix(line, "Binary files"):
		return "dl-file"
	case strings.HasPrefix(line, "+"):
		return "dl-add"
	case strings.HasPrefix(line, "-"):
		return "dl-del"
	default: // context, "\ No newline at end of file", blank
		return "dl-ctx"
	}
}

// HasMermaid reports whether rendered markdown contains a mermaid diagram block
// (emitted as <pre class="mermaid">). So a page pulls in the mermaid script
// only when it has a diagram. The renderer HTML-escapes repo text that
// literally reads class="mermaid", so that text can't trigger this.
func HasMermaid(h template.HTML) bool {
	return strings.Contains(string(h), `class="mermaid"`)
}

// IsMarkdown reports whether filename names a markdown file (.md or
// .markdown, case-insensitive).
func IsMarkdown(filename string) bool {
	switch strings.ToLower(path.Ext(filename)) {
	case ".md", ".markdown":
		return true
	default:
		return false
	}
}

// IsReadme reports whether name is a README markdown file (README.md or
// README.markdown, case-insensitive). That is the file rendered below a tree
// listing.
func IsReadme(name string) bool {
	lower := strings.ToLower(name)
	return lower == "readme.md" || lower == "readme.markdown"
}
