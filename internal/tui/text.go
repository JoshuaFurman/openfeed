package tui

import (
	"strings"
	"unicode"
)

// Sanitize neutralizes characters that could drive the terminal or reorder displayed text.
// Apply it to every string written to a terminal, not only feed text.
func Sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.Is(unicode.Bidi_Control, r):
			return -1
		case unicode.IsControl(r), unicode.In(r, unicode.Zl, unicode.Zp):
			return ' '
		}
		return r
	}, s)
}
