package secure

import (
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters (OWASP: m=19 MiB, t=2, p=1).
const (
	argonMemory  = 19 * 1024
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
)

// HashPassword returns a PHC-formatted argon2id hash.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// CheckPassword verifies pw against a hash produced by HashPassword.
func CheckPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// DummyPasswordCheck spends the same time as a real check, so login does not
// reveal whether an account exists.
func DummyPasswordCheck(pw string) {
	argon2.IDKey([]byte(pw), []byte("obx-dummy-salt!!"), argonTime, argonMemory, argonThreads, argonKeyLen)
}

//go:embed common-passwords.txt
var commonPasswordsTxt string

var commonPasswords = func() map[string]bool {
	m := map[string]bool{}
	for _, l := range strings.Split(commonPasswordsTxt, "\n") {
		l = strings.TrimSpace(strings.ToLower(l))
		if l != "" && !strings.HasPrefix(l, "#") {
			m[l] = true
		}
	}
	return m
}()

// ErrWeakPassword explains why a password is refused.
var ErrWeakPassword = errors.New("weak password")

// PasswordProblem returns "" when pw is acceptable, else a human reason.
func PasswordProblem(pw string) string {
	n := utf8.RuneCountInString(pw)
	if n < 10 {
		return "Use at least 10 characters."
	}
	if len(pw) > 72 {
		return "Use at most 72 characters."
	}
	if commonPasswords[strings.ToLower(pw)] {
		return "This password is too common. Choose another one."
	}
	return ""
}
