package auth

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var (
	ErrTokenInvalid = errors.New("token is not valid")
	ErrTokenExpired = errors.New("token has expired")
)

// MinSecretBytes is the HMAC key floor. HS256 keys shorter than the 256-bit
// digest add nothing over a 256-bit one and are usually a passphrase someone
// typed, which is guessable.
const MinSecretBytes = 32

// TokenConfig configures issuance and verification. Issuer and Audience are not
// decoration: they are verified on every parse, so a token minted for a
// different service or a different environment cannot be replayed here.
type TokenConfig struct {
	Secret   string
	Issuer   string
	Audience string
	TTL      time.Duration
}

// Claims is the token payload. Only the subject is trusted downstream; email is
// carried for convenience in logs and the UI and is never used for lookups,
// because a user can change it while a token is still live.
type Claims struct {
	jwt.RegisteredClaims
	Email string `json:"email,omitempty"`
}

type TokenService struct {
	secret   []byte
	issuer   string
	audience string
	ttl      time.Duration
	parser   *jwt.Parser
}

func NewTokenService(cfg TokenConfig) (*TokenService, error) {
	if len(cfg.Secret) < MinSecretBytes {
		return nil, fmt.Errorf("jwt secret must be at least %d bytes, got %d", MinSecretBytes, len(cfg.Secret))
	}
	if cfg.Issuer == "" || cfg.Audience == "" {
		return nil, errors.New("jwt issuer and audience are required")
	}
	if cfg.TTL <= 0 {
		return nil, errors.New("jwt ttl must be positive")
	}

	return &TokenService{
		secret:   []byte(cfg.Secret),
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
		ttl:      cfg.TTL,
		// WithValidMethods pins the accepted algorithm. It is defence in depth
		// rather than the only guard: jwt/v5 refuses alg:none on its own, and
		// the keyfunc below always returns []byte, so an RS256 header fails on
		// key type before any signature is checked. The allowlist is what keeps
		// that true if the keyfunc ever grows to return more than one key type,
		// which is where algorithm confusion actually becomes reachable.
		parser: jwt.NewParser(
			jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
			jwt.WithIssuer(cfg.Issuer),
			jwt.WithAudience(cfg.Audience),
			jwt.WithExpirationRequired(),
			jwt.WithIssuedAt(),
		),
	}, nil
}

func (s *TokenService) TTL() time.Duration { return s.ttl }

// Issue mints an access token for a user.
func (s *TokenService) Issue(userID uuid.UUID, email string) (string, time.Time, error) {
	now := time.Now().UTC()
	expiresAt := now.Add(s.ttl)

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    s.issuer,
			Audience:  jwt.ClaimStrings{s.audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			// A unique id per token. Nothing consumes it yet; it is what a
			// revocation list would key on, and it costs nothing to mint now.
			ID: uuid.NewString(),
		},
		Email: email,
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign token: %w", err)
	}
	return signed, expiresAt, nil
}

// Parse verifies a token and returns its subject.
//
// Every failure other than expiry collapses to ErrTokenInvalid: the caller has
// no legitimate use for the distinction between a bad signature, a wrong
// audience and a malformed segment, and reporting it tells a forger which part
// to fix next.
func (s *TokenService) Parse(token string) (uuid.UUID, *Claims, error) {
	var claims Claims
	parsed, err := s.parser.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		return s.secret, nil
	})
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return uuid.Nil, nil, ErrTokenExpired
	case err != nil:
		return uuid.Nil, nil, fmt.Errorf("%w: %s", ErrTokenInvalid, err)
	case !parsed.Valid:
		return uuid.Nil, nil, ErrTokenInvalid
	}

	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("%w: subject is not a uuid", ErrTokenInvalid)
	}
	return userID, &claims, nil
}

// SecretsEqual compares two secrets without leaking their contents through
// timing. Used by configuration checks, never on the request path.
func SecretsEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
