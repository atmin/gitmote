package render

import (
	"strings"
	"testing"
)

func TestHighlightByExtension(t *testing.T) {
	out, err := Highlight([]byte("package main\n\nfunc main() {}\n"), "main.go")
	if err != nil {
		t.Fatalf("Highlight: %v", err)
	}
	s := string(out)
	// Class-based output wraps keywords in class spans, with no inline styles.
	if !strings.Contains(s, "<pre") || !strings.Contains(s, `class="`) {
		t.Fatalf("go highlight missing class spans:\n%s", s)
	}
	if strings.Contains(s, "style=") {
		t.Fatalf("expected class-based output, got inline styles:\n%s", s)
	}
}

func TestHighlightUnknownExtensionFallsBack(t *testing.T) {
	out, err := Highlight([]byte("just some text\n"), "notes.zzz")
	if err != nil {
		t.Fatalf("Highlight: %v", err)
	}
	if !strings.Contains(string(out), "just some text") {
		t.Fatalf("fallback dropped content:\n%s", out)
	}
}

func TestHighlightEmpty(t *testing.T) {
	out, err := Highlight(nil, "empty.go")
	if err != nil {
		t.Fatalf("Highlight(empty): %v", err)
	}
	if !strings.Contains(string(out), "<pre") {
		t.Fatalf("empty input produced no <pre>:\n%s", out)
	}
}

func TestHighlightCSS(t *testing.T) {
	css := string(HighlightCSS())
	if css == "" || !strings.Contains(css, ".chroma") {
		t.Fatalf("HighlightCSS looks wrong:\n%s", css)
	}
	// A dark-mode override comes with the light theme.
	if !strings.Contains(css, "prefers-color-scheme: dark") {
		t.Fatalf("HighlightCSS missing dark-mode variant:\n%s", css)
	}
}

func TestMarkdownGFM(t *testing.T) {
	src := []byte("# Title\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n```go\nfunc main() {}\n```\n")
	out := string(Markdown(src))
	if !strings.Contains(out, "<h1") {
		t.Fatalf("heading not rendered:\n%s", out)
	}
	if !strings.Contains(out, "<table") {
		t.Fatalf("GFM table not rendered:\n%s", out)
	}
	// goldmark-highlighting highlights fenced code into chroma class spans.
	if !strings.Contains(out, `class="`) {
		t.Fatalf("fenced code not highlighted:\n%s", out)
	}
}

func TestMarkdownSanitizesXSS(t *testing.T) {
	src := []byte("# Hi\n\n<script>alert(1)</script>\n\n<img src=x onerror=alert(1)>\n\n[click](javascript:alert(1))\n")
	out := string(Markdown(src))
	if strings.Contains(out, "<script") {
		t.Fatalf("script tag survived sanitization:\n%s", out)
	}
	if strings.Contains(out, "onerror") {
		t.Fatalf("event handler survived sanitization:\n%s", out)
	}
	if strings.Contains(strings.ToLower(out), "javascript:") {
		t.Fatalf("javascript: URL survived sanitization:\n%s", out)
	}
}

func TestMarkdownLinks(t *testing.T) {
	// A stat callback. doc.md and CONTRIBUTING.md are files. "sub" and "docs"
	// are directories. Everything else is absent.
	lc := &LinkContext{
		Repo: "app",
		Ref:  "main",
		Dir:  "docs",
		Stat: func(p string) (string, bool) {
			switch p {
			case "docs/doc.md", "CONTRIBUTING.md":
				return "blob", true
			case "docs/sub", "sub":
				return "tree", true
			default:
				return "", false
			}
		},
	}

	src := []byte(strings.Join([]string{
		"[sib](./doc.md)",          // a sibling file maps to blob
		"[dir](sub/)",              // a subdirectory maps to tree
		"[up](../CONTRIBUTING.md)", // a ../ link up maps to blob at the repo root
		"![img](diagram.png)",      // an image maps to raw (no stat)
		"[ext](https://x.example)", // an external link stays unchanged
		"[mail](mailto:a@b.co)",    // a mailto link stays unchanged
		"[anchor](#section)",       // an in-page anchor stays unchanged
		"[abs](/other/thing)",      // a site-absolute link stays unchanged
		"[gone](./missing.md)",     // a missing target stays unchanged
	}, "\n\n"))
	out := string(MarkdownLinks(src, lc))

	wantContains := []string{
		`href="/app/blob/main/docs/doc.md"`,
		`href="/app/tree/main/docs/sub"`,
		`href="/app/blob/main/CONTRIBUTING.md"`,
		`src="/app/raw/main/docs/diagram.png"`,
		`href="https://x.example"`,
		`href="mailto:a@b.co"`,
		`href="#section"`,
		`href="/other/thing"`,
		`href="./missing.md"`, // dangling, left unchanged
	}
	for _, w := range wantContains {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q:\n%s", w, out)
		}
	}
	// A missing navigation target must never become a blob or tree URL.
	if strings.Contains(out, "missing.md\"") && strings.Contains(out, "/app/blob/main/docs/missing.md") {
		t.Errorf("missing target was rewritten into a false blob URL:\n%s", out)
	}
}

