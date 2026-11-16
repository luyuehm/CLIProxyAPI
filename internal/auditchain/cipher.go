package auditchain

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
)

// errDecryptFailed is returned when OpenSnapshot cannot open a sealed
// snapshot (wrong key, corrupt blob, or truncated nonce).
var errDecryptFailed = errors.New("audit chain: cannot decrypt snapshot")

// newAEAD derives a keyed AEAD from the operator-supplied 32-byte key.
// The key never touches the chain hash: only the sealed snapshot bytes do,
// so records remain tamper-evident while the captured model context stays
// unreadable to anyone who does not hold the key.
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

// OpenSnapshot decrypts a sealed snapshot produced by Append under the same
// key. The blob layout is nonce || GCM(ciphertext+tag); the nonce was
// stored in front of the ciphertext so an encrypted capture is not just
// tamper-evident but also recoverable by the operator holding the key.
// This is the inverse of the seal performed in Append and is exposed so
// compliance tooling (out of scope for this channel) can read back the
// model context without re-implementing the layout.
func OpenSnapshot(key, blob []byte) ([]byte, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	ns := aead.NonceSize()
	if len(blob) < ns+1 {
		return nil, errDecryptFailed
	}
	nonce, ct := blob[:ns], blob[ns:]
	plain, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, errDecryptFailed
	}
	return plain, nil
}
