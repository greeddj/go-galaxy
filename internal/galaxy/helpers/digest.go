package helpers

// SHA256HexLen is the length of a sha256 digest written as lowercase hex
// (32 bytes -> 64 hex chars).
const SHA256HexLen = 64

// IsSHA256Hex reports whether s is exactly SHA256HexLen lowercase hex digits,
// the one digest shape this project writes or trusts. markerRel relies on it
// to keep a sha out of path traversal, so the alphabet must not be widened.
func IsSHA256Hex(s string) bool {
	if len(s) != SHA256HexLen {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
