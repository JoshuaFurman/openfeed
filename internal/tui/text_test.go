package tui

import "testing"

func TestSanitize(t *testing.T) {
	for in, want := range map[string]string{
		"plain ASCII":                     "plain ASCII",
		"日本語 and emoji 👩\u200d💻":          "日本語 and emoji 👩\u200d💻",
		"clear\x1b[2Jscreen":              "clear [2Jscreen",
		"bell\a tab\t newline\n C1\u009b": "bell  tab  newline  C1 ",
		"evil\u202eexe.txt\u2066":         "evilexe.txt",
		"line\u2028para\u2029end":         "line para end",
	} {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}
