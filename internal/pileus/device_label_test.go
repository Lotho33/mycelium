package pileus

import (
	"testing"
	"unicode/utf8"
)

func TestSanitizeDeviceLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  TV salotto  ", "TV salotto"},
		{"", ""},
		{"a\tb\nc\rd", "abcd"},
		{"normal name", "normal name"},
	}
	for _, c := range cases {
		if got := sanitizeDeviceLabel(c.in); got != c.want {
			t.Errorf("sanitizeDeviceLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// capped (approximately, by byte length — the check runs after WriteRune
	// so a multi-byte rune can push it a few bytes over) and always valid UTF-8.
	long := ""
	for i := 0; i < 100; i++ {
		long += "à" // 2-byte UTF-8 rune
	}
	got := sanitizeDeviceLabel(long)
	if len(got) > 70 {
		t.Errorf("sanitizeDeviceLabel didn't cap: %d bytes", len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("sanitizeDeviceLabel produced invalid UTF-8: %q", got)
	}
}
