//go:build integration

package integration

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mlkad/stripe-payment-service/internal/auth"
)

const (
	testJWTSecret = "integration-test-secret-at-least-32-bytes"
	testIssuer    = "test-issuer"
	testAudience  = "test-audience"
	// Cost 10 rather than the production 12: the suite runs hundreds of
	// comparisons and 12 would add minutes without testing anything extra.
	testBcryptCost = 10
)

var (
	tokensOnce sync.Once
	tokensSvc  *auth.TokenService
	hasherOnce sync.Once
	hasherSvc  *auth.Hasher
)

func testTokens(t *testing.T) *auth.TokenService {
	t.Helper()
	tokensOnce.Do(func() {
		svc, err := auth.NewTokenService(auth.TokenConfig{
			Secret: testJWTSecret, Issuer: testIssuer, Audience: testAudience, TTL: time.Hour,
		})
		if err != nil {
			panic(err)
		}
		tokensSvc = svc
	})
	return tokensSvc
}

func testHasher(t *testing.T) *auth.Hasher {
	t.Helper()
	hasherOnce.Do(func() {
		h, err := auth.NewHasher(testBcryptCost)
		if err != nil {
			panic(err)
		}
		hasherSvc = h
	})
	return hasherSvc
}

// tokenFor mints a valid access token for an arbitrary user id.
func tokenFor(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	token, _, err := testTokens(t).Issue(userID, "someone@example.com")
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return token
}