func TestMarkdownLinksNilContextIsInert(t *testing.T) {
	src := []byte("[x](./doc.md)\n\n![y](img.png)\n")
	if a, b := MarkdownLinks(src, nil), Markdown(src); string(a) != string(b) {
		t.Fatalf("nil LinkContext changed output:\n%s\nvs\n%s", a, b)
	}
	// Relative destinations pass through untouched with no context.
	if out := string(Markdown(src)); !strings.Contains(out, `href="./doc.md"`) || !strings.Contains(out, `src="img.png"`) {
		t.Fatalf("relative links altered without a context:\n%s", out)
	}
}

func TestDiff(t *testing.T) {
	unified := strings.Join([]string{
		"diff --git a/hello.go b/hello.go",
		"index 1111111..2222222 100644",
		"--- a/hello.go",
		"+++ b/hello.go",
		"@@ -1,3 +1,3 @@ func main() {",
		" context stays",
		"-func main() {}",
		"+func main() { println(1) }",
	}, "\n") + "\n"
	out := string(Diff(unified))

	for _, want := range []string{
		`class="diff"`,
		`class="dl dl-file">diff --git a/hello.go b/hello.go</div>`,
		`class="dl dl-file">--- a/hello.go</div>`, // --- is a file header, not a deletion
		`class="dl dl-file">+++ b/hello.go</div>`, // +++ is a file header, not an addition
		`class="dl dl-hunk">@@ -1,3 +1,3 @@`,
		`class="dl dl-ctx"> context stays</div>`,
		`class="dl dl-del">-func main() {}</div>`,
		`class="dl dl-add">+func main() { println(1) }</div>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diff HTML missing %q:\n%s", want, out)
		}
	}
	// The trailing newline must not produce a stray empty row.
	if strings.Contains(out, `<div class="dl dl-ctx">&#8203;</div></div>`) {
		t.Errorf("trailing newline rendered as a blank row:\n%s", out)
	}
}

func TestDiffEscapesAndEmpty(t *testing.T) {
	if got := Diff("   \n"); got != "" {
		t.Errorf("blank diff = %q, want empty", got)
	}
	// A diff line containing HTML must be escaped, never emitted as live markup.
	out := string(Diff("+<script>alert(1)</script>\n"))
	if strings.Contains(out, "<script>") {
		t.Fatalf("diff did not escape HTML:\n%s", out)
	}
	if !strings.Contains(out, `dl-add">+&lt;script&gt;`) {
		t.Fatalf("expected escaped addition line:\n%s", out)
	}
}

func TestMarkdownMermaid(t *testing.T) {
	src := []byte("# Diagram\n\n```mermaid\ngraph TD\n  A --> B\n```\n")
	out := string(Markdown(src))
	// The fence becomes a mermaid container, rendered on the client. It is not
	// chroma output.
	if !strings.Contains(out, `class="mermaid"`) {
		t.Fatalf("mermaid block not emitted as a mermaid container:\n%s", out)
	}
	// The diagram source stays inside it, HTML-escaped (--> becomes --&gt;).
	if !strings.Contains(out, "graph TD") || !strings.Contains(out, "--&gt;") {
		t.Fatalf("diagram source missing/unescaped:\n%s", out)
	}
	// No script is emitted here. The layout includes its own embedded script.
	// The sanitizer would strip any injected <script> in any case.
	if strings.Contains(out, "<script") {
		t.Fatalf("mermaid render leaked a <script>:\n%s", out)
	}
	if !HasMermaid(Markdown(src)) {
		t.Fatal("HasMermaid = false for markdown with a mermaid block")
	}
	// A literal class="mermaid" in prose is escaped, so HasMermaid stays false.
	if HasMermaid(Markdown([]byte("a `class=\"mermaid\"` mention\n"))) {
		t.Fatal("HasMermaid = true for an escaped literal, not a real diagram")
	}
	if HasMermaid(Markdown([]byte("# just text\n"))) {
		t.Fatal("HasMermaid = true for markdown with no diagram")
	}
}

func TestIsMarkdownAndReadme(t *testing.T) {
	for _, name := range []string{"a.md", "A.MARKDOWN", "docs/x.Md"} {
		if !IsMarkdown(name) {
			t.Fatalf("IsMarkdown(%q) = false", name)
		}
	}
	if IsMarkdown("main.go") {
		t.Fatal("IsMarkdown(main.go) = true")
	}
	for _, name := range []string{"README.md", "readme.markdown", "ReadMe.MD"} {
		if !IsReadme(name) {
			t.Fatalf("IsReadme(%q) = false", name)
		}
	}
	if IsReadme("readme.txt") || IsReadme("CONTRIBUTING.md") {
		t.Fatal("IsReadme matched a non-README")
	}
}
