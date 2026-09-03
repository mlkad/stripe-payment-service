//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mlkad/stripe-payment-service/internal/auth"
	repo "github.com/mlkad/stripe-payment-service/internal/repository/postgres"
)

// authWithCookies performs a request carrying the given cookies and returns the
// recorder, so a test can follow a session the way a browser does.
func authWithCookies(t *testing.T, h http.Handler, method, path, body string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func refreshCookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == "sps_refresh" {
			return c
		}
	}
	t.Fatalf("no refresh cookie on the response (headers: %v)", rec.Header())
	return nil
}

func registerWithSession(t *testing.T, h http.Handler, email string) (authBody, *http.Cookie) {
	t.Helper()
	rec := authWithCookies(t, h, http.MethodPost, "/api/v1/auth/register",
		fmt.Sprintf(`{"email":%q,"password":"a-sufficiently-long-password"}`, email), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: status = %d, body = %s", rec.Code, rec.Body)
	}
	var out authBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("register body: %v", err)
	}
	return out, refreshCookieFrom(t, rec)
}

// expireToken backdates a token so it expired `ago` in the past. Both
// timestamps move, because refresh_tokens_expiry_after_issue_chk requires
// expires_at to stay after issued_at.
func expireToken(t *testing.T, token string, ago time.Duration) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		UPDATE refresh_tokens
		SET issued_at  = now() - make_interval(secs => $2) - interval '1 hour',
		    expires_at = now() - make_interval(secs => $2)
		WHERE token_hash = $1`,
		auth.HashRefreshToken(token), ago.Seconds())
	if err != nil {
		t.Fatalf("expire token: %v", err)
	}
}

// The cookie is what stops an XSS from becoming permanent access: script can
// read the access token, but not this.
func TestRefresh_CookieIsHardened(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	_, cookie := registerWithSession(t, h, "cookie@example.com")

	if !cookie.HttpOnly {
		t.Error("the refresh cookie is readable by script")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict: refresh and logout are state-changing "+
			"and authenticated by this cookie alone", cookie.SameSite)
	}
	// Scoped to the auth endpoints, so it is not attached to ordinary API calls.
	if cookie.Path != "/api/v1/auth" {
		t.Errorf("Path = %q, want /api/v1/auth", cookie.Path)
	}
	if cookie.Value == "" {
		t.Error("the cookie carries no token")
	}
	// A response body must never carry the refresh token; the cookie is the
	// only channel.
	rec := authWithCookies(t, h, http.MethodPost, "/api/v1/auth/refresh", "", []*http.Cookie{cookie})
	if strings.Contains(rec.Body.String(), refreshCookieFrom(t, rec).Value) {
		t.Error("the response body echoed the refresh token")
	}
}

// The token is a bearer credential; a leaked table must yield nothing usable.
func TestRefresh_TokenIsNotStoredInPlaintext(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	_, cookie := registerWithSession(t, h, "hash@example.com")

	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM refresh_tokens WHERE token_hash = $1`, cookie.Value).Scan(&count); err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 0 {
		t.Error("the raw token is stored in token_hash")
	}

	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM refresh_tokens WHERE token_hash = $1`,
		auth.HashRefreshToken(cookie.Value)).Scan(&count); err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 1 {
		t.Errorf("found %d rows for the token's hash, want 1", count)
	}
}

// Every renewal consumes one token and issues another, so a leaked token is
// useful only until the legitimate client next refreshes.
func TestRefresh_RotatesOnEveryUse(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	first, cookie := registerWithSession(t, h, "rotate@example.com")

	rec := authWithCookies(t, h, http.MethodPost, "/api/v1/auth/refresh", "", []*http.Cookie{cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: status = %d, body = %s", rec.Code, rec.Body)
	}

	next := refreshCookieFrom(t, rec)
	if next.Value == cookie.Value {
		t.Fatal("the refresh token was not rotated")
	}

	var out authBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("refresh body: %v", err)
	}
	if out.Token == "" || out.Token == first.Token {
		t.Error("refresh did not issue a new access token")
	}

	// The new token has to work.
	again := authWithCookies(t, h, http.MethodPost, "/api/v1/auth/refresh", "", []*http.Cookie{next})
	if again.Code != http.StatusOK {
		t.Errorf("the rotated token was rejected: status = %d", again.Code)
	}
}

// The property the whole design exists for. A consumed token coming back means
// either the thief or the victim is presenting a token the other has spent, and
// there is no way to tell which - so the family goes.
func TestRefresh_ReuseRevokesTheEntireFamily(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	_, stolen := registerWithSession(t, h, "reuse@example.com")

	// The legitimate client refreshes, consuming the token the thief also holds.
	rec := authWithCookies(t, h, http.MethodPost, "/api/v1/auth/refresh", "", []*http.Cookie{stolen})
	if rec.Code != http.StatusOK {
		t.Fatalf("legitimate refresh: status = %d", rec.Code)
	}
	live := refreshCookieFrom(t, rec)

	// The thief now presents the spent token.
	replay := authWithCookies(t, h, http.MethodPost, "/api/v1/auth/refresh", "", []*http.Cookie{stolen})
	if replay.Code != http.StatusUnauthorized {
		t.Fatalf("replaying a spent token: status = %d, want 401", replay.Code)
	}

	// And the legitimate client's live token is dead too - the family was
	// revoked, because there is no way to know which side was the thief.
	after := authWithCookies(t, h, http.MethodPost, "/api/v1/auth/refresh", "", []*http.Cookie{live})
	if after.Code != http.StatusUnauthorized {
		t.Errorf("the live token still works after reuse detection: status = %d; "+
			"the family was not revoked", after.Code)
	}

	var revoked int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM refresh_tokens WHERE revoked_at IS NOT NULL`).Scan(&revoked); err != nil {
		t.Fatalf("query: %v", err)
	}
	if revoked < 2 {
		t.Errorf("%d tokens revoked, want the whole family", revoked)
	}
}

