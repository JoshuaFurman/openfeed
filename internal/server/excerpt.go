package server

import (
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// excerptRunes caps an excerpt; the page clamps it to a few lines anyway.
const excerptRunes = 280

// excerpt is the plain text of feed-provided content, whitespace collapsed and cut to excerptRunes.
// Markup is discarded, never interpreted, so the result is safe to escape into a page.
func excerpt(content string) string {
	var b strings.Builder
	runes, space, skip := 0, false, 0
	z := html.NewTokenizer(strings.NewReader(content))
	for runes <= excerptRunes {
		switch kind := z.Next(); kind {
		case html.ErrorToken:
			return strings.TrimSpace(b.String())
		case html.StartTagToken, html.EndTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			a := atom.Lookup(name)
			if hidden(a) && kind == html.StartTagToken {
				skip++
			} else if hidden(a) && kind == html.EndTagToken && skip > 0 {
				skip--
			}
			space = space || !inline(a) // Inline markup like <b> must not split "Go</b>." into "Go .".
		case html.TextToken:
			if skip > 0 {
				continue
			}
			for _, r := range string(z.Text()) {
				if unicode.IsSpace(r) || unicode.IsControl(r) {
					space = true
					continue
				}
				if space && b.Len() > 0 {
					b.WriteByte(' ')
					runes++
				}
				space = false
				b.WriteRune(r)
				if runes++; runes > excerptRunes {
					break
				}
			}
		}
	}
	return strings.TrimSpace(string([]rune(b.String())[:excerptRunes])) + "…"
}

// inline reports phrasing elements, which continue the surrounding text rather than break it.
func inline(a atom.Atom) bool {
	switch a {
	case atom.A, atom.Abbr, atom.B, atom.Bdi, atom.Bdo, atom.Cite, atom.Code, atom.Data, atom.Dfn, atom.Em, atom.Font,
		atom.I, atom.Kbd, atom.Mark, atom.Q, atom.S, atom.Samp, atom.Small, atom.Span, atom.Strike, atom.Strong,
		atom.Sub, atom.Sup, atom.Time, atom.Tt, atom.U, atom.Var, atom.Wbr, atom.Del, atom.Ins:
		return true
	}
	return false
}

// hidden reports elements whose text is not prose a reader would see.
func hidden(a atom.Atom) bool {
	switch a {
	case atom.Script, atom.Style, atom.Noscript, atom.Template, atom.Head, atom.Title, atom.Svg, atom.Math, atom.Iframe, atom.Object:
		return true
	}
	return false
}
