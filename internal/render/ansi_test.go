package render

import (
	"strings"
	"testing"
)

func TestANSIPlainText(t *testing.T) {
	got := string(ANSI([]byte("hello world\n")))
	if got != "hello world\n" {
		t.Errorf("plain text = %q, want unchanged", got)
	}
}

func TestANSIColorBecomesSpan(t *testing.T) {
	// red "err" then reset.
	got := string(ANSI([]byte("\x1b[31merr\x1b[0m ok")))
	if !strings.Contains(got, "<span style=\"color:#c0392b;\">err</span>") {
		t.Errorf("colored run not wrapped: %q", got)
	}
	if !strings.HasSuffix(got, " ok") {
		t.Errorf("text after reset not plain: %q", got)
	}
}

func TestANSIBold(t *testing.T) {
	got := string(ANSI([]byte("\x1b[1mB\x1b[0m")))
	if !strings.Contains(got, "font-weight:bold;") || !strings.Contains(got, ">B</span>") {
		t.Errorf("bold not applied: %q", got)
	}
}

func TestANSIEscapesHTML(t *testing.T) {
	// Untrusted log content must be escaped — no raw tags reach the output.
	got := string(ANSI([]byte("<script>alert(1)</script>")))
	if strings.Contains(got, "<script>") {
		t.Errorf("HTML not escaped (injection): %q", got)
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Errorf("expected escaped entities, got %q", got)
	}
}

func TestANSIColoredHTMLStillEscaped(t *testing.T) {
	// HTML inside a colored run is still escaped.
	got := string(ANSI([]byte("\x1b[32m<b>\x1b[0m")))
	if strings.Contains(got, "<b>") {
		t.Errorf("HTML inside colored run not escaped: %q", got)
	}
	if !strings.Contains(got, "&lt;b&gt;") {
		t.Errorf("want escaped entities inside span: %q", got)
	}
}

func TestANSIDropsNonSGREscapes(t *testing.T) {
	// A cursor-move CSI (ends in 'H') and an erase (ends in 'K') are dropped, not
	// rendered as bytes.
	got := string(ANSI([]byte("a\x1b[2Kb\x1b[1;1Hc")))
	if got != "abc" {
		t.Errorf("non-SGR escapes not dropped: %q", got)
	}
}

func TestANSIUnterminatedEscape(t *testing.T) {
	// A trailing, unterminated escape must not panic or leak raw bytes.
	got := string(ANSI([]byte("ok\x1b[")))
	if got != "ok" {
		t.Errorf("unterminated escape = %q, want %q", got, "ok")
	}
}
