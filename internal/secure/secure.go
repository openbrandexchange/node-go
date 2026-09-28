// Package secure holds the node's small cryptographic helpers: random
// tokens, token hashing, constant-time comparison, AES-256-GCM encryption of
// stored credentials and HMAC signing.
package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

// RandomString returns n random bytes encoded as unpadded base64url.
func RandomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Token returns prefix + 32 random bytes (256 bits) in base64url.
func Token(prefix string) string { return prefix + RandomString(32) }

// HashToken is the SHA-256 hex of a high-entropy token (sufficient for
// random secrets; passwords use argon2id, see password.go).
func HashToken(token string) string {
	s := sha256.Sum256([]byte(token))
	return hex.EncodeToString(s[:])
}

// Equal compares two strings in constant time.
func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Keys derives purpose-specific keys from the node secret.
type Keys struct {
	master []byte
}

// NewKeys wraps the node secret (OBX_SECRET_KEY, at least 32 bytes).
func NewKeys(secret []byte) *Keys { return &Keys{master: append([]byte(nil), secret...)} }

func (k *Keys) derive(purpose string) []byte {
	m := hmac.New(sha256.New, k.master)
	m.Write([]byte("obx-node-go/" + purpose))
	return m.Sum(nil)
}

// Sign returns the lowercase hex HMAC-SHA256 of msg under the key for purpose.
func (k *Keys) Sign(purpose, msg string) string {
	m := hmac.New(sha256.New, k.derive(purpose))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// Verify checks a signature produced by Sign in constant time.
func (k *Keys) Verify(purpose, msg, sig string) bool {
	return Equal(k.Sign(purpose, msg), strings.ToLower(sig))
}

// Encrypt seals plaintext with AES-256-GCM; the result is base64url text.
func (k *Keys) Encrypt(plaintext []byte) (string, error) {
	block, err := aes.NewCipher(k.derive("encrypt"))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := gcm.Seal(nonce, nonce, plaintext, nil)
	return "v1." + base64.RawURLEncoding.EncodeToString(out), nil
}

// Decrypt opens a value produced by Encrypt.
func (k *Keys) Decrypt(s string) ([]byte, error) {
	if !strings.HasPrefix(s, "v1.") {
		return nil, errors.New("unknown ciphertext version")
	}
	raw, err := base64.RawURLEncoding.DecodeString(s[3:])
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k.derive("encrypt"))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	return gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
}

// WebhookSignature computes the OBX webhook signature header value:
// v1=hex(HMAC-SHA256(secret, "<ts>.<raw body>")), keyed with the secret's
// UTF-8 bytes including the whsec_ prefix.
func WebhookSignature(secret, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(timestamp))
	m.Write([]byte("."))
	m.Write(body)
	return "v1=" + hex.EncodeToString(m.Sum(nil))
}
