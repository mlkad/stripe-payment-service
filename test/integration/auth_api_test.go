//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mlkad/stripe-payment-service/internal/auth"
	"github.com/mlkad/stripe-payment-service/internal/domain"
	repo "github.com/mlkad/stripe-payment-service/internal/repository/postgres"
)

func authRequest(t *testing.T, h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

type authBody struct {
	Token string `json:"token"`
	User  struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
}

func registerUser(t *testing.T, h http.Handler, email, password string) authBody {
	t.Helper()
	rec := authRequest(t, h, http.MethodPost, "/api/v1/auth/register",
		fmt.Sprintf(`{"email":%q,"password":%q}`, email, password), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: status = %d, body = %s", rec.Code, rec.Body)
	}
	var out authBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("register body: %v", err)
	}
	return out
}

/* --- the hole this step exists to close ---------------------------------- */

// Every protected route must refuse an anonymous caller. Before this step both
// of these read a user id straight out of the request and served it.
func TestAuth_ProtectedRoutesRejectAnonymous(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	routes := []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/subscription", ""},
		{http.MethodGet, "/api/v1/auth/me", ""},
		{http.MethodPost, "/api/v1/checkout", `{"price_id":"price_X"}`},
	}
	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rec := authRequest(t, h, route.method, route.path, route.body, "")
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401 (body: %s)", rec.Code, rec.Body)
			}
			if wa := rec.Header().Get("WWW-Authenticate"); !strings.Contains(wa, "Bearer") {
				t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", wa)
			}
		})
	}
}

// The core guarantee: a caller cannot reach another user's billing state by
// naming them. The identity comes from the token; the old request fields are
// gone, and a client still sending them changes nothing.
func TestAuth_CannotReadAnotherUsersSubscription(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	victim, _ := seedSubscription(t, "sub_Victim0001")
	attacker := registerUser(t, h, "attacker@example.com", "attacker-password-1")

	// The attacker's own token, pointed at the victim every way the old API
	// allowed. None of these may return the victim's subscription.
	for _, path := range []string{
		"/api/v1/subscription",
		"/api/v1/subscription?user_id=" + victim.UserID.String(),
	} {
		rec := authRequest(t, h, http.MethodGet, path, "", attacker.Token)
		if rec.Code == http.StatusOK {
			t.Fatalf("%s returned 200 for the attacker: %s", path, rec.Body)
		}
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 (the attacker has no subscription)", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "price_Pro001") {
			t.Fatalf("%s leaked the victim's subscription: %s", path, rec.Body)
		}
	}
}

// A checkout must bill the token's subject. The body no longer has a user_id
// field at all, and DisallowUnknownFields turns a stale client into a 400
// rather than silently ignoring the field.
func TestAuth_CheckoutIgnoresClientSuppliedUserID(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	victim, _ := seedSubscription(t, "sub_Target0001")
	attacker := registerUser(t, h, "attacker2@example.com", "attacker-password-1")

	rec := authRequest(t, h, http.MethodPost, "/api/v1/checkout",
		fmt.Sprintf(`{"user_id":%q,"price_id":"price_Pro001"}`, victim.UserID), attacker.Token)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: an unknown user_id field must be rejected, not ignored (body: %s)",
			rec.Code, rec.Body)
	}
}

/* --- registration and login ---------------------------------------------- */

func TestAuth_RegisterThenLogin(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	created := registerUser(t, h, "ada@example.com", "a-sufficiently-long-password")
	if created.Token == "" {
		t.Fatal("register returned no token")
	}

	// The token must actually authorise something.
	rec := authRequest(t, h, http.MethodGet, "/api/v1/auth/me", "", created.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("me: status = %d, body = %s", rec.Code, rec.Body)
	}

	login := authRequest(t, h, http.MethodPost, "/api/v1/auth/login",
		`{"email":"ada@example.com","password":"a-sufficiently-long-password"}`, "")
	if login.Code != http.StatusOK {
		t.Fatalf("login: status = %d, body = %s", login.Code, login.Body)
	}

	var out authBody
	if err := json.Unmarshal(login.Body.Bytes(), &out); err != nil {
		t.Fatalf("login body: %v", err)
	}
	if out.User.ID != created.User.ID {
		t.Errorf("login resolved a different user: %s vs %s", out.User.ID, created.User.ID)
	}
}

// Email is CITEXT and the unique index is on the active rows, so case must not
// create a second account.
func TestAuth_EmailIsCaseInsensitive(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	registerUser(t, h, "ada@example.com", "a-sufficiently-long-password")

	dupe := authRequest(t, h, http.MethodPost, "/api/v1/auth/register",
		`{"email":"ADA@Example.COM","password":"another-long-password"}`, "")
	if dupe.Code != http.StatusConflict {
		t.Errorf("duplicate registration: status = %d, want 409", dupe.Code)
	}

	login := authRequest(t, h, http.MethodPost, "/api/v1/auth/login",
		`{"email":"ADA@EXAMPLE.COM","password":"a-sufficiently-long-password"}`, "")
	if login.Code != http.StatusOK {
		t.Errorf("login with different casing: status = %d, want 200", login.Code)
	}
}

