package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mlkad/stripe-payment-service/internal/logger"
)

// capture returns a logger writing JSON into buf, plus a helper that finds the
// first record with the given message.
func capture() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

func findRecord(t *testing.T, buf *bytes.Buffer, msg string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if rec["msg"] == msg {
			return rec
		}
	}
	t.Fatalf("no log record with msg %q in:\n%s", msg, buf)
	return nil
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func panicking() http.Handler {
	return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
}

// The access log must sit outside the recoverer.
//
// With that ordering the recovered 500 is written before AccessLog reads the
// status. Reversed, the panic unwinds through AccessLog before anything is
// written, and the request is recorded as a success - or not at all.
func TestAccessLogOutsideRecovererRecordsThePanicStatus(t *testing.T) {
	log, buf := capture()

	chain := AccessLog(log)(Recoverer(log)(panicking()))
	rec := serve(chain, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	entry := findRecord(t, buf, "request")
	if got := entry["status"]; got != float64(http.StatusInternalServerError) {
		t.Errorf("access log status = %v, want 500", got)
	}
	if entry["level"] != "ERROR" {
		t.Errorf("access log level = %v, want ERROR", entry["level"])
	}
}

// The negative control for the test above: the reversed chain misreports the
// panic. This documents why the order in NewRouter is not interchangeable.
func TestRecovererOutsideAccessLogMisreportsThePanic(t *testing.T) {
	log, buf := capture()

	chain := Recoverer(log)(AccessLog(log)(panicking()))
	serve(chain, httptest.NewRequest(http.MethodGet, "/boom", nil))

	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil || rec["msg"] != "request" {
			continue
		}
		if rec["status"] == float64(http.StatusInternalServerError) {
			t.Fatal("reversed order recorded 500; the ordering constraint no longer holds " +
				"and the comment in NewRouter should be revisited")
		}
	}
}

func TestRecovererLetsAbortHandlerThrough(t *testing.T) {
	log, _ := capture()

	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler to propagate", rec)
		}
	}()

	h := Recoverer(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	serve(h, httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestRequestID(t *testing.T) {
	var seen string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = logger.RequestIDFromContext(r.Context())
	})

	tests := []struct {
		name     string
		inbound  string
		wantKept bool
	}{
		{"mints when absent", "", false},
		{"honours an inbound id", "trace-abc-123", true},
		{"replaces an overlong id", strings.Repeat("x", maxInboundRequestIDLen+1), false},
		{"replaces an id with a newline", "abc\ndef", false},
		{"replaces an id with a control character", "abc\x00def", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.inbound != "" {
				req.Header.Set(RequestIDHeader, tt.inbound)
			}
			rec := serve(RequestID(next), req)

			if seen == "" {
				t.Fatal("no request id reached the handler")
			}
			if rec.Header().Get(RequestIDHeader) != seen {
				t.Errorf("response header %q != context id %q", rec.Header().Get(RequestIDHeader), seen)
			}
			if tt.wantKept && seen != tt.inbound {
				t.Errorf("id = %q, want the inbound %q", seen, tt.inbound)
			}
			if !tt.wantKept && seen == tt.inbound {
				t.Errorf("id = %q, want a freshly minted one", seen)
			}
			if !isPrintableASCII(seen) {
				t.Errorf("id %q reached the log with control characters", seen)
			}
		})
	}
}

// The deadline must reach the handler's context, which is what actually stops
// pgx and the Stripe client.
func TestTimeoutCancelsTheHandlerContext(t *testing.T) {
	log, buf := capture()

	h := Timeout(20*time.Millisecond, log)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	rec := serve(h, httptest.NewRequest(http.MethodGet, "/slow", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want JSON", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("timeout body is not JSON: %q", rec.Body)
	}
	if body["error"] == "" {
		t.Errorf("timeout body has no error field: %v", body)
	}
	findRecord(t, buf, "request deadline exceeded")
}

// A handler that answered before the deadline keeps its response: the timeout
// must never append a second status to a reply already in flight.
func TestTimeoutDoesNotOverwriteAHandlerThatAnswered(t *testing.T) {
	log, _ := capture()

	h := Timeout(20*time.Millisecond, log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
		<-r.Context().Done() // outlive the deadline after replying
	}))
	rec := serve(h, httptest.NewRequest(http.MethodGet, "/slow-after-write", nil))

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201 preserved", rec.Code)
	}
	if body := rec.Body.String(); body != `{"ok":true}` {
		t.Errorf("body = %q, want the handler's own response", body)
	}
}

func TestTimeoutLeavesFastHandlersAlone(t *testing.T) {
	log, _ := capture()

	h := Timeout(time.Second, log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	if rec := serve(h, httptest.NewRequest(http.MethodGet, "/fast", nil)); rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
}

// Nesting must not produce two recorders: the outer one has to observe what the
// inner middleware and the handler actually wrote.
func TestWrapWriterReusesAnExistingRecorder(t *testing.T) {
	base := httptest.NewRecorder()
	first := wrapWriter(base)
	if second := wrapWriter(first); second != first {
		t.Error("wrapWriter nested a second recorder instead of reusing the first")
	}
}

func TestQuietPathsAreLoggedAtDebug(t *testing.T) {
	log, buf := capture()

	h := AccessLog(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	serve(h, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if got := findRecord(t, buf, "request")["level"]; got != "DEBUG" {
		t.Errorf("level = %v, want DEBUG: health probes would bury real traffic at info", got)
	}
}

func TestAccessLogOmitsTheQueryString(t *testing.T) {
	log, buf := capture()

	h := AccessLog(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	serve(h, httptest.NewRequest(http.MethodGet, "/api/v1/thing?session_id=cs_secret", nil))

	if strings.Contains(buf.String(), "cs_secret") {
		t.Error("query string reached the access log; redirect-back URLs carry session ids")
	}
}
