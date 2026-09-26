package mattermost

import (
	"fmt"
	"math"
	"unicode/utf8"
)

// MaxBodyChars is where post bodies are cut unless the caller passes full=true.
const MaxBodyChars = 500

// FullHint is the Truncate hint for post bodies.
const FullHint = "pass full=true for the whole message"

// HumanSize renders a byte count as `N B` below 1 KiB, then KB/MB/GB/TB with one decimal
// (base 1024). The unit is picked after rounding, so 1048575 bytes is `1.0 MB`, not `1024.0 KB`.
func HumanSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	value := float64(bytes)
	unit := -1
	for {
		value /= 1024
		unit++
		if math.Round(value*10) < 1024*10 || unit == len(units)-1 {
			break
		}
	}
	return fmt.Sprintf("%.1f %s", value, units[unit])
}

// Truncate cuts body to limit characters (runes) and appends the marker line
// `**[truncated at <limit> chars — <hint>]**`. A body within the limit is returned unchanged.
func Truncate(body, hint string, limit int) string {
	limit = max(limit, 0)
	// Find the byte offset of rune limit instead of converting the whole body to []rune.
	n := 0
	for i := range body {
		if n == limit {
			head := body[:i]
			// As a []rune conversion would, turn invalid bytes into U+FFFD.
			if !utf8.ValidString(head) {
				head = string([]rune(head))
			}
			return fmt.Sprintf("%s\n**[truncated at %d chars — %s]**", head, limit, hint)
		}
		n++
	}
	return body
}