// Login must not become an account enumeration oracle: an unknown address and a
// wrong password have to be indistinguishable in status and body.
func TestAuth_LoginDoesNotRevealWhetherAnAccountExists(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)
	registerUser(t, h, "known@example.com", "a-sufficiently-long-password")

	unknown := authRequest(t, h, http.MethodPost, "/api/v1/auth/login",
		`{"email":"nobody@example.com","password":"a-sufficiently-long-password"}`, "")
	wrongPassword := authRequest(t, h, http.MethodPost, "/api/v1/auth/login",
		`{"email":"known@example.com","password":"definitely-the-wrong-one"}`, "")

	if unknown.Code != http.StatusUnauthorized || wrongPassword.Code != http.StatusUnauthorized {
		t.Fatalf("statuses = %d and %d, want 401 for both", unknown.Code, wrongPassword.Code)
	}
	if unknown.Body.String() != wrongPassword.Body.String() {
		t.Errorf("bodies differ:\n  unknown account: %s\n  wrong password:  %s",
			unknown.Body, wrongPassword.Body)
	}
}

func TestAuth_RegisterEnforcesPasswordPolicy(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	tests := []struct{ name, password string }{
		{"too short", "short"},
		{"empty", ""},
		{"past the bcrypt 72-byte limit", strings.Repeat("a", 73)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := authRequest(t, h, http.MethodPost, "/api/v1/auth/register",
				fmt.Sprintf(`{"email":"policy-%s@example.com","password":%q}`,
					strings.ReplaceAll(tt.name, " ", "-"), tt.password), "")
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want 422 (body: %s)", rec.Code, rec.Body)
			}
		})
	}
}

// The digest must never leave the process, and the response must not carry it.
func TestAuth_PasswordDigestNeverLeaves(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	const password = "a-sufficiently-long-password"
	rec := authRequest(t, h, http.MethodPost, "/api/v1/auth/register",
		fmt.Sprintf(`{"email":"digest@example.com","password":%q}`, password), "")

	body := rec.Body.String()
	if strings.Contains(body, password) {
		t.Error("the response echoed the plaintext password")
	}
	if strings.Contains(body, "password_hash") || strings.Contains(body, "$2a$") {
		t.Errorf("the response carried the password digest: %s", body)
	}

	// And it is genuinely hashed at rest.
	stored, err := repo.NewUserRepo(pool).GetUserByEmail(t.Context(), "digest@example.com")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.PasswordHash == nil {
		t.Fatal("no password hash was stored")
	}
	if *stored.PasswordHash == password {
		t.Fatal("the password was stored in plaintext")
	}
	if !strings.HasPrefix(*stored.PasswordHash, "$2") {
		t.Errorf("stored digest is not a bcrypt hash: %q", *stored.PasswordHash)
	}
}

/* --- token handling ------------------------------------------------------- */

func TestAuth_RejectsMalformedAndForeignTokens(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	expired, err := auth.NewTokenService(auth.TokenConfig{
		Secret: testJWTSecret, Issuer: testIssuer, Audience: testAudience, TTL: time.Nanosecond,
	})
	if err != nil {
		t.Fatalf("build expiring token service: %v", err)
	}
	expiredToken, _, _ := expired.Issue(uuid.New(), "x@y.com")

	foreign, err := auth.NewTokenService(auth.TokenConfig{
		Secret: "a-completely-different-secret-32-bytes-x", Issuer: testIssuer, Audience: testAudience, TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("build foreign token service: %v", err)
	}
	foreignToken, _, _ := foreign.Issue(uuid.New(), "x@y.com")

	time.Sleep(2 * time.Millisecond)

	for _, tc := range []struct{ name, token string }{
		{"garbage", "not-a-token"},
		{"empty bearer", ""},
		{"expired", expiredToken},
		{"signed with another secret", foreignToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := authRequest(t, h, http.MethodGet, "/api/v1/subscription", "", tc.token)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

// RFC 7235 defines the scheme as case-insensitive, and real clients send
// "bearer".
func TestAuth_BearerSchemeIsCaseInsensitive(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)
	user, _ := seedSubscription(t, "sub_Scheme0001")
	token := tokenFor(t, user.UserID)

	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/subscription", nil)
		r.Header.Set("Authorization", scheme+" "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)

		if rec.Code != http.StatusOK {
			t.Errorf("scheme %q: status = %d, want 200", scheme, rec.Code)
		}
	}
}

// A token whose subject no longer exists is a credential problem, not a
// missing resource.
func TestAuth_TokenForDeletedUserIsRejected(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	rec := authRequest(t, h, http.MethodGet, "/api/v1/auth/me", "", tokenFor(t, uuid.New()))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// A soft-deleted user must not be able to log in; GetUserByEmail carries
// `deleted_at IS NULL` for exactly this reason.
func TestAuth_SoftDeletedUserCannotLogIn(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	const password = "a-sufficiently-long-password"
	created := registerUser(t, h, "gone@example.com", password)

	if _, err := pool.Exec(t.Context(),
		`UPDATE users SET deleted_at = now() WHERE id = $1`, created.User.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	rec := authRequest(t, h, http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"email":"gone@example.com","password":%q}`, password), "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// The authenticated happy path, end to end.
func TestAuth_AuthenticatedUserSeesOwnSubscription(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)
	sub, _ := seedSubscription(t, "sub_Owner00001")

	rec := authRequest(t, h, http.MethodGet, "/api/v1/subscription", "", tokenFor(t, sub.UserID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("body: %v", err)
	}
	if view["status"] != string(domain.SubscriptionActive) {
		t.Errorf("status = %v, want active", view["status"])
	}
}
