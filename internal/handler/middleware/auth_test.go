package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/mlkad/stripe-payment-service/internal/auth"
)

type stubParser struct {
	userID uuid.UUID
	err    error
}

func (s stubParser) Parse(string) (uuid.UUID, *auth.Claims, error) {
	if s.err != nil {
		return uuid.Nil, nil, s.err
	}
	return s.userID, &auth.Claims{}, nil
}

// The context key is unexported, so nothing outside this package can forge an
// authenticated subject. A handler on a route that was never wrapped therefore
// gets an error rather than a zero uuid it might mistake for a real user.
func TestUserIDFromContextFailsClosed(t *testing.T) {
	if _, err := UserIDFromContext(t.Context()); err == nil {
		t.Fatal("an unwrapped context yielded a user id")
	}

	// A nil uuid written through the exported helper is still not a user.
	ctx := WithUserID(t.Context(), uuid.Nil)
	if _, err := UserIDFromContext(ctx); err == nil {
		t.Error("uuid.Nil was accepted as an authenticated subject")
	}

	want := uuid.New()
	if got, err := UserIDFromContext(WithUserID(t.Context(), want)); err != nil || got != want {
		t.Errorf("round trip = %v, %v; want %v, nil", got, err, want)
	}
}

func TestRequireAuthRejectsMalformedAuthorizationHeaders(t *testing.T) {
	log, _ := capture()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := RequireAuth(stubParser{userID: uuid.New()}, log)(next)

	headers := []struct{ name, value string }{
		{"absent", ""},
		{"scheme only", "Bearer"},
		{"scheme with no token", "Bearer "},
		{"wrong scheme", "Basic dXNlcjpwYXNz"},
		{"token with no scheme", "eyJhbGciOiJIUzI1NiJ9.e30.sig"},
	}
	for _, tc := range headers {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.value != "" {
				r.Header.Set("Authorization", tc.value)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if wa := rec.Header().Get("WWW-Authenticate"); wa == "" {
				t.Error("401 carried no WWW-Authenticate challenge")
			}
		})
	}
}

func TestRequireAuthPassesTheSubjectThrough(t *testing.T) {
	log, _ := capture()
	want := uuid.New()

	var got uuid.UUID
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := UserIDFromContext(r.Context())
		if err != nil {
			t.Errorf("handler saw no subject: %v", err)
		}
		got = id
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer anything-the-stub-accepts")
	rec := httptest.NewRecorder()
	RequireAuth(stubParser{userID: want}, log)(next).ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got != want {
		t.Errorf("subject = %v, want %v", got, want)
	}
}

// An expired token must not be logged at warn: every long-lived tab produces
// one, and warning on them trains operators to ignore the level that matters.
func TestRequireAuthLogsExpiryQuietly(t *testing.T) {
	log, buf := capture()
	h := RequireAuth(stubParser{err: auth.ErrTokenExpired}, log)(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer expired")
	h.ServeHTTP(httptest.NewRecorder(), r)

	if entry := findRecord(t, buf, "token rejected"); entry["level"] != "DEBUG" {
		t.Errorf("level = %v, want DEBUG", entry["level"])
	}
}
