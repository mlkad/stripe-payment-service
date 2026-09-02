// Package auth holds the credential primitives: password hashing and the
// signing and verification of access tokens.
//
// It imports nothing from the rest of the project, so the rules encoded here
// cannot drift from whatever a caller happens to remember.
package auth

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

// Password policy.
//
// MaxPasswordBytes is not a style choice: bcrypt silently truncates its input
// at 72 bytes. Accepting a longer password would mean two distinct passwords
// sharing a 72-byte prefix authenticate each other, and the user would never
// know their tail was discarded. Rejecting is the only honest option.
const (
	MinPasswordBytes = 12
	MaxPasswordBytes = 72

	// DefaultBcryptCost is deliberately above bcrypt.DefaultCost (10). Each
	// increment doubles the work; 12 costs roughly 250ms on current hardware,
	// which is unnoticeable on a login and expensive across a stolen dump.
	DefaultBcryptCost = 12
)

var (
	ErrPasswordTooShort = fmt.Errorf("password must be at least %d characters", MinPasswordBytes)
	ErrPasswordTooLong  = fmt.Errorf("password must be at most %d bytes", MaxPasswordBytes)
	ErrPasswordInvalid  = errors.New("password is not valid UTF-8")

	// ErrCredentialsMismatch is returned for both an unknown identity and a
	// wrong password. Callers must not distinguish the two: doing so turns the
	// login endpoint into an account enumeration oracle.
	ErrCredentialsMismatch = errors.New("invalid credentials")
)

// Hasher wraps bcrypt at a fixed cost.
type Hasher struct {
	cost int
	// decoy is a digest at this hasher's cost, compared against when no
	// account exists. See DummyCompare.
	decoy []byte
}

func NewHasher(cost int) (*Hasher, error) {
	if cost == 0 {
		cost = DefaultBcryptCost
	}
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		return nil, fmt.Errorf("bcrypt cost %d is outside the supported range %d-%d",
			cost, bcrypt.MinCost, bcrypt.MaxCost)
	}

	// Generated at this cost rather than hardcoded. A fixed decoy digest only
	// masks the timing difference when its cost matches the one real hashes use
	// - with a cost-12 decoy against cost-10 stored hashes, "no such account"
	// takes ~290ms and "wrong password" ~65ms, which is a louder oracle than
	// having no decoy at all. Measured, not assumed.
	decoy, err := bcrypt.GenerateFromPassword([]byte("decoy-for-constant-time-login"), cost)
	if err != nil {
		return nil, fmt.Errorf("build decoy digest: %w", err)
	}
	return &Hasher{cost: cost, decoy: decoy}, nil
}

// ValidatePassword enforces the policy before any hashing happens.
func ValidatePassword(password string) error {
	if !utf8.ValidString(password) {
		return ErrPasswordInvalid
	}
	// Measured in bytes, matching bcrypt's own limit: a 30-character password of
	// non-ASCII runes can exceed 72 bytes.
	switch n := len(password); {
	case n < MinPasswordBytes:
		return ErrPasswordTooShort
	case n > MaxPasswordBytes:
		return ErrPasswordTooLong
	}
	return nil
}

func (h *Hasher) Hash(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	digest, err := bcrypt.GenerateFromPassword([]byte(password), h.cost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(digest), nil
}

// Verify compares a candidate against a stored digest.
//
// It returns ErrCredentialsMismatch for every failure, including a malformed or
// empty digest, so a user row without a password (a federated identity, or one
// mid-migration) cannot be logged into with an empty string.
func (h *Hasher) Verify(digest, candidate string) error {
	if digest == "" {
		// Still burn a comparison so an identity with no password takes the same
		// time as one with a password. See DummyCompare for why that matters.
		h.DummyCompare()
		return ErrCredentialsMismatch
	}
	if err := bcrypt.CompareHashAndPassword([]byte(digest), []byte(candidate)); err != nil {
		return ErrCredentialsMismatch
	}
	return nil
}

// DummyCompare burns one bcrypt comparison against a decoy digest.
//
// The login path calls it when no account matches, so the response takes the
// same time as a real password check. Without it, "unknown email" returns in
// microseconds while "known email, wrong password" takes a quarter second, and
// that gap is an account enumeration oracle measurable over the internet.
func (h *Hasher) DummyCompare() {
	_ = bcrypt.CompareHashAndPassword(h.decoy, []byte("not-the-password"))
}

// NeedsRehash reports whether a stored digest was produced at a lower cost than
// the current setting, so it can be upgraded on the next successful login.
func (h *Hasher) NeedsRehash(digest string) bool {
	cost, err := bcrypt.Cost([]byte(digest))
	return err == nil && cost < h.cost
}
