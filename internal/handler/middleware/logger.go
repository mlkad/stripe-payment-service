package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"time"
)

// QuietPaths are logged at debug level. Health probes fire every few seconds and
// bury real traffic at info.
var QuietPaths = map[string]bool{
	"/healthz": true,
	"/livez":   true,
}

// AccessLog records one line per request at completion.
//
// It must sit outside Recoverer. The status is read after next.ServeHTTP
// returns, so if a panic unwinds through this middleware there is nothing to
// report yet - the recovered 500 is written further out, and the line would
// either be missing or claim 200.
func AccessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := wrapWriter(w)

			next.ServeHTTP(rec, r)

			level := slog.LevelInfo
			switch {
			case rec.status >= http.StatusInternalServerError:
				level = slog.LevelError
			case rec.status >= http.StatusBadRequest:
				level = slog.LevelWarn
			case QuietPaths[r.URL.Path]:
				level = slog.LevelDebug
			}

			// Query strings are omitted deliberately: they carry session ids and
			// tokens on redirect-back URLs, and this line is shipped off-host.
			log.LogAttrs(r.Context(), level, "request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int64("bytes", rec.written),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote_addr", ClientIP(r)),
				slog.String("user_agent", r.UserAgent()),
			)
		})
	}
}

// ClientIP reads RemoteAddr rather than X-Forwarded-For, which is
// caller-controlled unless a trusted proxy overwrites it. Treating the header as
// identity would let anyone forge the audit trail.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
