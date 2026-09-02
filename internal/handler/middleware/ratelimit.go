package middleware

import (
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimitConfig describes one limiter.
type RateLimitConfig struct {
	// Rate is the sustained refill, in requests per second. A legitimate human
	// signing in needs a handful of attempts, not a steady stream.
	Rate float64

	// Burst is how many requests may arrive at once before throttling starts.
	Burst int

	// TTL is how long an idle bucket is kept. Too short and an attacker resets
	// their allowance by pausing; too long and the map holds addresses nobody
	// is using.
	TTL time.Duration

	// MaxEntries caps the map. Without it the limiter is itself a memory
	// exhaustion vector: a single /64 gives an attacker 2^64 source addresses
	// to allocate buckets for.
	MaxEntries int
}

const (
	defaultRateLimitTTL        = 15 * time.Minute
	defaultRateLimitMaxEntries = 50_000
	cleanupInterval            = time.Minute
)

func (c RateLimitConfig) withDefaults() RateLimitConfig {
	if c.Rate <= 0 {
		c.Rate = 0.2 // one request every five seconds, sustained
	}
	if c.Burst <= 0 {
		c.Burst = 5
	}
	if c.TTL <= 0 {
		c.TTL = defaultRateLimitTTL
	}
	if c.MaxEntries <= 0 {
		c.MaxEntries = defaultRateLimitMaxEntries
	}
	return c
}

type bucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// RateLimiter is a per-client token bucket with a bounded, self-pruning map.
//
// Deliberately in-memory and therefore per-instance: behind N replicas the
// effective limit is N times the configured one. That is the right trade at
// this scale - a shared Redis counter adds a network round trip and a new
// failure mode to the login path - but it is a ceiling, not a floor, and a
// horizontally scaled deployment should move the state out.
type RateLimiter struct {
	cfg RateLimitConfig
	log *slog.Logger

	mu      sync.Mutex
	buckets map[string]*bucket

	stop chan struct{}
	once sync.Once
}

func NewRateLimiter(cfg RateLimitConfig, log *slog.Logger) *RateLimiter {
	rl := &RateLimiter{
		cfg:     cfg.withDefaults(),
		log:     log,
		buckets: make(map[string]*bucket),
		stop:    make(chan struct{}),
	}
	go rl.janitor()
	return rl
}

// Close stops the cleanup goroutine. Safe to call more than once.
func (rl *RateLimiter) Close() {
	rl.once.Do(func() { close(rl.stop) })
}

func (rl *RateLimiter) janitor() {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-rl.stop:
			return
		case <-ticker.C:
			rl.prune(time.Now())
		}
	}
}

func (rl *RateLimiter) prune(now time.Time) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for key, b := range rl.buckets {
		if now.Sub(b.lastSeen) > rl.cfg.TTL {
			delete(rl.buckets, key)
		}
	}
}

// allow reports whether the request may proceed, and if not, how long until it
// could.
func (rl *RateLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	rl.mu.Lock()
	b, ok := rl.buckets[key]
	if !ok {
		// At capacity, drop the whole map rather than evicting one entry at
		// random. Picking a victim would let an attacker who can spray
		// addresses evict a legitimate client's bucket on demand, which turns
		// the limiter into a way to *clear* someone's rate limit. A periodic
		// reset is blunt, but it fails toward availability and cannot be
		// steered.
		if len(rl.buckets) >= rl.cfg.MaxEntries {
			rl.log.Warn("rate limiter at capacity, resetting all buckets",
				slog.Int("max_entries", rl.cfg.MaxEntries))
			rl.buckets = make(map[string]*bucket, rl.cfg.MaxEntries/2)
		}
		b = &bucket{limiter: rate.NewLimiter(rate.Limit(rl.cfg.Rate), rl.cfg.Burst)}
		rl.buckets[key] = b
	}
	b.lastSeen = now
	limiter := b.limiter
	rl.mu.Unlock()

	// ReserveN consumes a token whether or not the caller proceeds, so a
	// rejected request must cancel its reservation - otherwise every blocked
	// attempt pushes the client's recovery further out and a burst of denials
	// becomes an indefinite lockout.
	reservation := limiter.ReserveN(now, 1)
	if !reservation.OK() {
		return false, rl.retryAfter()
	}
	if delay := reservation.DelayFrom(now); delay > 0 {
		reservation.CancelAt(now)
		return false, delay
	}
	return true, 0
}

