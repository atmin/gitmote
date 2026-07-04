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
	// Class-based output: keywords wrapped in class spans, no inline styles.
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
	// A dark-mode override ships alongside the light theme.
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
	// Fenced code highlighted via goldmark-highlighting → chroma class spans.
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

func TestMarkdownMermaid(t *testing.T) {
	src := []byte("# Diagram\n\n```mermaid\ngraph TD\n  A --> B\n```\n")
	out := string(Markdown(src))
	// The fence becomes a mermaid container (client-side render), not chroma output.
	if !strings.Contains(out, `class="mermaid"`) {
		t.Fatalf("mermaid block not emitted as a mermaid container:\n%s", out)
	}
	// The diagram source survives inside it (HTML-escaped: --> becomes --&gt;).
	if !strings.Contains(out, "graph TD") || !strings.Contains(out, "--&gt;") {
		t.Fatalf("diagram source missing/unescaped:\n%s", out)
	}
	// No script is emitted here — the layout includes our own embedded one, and the
	// sanitizer would strip any injected <script> regardless.
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
