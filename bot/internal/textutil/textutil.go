package textutil

// Ellipsize returns s if it's already at most n bytes; otherwise it returns
// the first n bytes followed by "...". Note: operates on bytes, not runes —
// fine for log truncation, not for user-visible UTF-8-aware truncation.
func Ellipsize(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
