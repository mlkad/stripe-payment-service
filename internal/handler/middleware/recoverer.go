package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recoverer converts a panic into a 500 and keeps the process alive.
//
// It belongs inside AccessLog: the recovered status must be written before the
// access log reads it. It also belongs inside Timeout, so that a panic still
// unwinds through a live context.
func Recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// http.ErrAbortHandler is the documented way to abandon a
				// response. Reporting it as a crash would fill the log with
				// noise from clients that hung up mid-body.
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}
				// A panic after the client disconnected is noise, not a defect.
				if errors.Is(r.Context().Err(), context.Canceled) {
					return
				}

				log.ErrorContext(r.Context(), "panic recovered",
					slog.Any("panic", rec),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("stack", string(debug.Stack())),
				)
				writeJSONError(w, http.StatusInternalServerError, "internal server error")
			}()
			next.ServeHTTP(w, r)
		})
	}
}
