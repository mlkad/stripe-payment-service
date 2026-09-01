// Package middleware holds the cross-cutting HTTP concerns: correlation,
// logging, panic recovery and deadlines.
//
// Order matters and is documented where the chain is assembled, in
// handler.NewRouter. Nothing here knows about Stripe or the database.
package middleware

import "net/http"

// responseRecorder captures the status code and body size for the access log,
// and lets the timeout middleware tell "the handler answered" from "the handler
// gave up without writing".
type responseRecorder struct {
	http.ResponseWriter
	status      int
	written     int64
	wroteHeader bool
}

// wrapWriter reuses a recorder already present in the chain rather than nesting
// a second one, so status and byte counts stay attributed to a single object no
// matter how many middlewares ask for it.
func wrapWriter(w http.ResponseWriter) *responseRecorder {
	if rr, ok := w.(*responseRecorder); ok {
		return rr
	}
	return &responseRecorder{ResponseWriter: w, status: http.StatusOK}
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, so flushing
// and per-request deadline control keep working through the wrapper.
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
