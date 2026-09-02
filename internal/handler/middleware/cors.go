package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CORSConfig lists the browser origins allowed to call this API.
type CORSConfig struct {
	// AllowedOrigins is matched exactly, scheme and port included. There is no
	// wildcard and no pattern matching: this API is called with credentials, and
	// reflecting an arbitrary Origin back would let any site on the internet
	// read a logged-in user's billing data.
	AllowedOrigins []string
	MaxAge         time.Duration
}

const defaultCORSMaxAge = 10 * time.Minute

var (
	corsAllowedMethods = strings.Join([]string{http.MethodGet, http.MethodPost, http.MethodOptions}, ", ")
	corsAllowedHeaders = strings.Join([]string{"Content-Type", "Authorization", RequestIDHeader}, ", ")
)

// CORS answers preflights and marks cross-origin responses.
//
// An unlisted origin is not rejected here: the request proceeds without the
// CORS headers, and the browser blocks the response. Returning 403 instead
// would break non-browser clients, which never send Origin and are not subject
// to the same-origin policy in the first place.
func CORS(cfg CORSConfig) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		if o = strings.TrimSpace(strings.TrimSuffix(o, "/")); o != "" {
			allowed[o] = true
		}
	}
	maxAge := cfg.MaxAge
	if maxAge <= 0 {
		maxAge = defaultCORSMaxAge
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && allowed[origin] {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Set("Access-Control-Expose-Headers", RequestIDHeader)
				// The response varies by Origin, so a shared cache must not serve
				// one origin's response to another.
				h.Add("Vary", "Origin")

				if r.Method == http.MethodOptions {
					h.Set("Access-Control-Allow-Methods", corsAllowedMethods)
					h.Set("Access-Control-Allow-Headers", corsAllowedHeaders)
					h.Set("Access-Control-Max-Age", strconv.Itoa(int(maxAge.Seconds())))
					h.Add("Vary", "Access-Control-Request-Method")
					h.Add("Vary", "Access-Control-Request-Headers")
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
