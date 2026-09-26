package mattermost

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHumanSize(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024*1024 - 1, "1.0 MB"},
		{1024 * 1024, "1.0 MB"},
		{1258291, "1.2 MB"},
		{100*1024*1024 + 1, "100.0 MB"},
		{1024*1024*1024 - 1, "1.0 GB"},
		{5 * 1024 * 1024 * 1024, "5.0 GB"},
		{1024 * 1024 * 1024 * 1024, "1.0 TB"},
		{2048 * 1024 * 1024 * 1024 * 1024, "2048.0 TB"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := HumanSize(tt.bytes); got != tt.want {
				t.Errorf("HumanSize(%d) = %q, want %q", tt.bytes, got, tt.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	marker := "\n**[truncated at 500 chars — " + FullHint + "]**"
	tests := []struct {
		name, body, hint string
		limit            int
		want             string
	}{
		{"empty", "", FullHint, MaxBodyChars, ""},
		{"at limit", strings.Repeat("a", 500), FullHint, MaxBodyChars, strings.Repeat("a", 500)},
		{"over limit", strings.Repeat("a", 501), FullHint, MaxBodyChars, strings.Repeat("a", 500) + marker},
		{"runes not bytes", strings.Repeat("я", 500), FullHint, MaxBodyChars, strings.Repeat("я", 500)},
		{"multibyte cut", strings.Repeat("я", 501), FullHint, MaxBodyChars, strings.Repeat("я", 500) + marker},
		{"custom hint", "abcdef", "response cut", 3, "abc\n**[truncated at 3 chars — response cut]**"},
		{"zero limit", "ab", "h", 0, "\n**[truncated at 0 chars — h]**"},
		{"invalid bytes", "a\xffb\xfec", "h", 4, "a\uFFFDb\uFFFD\n**[truncated at 4 chars — h]**"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Truncate(tt.body, tt.hint, tt.limit); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestTruncateMatchesRuneCut checks the byte-offset cut against the []rune cut it replaces, for
// mixed-width text cut on every rune around the limit.
func TestTruncateMatchesRuneCut(t *testing.T) {
	runeCut := func(body, hint string, limit int) string {
		if utf8.RuneCountInString(body) <= limit {
			return body
		}
		return fmt.Sprintf("%s\n**[truncated at %d chars — %s]**", string([]rune(body)[:limit]), limit, hint)
	}
	text := strings.Repeat("aя€😀", 5) + "b\xffc"
	for limit := 0; limit <= utf8.RuneCountInString(text)+1; limit++ {
		for _, body := range []string{text, text[1:], text[:len(text)-1], text + "€"[:2]} {
			if got, want := Truncate(body, "h", limit), runeCut(body, "h", limit); got != want {
				t.Fatalf("limit %d body %q: got %q, want %q", limit, body, got, want)
			}
		}
	}
}