// retryAfter is the fallback when a reservation cannot be satisfied at all,
// which happens when burst is smaller than the request size.
func (rl *RateLimiter) retryAfter() time.Duration {
	if rl.cfg.Rate <= 0 {
		return time.Minute
	}
	return time.Duration(float64(time.Second) / rl.cfg.Rate)
}

// Middleware throttles by client address.
//
// It is mounted only on the credential endpoints. Those are the ones where an
// unauthenticated caller can make the server do expensive work - a bcrypt
// comparison at cost 12 is ~250ms of CPU, so a few hundred concurrent requests
// saturate a box - and the ones where unlimited attempts mean unlimited
// password guesses.
func (rl *RateLimiter) Middleware(clientKey func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := clientKey(r)
			allowed, retryAfter := rl.allow(key, time.Now())
			if allowed {
				next.ServeHTTP(w, r)
				return
			}

			seconds := int(math.Ceil(retryAfter.Seconds()))
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))

			// The key is logged, not the raw address, so an IPv6 client is
			// recorded as the /64 the limiter actually groups it by.
			rl.log.WarnContext(r.Context(), "rate limit exceeded",
				slog.String("client", key),
				slog.String("path", r.URL.Path),
				slog.Int("retry_after_seconds", seconds))

			writeJSONError(w, http.StatusTooManyRequests, "too many requests, please slow down")
		})
	}
}

// ClientKeyFunc builds the bucket key for a request.
//
// trustedProxies is the number of reverse proxies in front of this service. It
// must be exact. X-Forwarded-For is appended to by each hop, so with N trusted
// proxies the client address is the Nth entry from the right; everything to the
// left of it was supplied by the caller and can say anything. Reading the
// leftmost entry - the common shortcut - lets an attacker send a fresh
// X-Forwarded-For per request and get an unlimited allowance.
//
// Zero means no proxy, and RemoteAddr is used directly.
func ClientKeyFunc(trustedProxies int) func(*http.Request) string {
	return func(r *http.Request) string {
		return normaliseIP(clientAddr(r, trustedProxies))
	}
}

func clientAddr(r *http.Request, trustedProxies int) string {
	direct := ClientIP(r)
	if trustedProxies <= 0 {
		return direct
	}

	forwarded := r.Header.Values("X-Forwarded-For")
	var hops []string
	for _, header := range forwarded {
		for _, part := range splitAndTrim(header) {
			hops = append(hops, part)
		}
	}
	// The direct peer is the last hop and is not in the header.
	hops = append(hops, direct)

	idx := len(hops) - 1 - trustedProxies
	if idx < 0 {
		// Fewer hops than configured proxies: the request did not come through
		// the expected chain. Fall back to the peer, which is always genuine.
		return direct
	}
	return hops[idx]
}

func splitAndTrim(header string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(header); i++ {
		if i == len(header) || header[i] == ',' {
			if part := trimSpace(header[start:i]); part != "" {
				out = append(out, part)
			}
			start = i + 1
		}
	}
	return out
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// normaliseIP groups IPv6 addresses by /64.
//
// A single residential IPv6 allocation is typically a /64 or larger, so
// limiting by exact address lets one attacker cycle through billions of
// addresses at no cost - defeating the limit and exhausting the bucket map at
// the same time. IPv4 is limited per address, since one address is one client.
func normaliseIP(addr string) string {
	ip := net.ParseIP(addr)
	if ip == nil {
		// Not an address at all (a proxy sent a hostname, or the header was
		// garbage). Key on the literal so it is still bounded.
		return addr
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}
