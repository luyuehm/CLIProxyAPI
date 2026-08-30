package contentfilter

import (
	"crypto/sha256"
	"encoding/hex"
)

// shortHash returns the first 12 hex characters of SHA-256(s), used to
// pseudonymise sensitive identifiers (e.g. Authorization tokens) before they
// are written to the audit log. Operators get a stable per-caller handle
// without exposing raw secrets in the database.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}
