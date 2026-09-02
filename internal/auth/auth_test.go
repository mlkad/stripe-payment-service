package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const testSecret = "test-secret-at-least-32-bytes-long-for-hs256"

func newTestTokens(t *testing.T, ttl time.Duration) *TokenService {
	t.Helper()
	svc, err := NewTokenService(TokenConfig{
		Secret: testSecret, Issuer: "test-iss", Audience: "test-aud", TTL: ttl,
	})
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	return svc
}

func TestTokenRoundTrip(t *testing.T) {
	svc := newTestTokens(t, time.Hour)
	userID := uuid.New()

	token, expiresAt, err := svc.Issue(userID, "ada@example.com")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if !expiresAt.After(time.Now()) {
		t.Errorf("expiresAt %v is not in the future", expiresAt)
	}

	got, claims, err := svc.Parse(token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got != userID {
		t.Errorf("subject = %v, want %v", got, userID)
	}
	if claims.Email != "ada@example.com" {
		t.Errorf("email = %q, want ada@example.com", claims.Email)
	}
	if claims.ID == "" {
		t.Error("token carries no jti")
	}
}

func TestTokenRejectsExpired(t *testing.T) {
	svc := newTestTokens(t, time.Millisecond)
	token, _, err := svc.Issue(uuid.New(), "x@y.com")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	if _, _, err := svc.Parse(token); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("err = %v, want ErrTokenExpired", err)
	}
}

// An unsigned token must never authenticate.
//
// This asserts the outcome, not the mechanism: jwt/v5 refuses alg:none in the
// library itself, so this passes with or without WithValidMethods. Verified by
// removing the allowlist and re-running - the test still passed, which is why
// this comment does not claim otherwise. The allowlist stays as defence in
// depth for a future keyfunc that returns more than one key type.
func TestTokenRejectsUnsignedToken(t *testing.T) {
	svc := newTestTokens(t, time.Hour)
	userID := uuid.New()

	claims := Claims{RegisteredClaims: jwt.RegisteredClaims{
		Subject:   userID.String(),
		Issuer:    "test-iss",
		Audience:  jwt.ClaimStrings{"test-aud"},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		IssuedAt:  jwt.NewNumericDate(time.Now()),
	}}

	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("could not build the alg:none token: %v", err)
	}

	if _, _, err := svc.Parse(unsigned); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("an unsigned token authenticated (err = %v)", err)
	}
}

func TestTokenRejectsForeignIssuerAndAudience(t *testing.T) {
	svc := newTestTokens(t, time.Hour)

	other, err := NewTokenService(TokenConfig{
		Secret: testSecret, Issuer: "other-iss", Audience: "other-aud", TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}

	// Same signing key, different issuer and audience: a staging token replayed
	// against production must still fail.
	token, _, err := other.Issue(uuid.New(), "x@y.com")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, _, err := svc.Parse(token); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("err = %v, want ErrTokenInvalid for a foreign issuer/audience", err)
	}
}

func TestTokenRejectsWrongSignature(t *testing.T) {
	svc := newTestTokens(t, time.Hour)
	token, _, err := svc.Issue(uuid.New(), "x@y.com")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Flip one character of the signature segment.
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	sig := []byte(parts[2])
	sig[0] ^= 'A' ^ 'B'
	tampered := parts[0] + "." + parts[1] + "." + string(sig)

	if _, _, err := svc.Parse(tampered); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("err = %v, want ErrTokenInvalid", err)
	}
}

func TestTokenServiceRejectsWeakConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  TokenConfig
	}{
		{"short secret", TokenConfig{Secret: "too-short", Issuer: "i", Audience: "a", TTL: time.Hour}},
		{"no issuer", TokenConfig{Secret: testSecret, Audience: "a", TTL: time.Hour}},
		{"no audience", TokenConfig{Secret: testSecret, Issuer: "i", TTL: time.Hour}},
		{"zero ttl", TokenConfig{Secret: testSecret, Issuer: "i", Audience: "a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewTokenService(tt.cfg); err == nil {
				t.Error("weak configuration was accepted")
			}
		})
	}
}

/* --- passwords ------------------------------------------------------------ */

func TestPasswordHashAndVerify(t *testing.T) {
	h, err := NewHasher(bcryptTestCost)
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}

	digest, err := h.Hash("correct horse battery staple")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if digest == "correct horse battery staple" {
		t.Fatal("password was stored in plaintext")
	}
	if err := h.Verify(digest, "correct horse battery staple"); err != nil {
		t.Errorf("Verify with the right password: %v", err)
	}
	if err := h.Verify(digest, "wrong password entirely"); !errors.Is(err, ErrCredentialsMismatch) {
		t.Errorf("Verify with the wrong password: %v, want ErrCredentialsMismatch", err)
	}
}

// bcrypt truncates at 72 bytes. Accepting a longer password would mean two
// passwords sharing a 72-byte prefix authenticate each other.
func TestPasswordPolicy(t *testing.T) {
	tests := []struct {
		name     string
		password string
		wantErr  error
	}{
		{"too short", "short", ErrPasswordTooShort},
		{"at the minimum", strings.Repeat("a", MinPasswordBytes), nil},
		{"at the bcrypt limit", strings.Repeat("a", MaxPasswordBytes), nil},
		{"past the bcrypt limit", strings.Repeat("a", MaxPasswordBytes+1), ErrPasswordTooLong},
		{"multibyte past the limit in bytes", strings.Repeat("é", 40), ErrPasswordTooLong},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePassword(tt.password)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// An identity with no password (federated, or mid-migration) must not be
// loggable into with an empty string.
func TestPasswordVerifyRejectsEmptyDigest(t *testing.T) {
	h, _ := NewHasher(bcryptTestCost)
	for _, candidate := range []string{"", "anything"} {
		if err := h.Verify("", candidate); !errors.Is(err, ErrCredentialsMismatch) {
			t.Errorf("Verify(%q, %q) = %v, want ErrCredentialsMismatch", "", candidate, err)
		}
	}
}

func TestNeedsRehashDetectsWeakerCost(t *testing.T) {
	weak, _ := NewHasher(10)
	strong, _ := NewHasher(12)

	digest, err := weak.Hash("a password long enough")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strong.NeedsRehash(digest) {
		t.Error("a cost-10 digest was not flagged for upgrade by a cost-12 hasher")
	}
	if weak.NeedsRehash(digest) {
		t.Error("a cost-10 digest was flagged for upgrade by a cost-10 hasher")
	}
}

// Cost 10 keeps the suite fast; the production default is 12 and is exercised
// by the config validation instead.
const bcryptTestCost = 10
