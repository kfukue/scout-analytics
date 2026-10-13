package main

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// splitMessage cuts at the last newline within the limit, otherwise at a rune
// boundary (never inside an emoji), never over the limit, with no empty chunk,
// and the chunks join back to the text exactly.
func TestSplitMessage(t *testing.T) {
	const rocket = "🚀" // 4 bytes
	const check = "✅"  // 3 bytes
	a := func(n int) string { return strings.Repeat("a", n) }
	cases := []struct {
		name  string
		text  string
		limit int
		want  []string // nil: check the invariants only
	}{
		{"empty", "", 10, []string{}},
		{"under the limit", "hello", 10, []string{"hello"}},
		{"exactly the limit", a(10), 10, []string{a(10)}},
		{"one over, no newline", a(11), 10, []string{a(10), "a"}},
		{"at a newline", "aaaa\nbbbbbbbb", 10, []string{"aaaa\n", "bbbbbbbb"}},
		{"the last newline within the limit", "aa\nbb\ncccc\ndddd", 10, []string{"aa\nbb\n", "cccc\ndddd"}},
		{"newline as the last byte within the limit", a(9) + "\n" + "bbb", 10, []string{a(9) + "\n", "bbb"}},
		{"newline right after the limit", a(10) + "\nbb", 10, []string{a(10), "\nbb"}},
		{"newline only at the start: no chunk of just a newline", "\n" + a(12), 10, []string{"\n" + a(9), a(3)}},
		{"emoji across the limit", a(8) + rocket + "b", 10, []string{a(8), rocket + "b"}},
		{"emoji ending at the limit", a(6) + rocket + "b", 10, []string{a(6) + rocket, "b"}},
		{"emoji starting at the limit", a(10) + rocket, 10, []string{a(10), rocket}},
		{"3-byte rune across the limit", a(9) + check + "b", 10, []string{a(9), check + "b"}},
		{"emoji across the limit, newline before it", "aa\n" + a(6) + rocket, 10, []string{"aa\n", a(6) + rocket}},
		{"only emoji", strings.Repeat(rocket, 5), 10, []string{rocket + rocket, rocket + rocket, rocket}},
		{"botAPILimit, emoji at the boundary", a(botAPILimit-2) + rocket + "tail", botAPILimit,
			[]string{a(botAPILimit - 2), rocket + "tail"}},
		{"botAPILimit, long message with lines and emoji",
			strings.Repeat("✅ $SNIFF No red flags found 🔁 re-checked\n", 300), botAPILimit, nil},
		{"invalid UTF-8: a byte cut is allowed", strings.Repeat("\x80", 25), 10,
			[]string{strings.Repeat("\x80", 10), strings.Repeat("\x80", 10), strings.Repeat("\x80", 5)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitMessage(c.text, c.limit)
			if c.want != nil && !slices.Equal(got, c.want) {
				t.Errorf("splitMessage(%q, %d) = %q, want %q", c.text, c.limit, got, c.want)
			}
			if j := strings.Join(got, ""); j != c.text {
				t.Errorf("splitMessage(%q, %d): chunks join to %q, want the text", c.text, c.limit, j)
			}
			for i, ch := range got {
				if ch == "" || len(ch) > c.limit {
					t.Errorf("splitMessage(%q, %d): chunk %d is %d bytes, want 1 to %d", c.text, c.limit, i, len(ch), c.limit)
				}
				if utf8.ValidString(c.text) && !utf8.ValidString(ch) {
					t.Errorf("splitMessage(%q, %d): chunk %d %q cuts a rune", c.text, c.limit, i, ch)
				}
			}
		})
	}
}
