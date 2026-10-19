package server

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestExcerpt(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
	}{
		{"empty", "", ""},
		{"plain text", "  Hello,\n\tworld  ", "Hello, world"},
		{"markup", "<p>One <b>bold</b> idea.</p><p>Two.</p>", "One bold idea. Two."},
		{"block boundaries separate words", "<li>first</li><li>second</li>", "first second"},
		{"inline markup joins", "Go <b>1.26</b>. <a href=x>Link</a>s and <code>x</code>!", "Go 1.26. Links and x!"},
		{"breaks and images separate", "one<br>two<img src=x>three<hr/>four", "one two three four"},
		{"unknown elements separate", "a<custom>b</custom>c", "a b c"},
		{"entities", "Fish &amp; chips &lt;3 &#x1F41F;", "Fish & chips <3 🐟"},
		{"hidden elements", "<style>p{}</style><script>alert(1)</script>Shown<noscript>no</noscript><svg><text>x</text></svg>", "Shown"},
		{"unclosed script hides the rest", "Before<script>steal()", "Before"},
		{"attributes are not text", `<img alt="alt text" src="x" onerror="alert(1)"><a href="javascript:x">link</a>`, "link"},
		{"escaped markup stays text", "&lt;script&gt;alert(1)&lt;/script&gt;", "<script>alert(1)</script>"},
		{"comments", "a<!-- hidden -->b", "ab"},
		{"control characters", "a\x1b[2Jb\u0085c", "a [2Jb c"},
		{"broken markup", "<p <b>text", "text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := excerpt(tc.content); got != tc.want {
				t.Errorf("excerpt(%q) = %q, want %q", tc.content, got, tc.want)
			}
		})
	}
}

func TestExcerptTruncates(t *testing.T) {
	exact := strings.Repeat("é", excerptRunes)
	if got := excerpt(exact); got != exact {
		t.Errorf("an excerpt of exactly %d runes was cut to %q", excerptRunes, got)
	}
	long := strings.Repeat("word ", 1000)
	got := excerpt("<p>" + long + "</p>")
	if !strings.HasSuffix(got, "…") || utf8.RuneCountInString(got) > excerptRunes+1 || !strings.HasPrefix(got, "word word") {
		t.Errorf("long excerpt = %q (%d runes)", got, utf8.RuneCountInString(got))
	}
	if strings.HasSuffix(strings.TrimSuffix(got, "…"), " ") {
		t.Errorf("excerpt ends in a space before the ellipsis: %q", got)
	}
}
