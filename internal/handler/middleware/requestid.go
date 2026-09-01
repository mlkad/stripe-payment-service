package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/mlkad/stripe-payment-service/internal/logger"
)

const (
	RequestIDHeader = "X-Request-Id"

	// maxInboundRequestIDLen bounds what an untrusted caller can put in a log
	// field. Anything longer is replaced rather than truncated: a client that
	// sends 4 KiB of header is not correlating traces.
	maxInboundRequestIDLen = 128
)

// RequestID honours an inbound correlation id so a trace survives a proxy hop,
// and otherwise mints one. The id goes on the context, where the logger's
// handler picks it up for every record, and on the response header so a caller
// can quote it in a bug report.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" || len(id) > maxInboundRequestIDLen || !isPrintableASCII(id) {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(logger.WithRequestID(r.Context(), id)))
	})
}

// isPrintableASCII rejects control characters and newlines. An id is echoed into
// a response header and into every log line for the request; a caller-supplied
// newline would let it forge additional log records.
func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; a timestamp keeps the request
		// traceable rather than dropping correlation entirely.
		return fmt.Sprintf("ts-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