// Reuse detection must not be defeatable by racing: two requests with the same
// token must not both succeed.
func TestRefresh_ConcurrentUseOfOneTokenElectsOneWinner(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	_, cookie := registerWithSession(t, h, "race@example.com")

	const attempts = 8
	results := make(chan int, attempts)
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		go func() {
			<-start
			results <- authWithCookies(t, h, http.MethodPost, "/api/v1/auth/refresh", "",
				[]*http.Cookie{cookie}).Code
		}()
	}
	close(start)

	ok := 0
	for i := 0; i < attempts; i++ {
		if <-results == http.StatusOK {
			ok++
		}
	}
	if ok != 1 {
		t.Errorf("%d of %d concurrent refreshes succeeded, want exactly 1", ok, attempts)
	}
}

// Logout ends the session, and the successor the client already holds must not
// resurrect it.
func TestRefresh_LogoutRevokesTheFamily(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	_, cookie := registerWithSession(t, h, "logout@example.com")

	rec := authWithCookies(t, h, http.MethodPost, "/api/v1/auth/logout", "", []*http.Cookie{cookie})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout: status = %d, want 204", rec.Code)
	}
	// The clearing cookie must mirror the original's attributes, or the browser
	// treats it as a different cookie and leaves the real one in place.
	cleared := refreshCookieFrom(t, rec)
	if cleared.MaxAge >= 0 {
		t.Errorf("MaxAge = %d, want a negative value to clear the cookie", cleared.MaxAge)
	}
	if cleared.Path != cookie.Path {
		t.Errorf("clearing Path = %q, want %q to match the original", cleared.Path, cookie.Path)
	}

	after := authWithCookies(t, h, http.MethodPost, "/api/v1/auth/refresh", "", []*http.Cookie{cookie})
	if after.Code != http.StatusUnauthorized {
		t.Errorf("the token still works after logout: status = %d", after.Code)
	}
}

// A client clearing a session it no longer holds is not an error; reporting one
// would leave the UI unable to sign out.
func TestRefresh_LogoutIsIdempotent(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	for _, cookies := range [][]*http.Cookie{
		nil,
		{{Name: "sps_refresh", Value: "not-a-real-token"}},
	} {
		rec := authWithCookies(t, h, http.MethodPost, "/api/v1/auth/logout", "", cookies)
		if rec.Code != http.StatusNoContent {
			t.Errorf("logout with %v: status = %d, want 204", cookies, rec.Code)
		}
	}
}

func TestRefresh_RejectsUnknownAndMissingTokens(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	for _, tc := range []struct {
		name    string
		cookies []*http.Cookie
	}{
		{"no cookie", nil},
		{"unknown token", []*http.Cookie{{Name: "sps_refresh", Value: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}},
		{"empty value", []*http.Cookie{{Name: "sps_refresh", Value: ""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := authWithCookies(t, h, http.MethodPost, "/api/v1/auth/refresh", "", tc.cookies)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

// Signing in on a second device must not end the session on the first.
func TestRefresh_SessionsAreIndependentPerLogin(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	const password = "a-sufficiently-long-password"
	_, first := registerWithSession(t, h, "twodevices@example.com")

	loginRec := authWithCookies(t, h, http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"email":"twodevices@example.com","password":%q}`, password), nil)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("second login: status = %d", loginRec.Code)
	}
	second := refreshCookieFrom(t, loginRec)

	// Reuse on the second device must not touch the first.
	authWithCookies(t, h, http.MethodPost, "/api/v1/auth/refresh", "", []*http.Cookie{second})
	authWithCookies(t, h, http.MethodPost, "/api/v1/auth/refresh", "", []*http.Cookie{second})

	rec := authWithCookies(t, h, http.MethodPost, "/api/v1/auth/refresh", "", []*http.Cookie{first})
	if rec.Code != http.StatusOK {
		t.Errorf("the first device's session was ended by the second's: status = %d", rec.Code)
	}
}

// Expiry is enforced, not merely recorded.
func TestRefresh_ExpiredTokenIsRejected(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	_, cookie := registerWithSession(t, h, "expired@example.com")
	// issued_at moves with it: refresh_tokens_expiry_after_issue_chk requires
	// expires_at > issued_at, so backdating one alone is rejected - which is
	// the constraint doing its job.
	expireToken(t, cookie.Value, time.Second)

	rec := authWithCookies(t, h, http.MethodPost, "/api/v1/auth/refresh", "", []*http.Cookie{cookie})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for an expired token", rec.Code)
	}
}

// Retention deletes tokens nothing can use, but only after a grace period so
// reuse detection still fires on one that expired between theft and use.
func TestRefresh_RetentionPrunesOnlyLongExpiredTokens(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)
	ctx := context.Background()

	_, recent := registerWithSession(t, h, "recent@example.com")
	_, ancient := registerWithSession(t, h, "ancient@example.com")

	expireToken(t, recent.Value, time.Hour)
	expireToken(t, ancient.Value, 30*24*time.Hour)

	deleted, err := repo.NewRefreshTokenRepo(pool).
		DeleteExpiredRefreshTokens(ctx, 7*24*time.Hour, 100)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted %d tokens, want 1", deleted)
	}

	var remaining int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM refresh_tokens WHERE token_hash = $1`,
		auth.HashRefreshToken(recent.Value)).Scan(&remaining); err != nil {
		t.Fatalf("query: %v", err)
	}
	if remaining != 1 {
		t.Error("a token inside the grace period was pruned; reuse detection would miss it")
	}
}
