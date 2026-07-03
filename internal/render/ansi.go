package render

import (
	"html/template"
	"strconv"
	"strings"
)

// ansiFG maps SGR foreground color codes (standard 30–37, bright 90–97) to CSS
// colors. Backgrounds and 256/truecolor are intentionally unsupported — CI logs
// use the basic palette, and a fixed table keeps the output injection-proof (the
// color never comes from the log).
var ansiFG = map[int]string{
	30: "#555555", 31: "#c0392b", 32: "#27ae60", 33: "#b7950b",
	34: "#2980b9", 35: "#8e44ad", 36: "#17a2a2", 37: "#aaaaaa",
	90: "#7f8c8d", 91: "#e74c3c", 92: "#2ecc71", 93: "#f1c40f",
	94: "#3498db", 95: "#9b59b6", 96: "#1abc9c", 97: "#ecf0f1",
}

// ANSI converts a byte stream carrying ANSI SGR escapes into HTML safe to embed
// in a <pre>. All text is HTML-escaped; SGR color/bold escapes become <span>
// wrappers with styles drawn only from the fixed palette; every other escape
// sequence (cursor moves, etc.) is dropped. Because the styles are ours and the
// text is escaped, the untrusted log can't inject markup.
func ANSI(b []byte) template.HTML {
	var out strings.Builder
	color := ""
	bold := false

	flush := func(s []byte) {
		if len(s) == 0 {
			return
		}
		esc := template.HTMLEscapeString(string(s))
		if color == "" && !bold {
			out.WriteString(esc)
			return
		}
		out.WriteString(`<span style="`)
		if bold {
			out.WriteString("font-weight:bold;")
		}
		if color != "" {
			out.WriteString("color:")
			out.WriteString(color)
			out.WriteString(";")
		}
		out.WriteString(`">`)
		out.WriteString(esc)
		out.WriteString("</span>")
	}

	i, n, start := 0, len(b), 0
	for i < n {
		if b[i] != 0x1b { // ESC
			i++
			continue
		}
		flush(b[start:i])
		if i+1 < n && b[i+1] == '[' { // CSI: ESC [ params <final>
			j := i + 2
			for j < n && (b[j] < 0x40 || b[j] > 0x7e) {
				j++
			}
			if j >= n { // unterminated — drop the rest
				i = n
			} else {
				if b[j] == 'm' {
					applySGR(string(b[i+2:j]), &color, &bold)
				}
				i = j + 1
			}
		} else {
			i++ // lone ESC — skip it
		}
		start = i
	}
	flush(b[start:n])
	return template.HTML(out.String())
}

// applySGR updates the current color/bold from a semicolon-separated SGR
// parameter list. Unknown codes are ignored.
func applySGR(params string, color *string, bold *bool) {
	if params == "" { // ESC[m is a reset
		*color, *bold = "", false
		return
	}
	for _, p := range strings.Split(params, ";") {
		code, err := strconv.Atoi(p)
		if err != nil {
			continue
		}
		switch code {
		case 0:
			*color, *bold = "", false
		case 1:
			*bold = true
		case 22:
			*bold = false
		case 39:
			*color = ""
		default:
			if c, ok := ansiFG[code]; ok {
				*color = c
			}
		}
	}
}
