package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// refreshTokenBytes is the entropy behind a refresh token. 256 bits makes
// guessing irrelevant, which is what lets the stored form be a plain SHA-256
// rather than a password hash.
const refreshTokenBytes = 32

var (
	// ErrRefreshTokenInvalid covers every way a presented token fails: unknown,
	// expired, revoked, or malformed. Callers must not distinguish them - the
	// answer is the same, and the difference tells a thief which one they hold.
	ErrRefreshTokenInvalid = errors.New("refresh token is not valid")

	// ErrRefreshTokenReused means a token that was already exchanged came back.
	// Either the thief or the legitimate client is presenting a spent token and
	// there is no way to tell which, so the family is revoked and both sign in
	// again.
	ErrRefreshTokenReused = errors.New("refresh token was already used")
)

// NewRefreshToken mints a token and its stored hash.
//
// The plaintext is returned once and never persisted; only the hash is stored.
// A leaked table therefore yields nothing usable.
func NewRefreshToken() (token, hash string, err error) {
	buf := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate refresh token: %w", err)
	}
	// URL-safe and unpadded so it survives a cookie value without escaping.
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, HashRefreshToken(token), nil
}

// HashRefreshToken renders the lookup key for a token.
//
// SHA-256, not bcrypt. The input is 256 bits of entropy, so there is no
// dictionary attack to slow down, and a password hash would put its full cost
// on every renewal for no security gain.
func HashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
