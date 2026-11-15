package auditchain

import (
	"crypto/aes"
	"crypto/cipher"
	"fmt"
)

// newAEAD derives a keyed AEAD from the operator-supplied 32-byte key.
// The key never touches the chain hash: only the sealed snapshot bytes do,
// so records remain tamper-evident while the captured model context stays
// unreadable without the key.
func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("%w: must be 32 bytes", ErrInvalidKey)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("audit chain: aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("audit chain: gcm: %w", err)
	}
	return aead, nil
}
