// Package crypto holds the vault's at-rest encryption and password primitives.
//
// Design notes (these are why the base ships gosec-clean):
//   - Secret values are sealed with AES-256-GCM; nonces come from crypto/rand and are
//     prepended to the ciphertext. GCM authenticates, so a tampered blob fails to open.
//   - The encryption key is DERIVED from the master password with Argon2id over a
//     per-vault random salt — the key is never stored, only held in memory after login.
//   - The master password is authenticated with a separate Argon2id hash and verified in
//     constant time, so login does not leak timing about the stored hash.
//
// No primitive here is the one-time-share feature; RandomToken is a generic helper.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/argon2"
)

// KeyLen is the AES-256 key length in bytes.
const KeyLen = 32

// SaltLen is the length of the random salts used for key derivation and password hashing.
const SaltLen = 16

// Argon2id cost parameters. Modest but real — enough to be honest in a demo without
// making login feel slow on a laptop on stage.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
)

// ErrDecrypt is returned when a sealed value cannot be opened (wrong key or tampering).
var ErrDecrypt = errors.New("crypto: cannot decrypt value")

// NewSalt returns SaltLen cryptographically-random bytes.
func NewSalt() ([]byte, error) {
	salt := make([]byte, SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("crypto: read salt: %w", err)
	}
	return salt, nil
}

// DeriveKey turns a master password + salt into a 32-byte AES key via Argon2id.
func DeriveKey(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, KeyLen)
}

// Seal encrypts plaintext with AES-256-GCM under key, returning nonce||ciphertext.
func Seal(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("crypto: read nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Open reverses Seal. A wrong key or any tampering yields ErrDecrypt.
func Open(key, blob []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(blob) < ns {
		return nil, ErrDecrypt
	}
	nonce, ct := blob[:ns], blob[ns:]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeyLen {
		return nil, fmt.Errorf("crypto: key must be %d bytes, got %d", KeyLen, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: new gcm: %w", err)
	}
	return gcm, nil
}

// HashPassword returns an Argon2id verifier string ("argon2id$<saltb64>$<hashb64>") for
// the master password. The salt is random per call.
func HashPassword(password string) (string, error) {
	salt, err := NewSalt()
	if err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, KeyLen)
	return fmt.Sprintf("argon2id$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash)), nil
}

// VerifyPassword reports whether password matches a verifier produced by HashPassword,
// comparing in constant time.
func VerifyPassword(password, verifier string) bool {
	parts := strings.SplitN(verifier, "$", 3)
	if len(parts) != 3 || parts[0] != "argon2id" {
		return false
	}
	saltB64, hashB64 := parts[1], parts[2]
	salt, err := base64.RawStdEncoding.DecodeString(saltB64)
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(hashB64)
	if err != nil || len(want) != KeyLen {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, KeyLen)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// RandomToken returns n random bytes encoded as a URL-safe base64 string. Generic
// helper (used for session IDs); not the share-link feature.
func RandomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto: read token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
