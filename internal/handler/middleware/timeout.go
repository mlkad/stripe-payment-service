package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Timeout bounds how long a request may run by putting a deadline on its
// context.
//
// It deliberately does not race the handler from a watchdog goroutine the way
// http.TimeoutHandler does. That type buffers the entire response so it can
// discard it and substitute its own, which defeats http.MaxBytesReader on the
// webhook route and makes every response allocate twice. Cancelling the context
// instead reaches the places that actually block - pgx queries and the Stripe
// HTTP client both honour it - and no two goroutines ever hold the
// ResponseWriter at once.
//
// The 503 below is therefore written only after the handler has returned, and
// only if it returned without writing anything. A handler that ignores its
// context entirely cannot be interrupted here; http.Server.WriteTimeout is the
// backstop for that, which is why it must be longer than this deadline.
func Timeout(d time.Duration, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()

			rec := wrapWriter(w)
			next.ServeHTTP(rec, r.WithContext(ctx))

			if rec.wroteHeader || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return
			}
			log.WarnContext(ctx, "request deadline exceeded",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Duration("timeout", d),
			)
			writeJSONError(rec, http.StatusServiceUnavailable, "request timed out")
		})
	}
}

// writeJSONError emits the same envelope the handler package uses. It is
// duplicated here rather than imported because handler imports middleware, and
// the dependency must not run the other way.
func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":` + quoteJSON(message) + `}`))
}

// quoteJSON escapes the few characters that can appear in the fixed messages
// this package emits. encoding/json would work too, but these strings are
// compile-time constants and the error path should not allocate a marshaller.
func quoteJSON(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\\':
			out = append(out, '\\', c)
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			if c < 0x20 {
				continue
			}
			out = append(out, c)
		}
	}
	return string(append(out, '"'))
}
