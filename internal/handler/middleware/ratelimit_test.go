package middleware

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestLimiter(t *testing.T, cfg RateLimitConfig) *RateLimiter {
	t.Helper()
	log, _ := capture()
	rl := NewRateLimiter(cfg, log)
	t.Cleanup(rl.Close)
	return rl
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func callFrom(h http.Handler, remoteAddr string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	r.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestRateLimiterAllowsBurstThenThrottles(t *testing.T) {
	rl := newTestLimiter(t, RateLimitConfig{Rate: 0.1, Burst: 3})
	h := rl.Middleware(ClientKeyFunc(0))(okHandler())

	for i := 1; i <= 3; i++ {
		if rec := callFrom(h, "192.0.2.10:1234"); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, rec.Code)
		}
	}

	rec := callFrom(h, "192.0.2.10:1234")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("fourth request: status = %d, want 429", rec.Code)
	}

	retryAfter := rec.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("429 carried no Retry-After header")
	}
	seconds, err := strconv.Atoi(retryAfter)
	if err != nil || seconds < 1 {
		t.Errorf("Retry-After = %q, want a positive integer number of seconds", retryAfter)
	}
}

// Throttling one client must not affect another, or a single attacker locks
// out the whole service.
func TestRateLimiterIsPerClient(t *testing.T) {
	rl := newTestLimiter(t, RateLimitConfig{Rate: 0.1, Burst: 2})
	h := rl.Middleware(ClientKeyFunc(0))(okHandler())

	for i := 0; i < 5; i++ {
		callFrom(h, "192.0.2.10:1111")
	}
	if rec := callFrom(h, "192.0.2.10:1111"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("noisy client: status = %d, want 429", rec.Code)
	}
	if rec := callFrom(h, "198.51.100.7:2222"); rec.Code != http.StatusOK {
		t.Errorf("quiet client: status = %d, want 200", rec.Code)
	}
}

// A rejected request must not consume a token. If it did, a client hammering
// the endpoint would push their own recovery further out with every attempt and
// never come back.
func TestRateLimiterRejectionDoesNotDeferRecovery(t *testing.T) {
	rl := newTestLimiter(t, RateLimitConfig{Rate: 100, Burst: 1})
	const key = "203.0.113.5"
	start := time.Now()

	if allowed, _ := rl.allow(key, start); !allowed {
		t.Fatal("first request was denied")
	}
	// Twenty denials in the same instant.
	for i := 0; i < 20; i++ {
		if allowed, _ := rl.allow(key, start); allowed {
			t.Fatalf("request %d was allowed past the burst", i)
		}
	}
	// One token refills in 10ms at 100/s. If the denials had consumed tokens,
	// the client would still be blocked here.
	if allowed, delay := rl.allow(key, start.Add(15*time.Millisecond)); !allowed {
		t.Errorf("still throttled after a refill window; denials consumed tokens (delay=%v)", delay)
	}
}

// IPv6 clients get a /64, not an address. A /64 is one customer allocation, and
// limiting per address would let a single attacker cycle through billions of
// them - defeating the limit and filling the bucket map at the same time.
func TestRateLimiterGroupsIPv6BySubnet(t *testing.T) {
	rl := newTestLimiter(t, RateLimitConfig{Rate: 0.1, Burst: 2})
	h := rl.Middleware(ClientKeyFunc(0))(okHandler())

	// Three different addresses inside one /64.
	addrs := []string{
		"[2001:db8:1:1::1]:1000",
		"[2001:db8:1:1::2]:1000",
		"[2001:db8:1:1::3]:1000",
	}
	for _, a := range addrs {
		callFrom(h, a)
	}
	if rec := callFrom(h, "[2001:db8:1:1::4]:1000"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429: addresses in one /64 are not sharing a bucket", rec.Code)
	}
	// A different /64 is a different customer.
	if rec := callFrom(h, "[2001:db8:1:2::1]:1000"); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for a separate /64", rec.Code)
	}
}

func TestNormaliseIP(t *testing.T) {
	tests := []struct{ in, want string }{
		{"192.0.2.10", "192.0.2.10"},
		{"::ffff:192.0.2.10", "192.0.2.10"},
		{"2001:db8:1:1::abcd", "2001:db8:1:1::/64"},
		{"2001:db8:1:1:ffff:ffff:ffff:ffff", "2001:db8:1:1::/64"},
		{"not-an-ip", "not-an-ip"},
	}
	for _, tt := range tests {
		if got := normaliseIP(tt.in); got != tt.want {
			t.Errorf("normaliseIP(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// X-Forwarded-For is appended to by each hop, so with N trusted proxies the
// client is the Nth entry from the right. Reading the leftmost entry - the
// common shortcut - lets a caller send a fresh header per request and get an
// unlimited allowance.
func TestClientKeyFuncResistsForwardedForSpoofing(t *testing.T) {
	tests := []struct {
		name           string
		trustedProxies int
		forwardedFor   string
		remoteAddr     string
		want           string
	}{
		{"no proxy, header ignored", 0, "1.2.3.4", "192.0.2.10:1234", "192.0.2.10"},
		{"no proxy, spoofed header ignored", 0, "attacker-chosen", "192.0.2.10:1234", "192.0.2.10"},
		{"one proxy, real client read", 1, "203.0.113.9", "192.0.2.10:1234", "203.0.113.9"},
		{"one proxy, prepended entries ignored", 1, "9.9.9.9, 203.0.113.9", "192.0.2.10:1234", "203.0.113.9"},
		{"two proxies", 2, "203.0.113.9, 10.0.0.1", "192.0.2.10:1234", "203.0.113.9"},
		{"shorter chain than configured falls back to peer", 3, "203.0.113.9", "192.0.2.10:1234", "192.0.2.10"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.forwardedFor != "" {
				r.Header.Set("X-Forwarded-For", tt.forwardedFor)
			}
			if got := ClientKeyFunc(tt.trustedProxies)(r); got != tt.want {
				t.Errorf("key = %q, want %q", got, tt.want)
			}
		})
	}
}

// The limiter must not become a memory exhaustion vector itself.
func TestRateLimiterBoundsItsMap(t *testing.T) {
	rl := newTestLimiter(t, RateLimitConfig{Rate: 100, Burst: 10, MaxEntries: 100})
	now := time.Now()

	for i := 0; i < 500; i++ {
		rl.allow("client-"+strconv.Itoa(i), now)
	}

	rl.mu.Lock()
	size := len(rl.buckets)
	rl.mu.Unlock()

	if size > 100 {
		t.Errorf("map holds %d entries, want at most the 100 configured", size)
	}
}

func TestRateLimiterPrunesIdleBuckets(t *testing.T) {
	rl := newTestLimiter(t, RateLimitConfig{Rate: 100, Burst: 10, TTL: time.Minute})
	now := time.Now()

	rl.allow("stale-client", now)
	rl.allow("fresh-client", now.Add(90*time.Second))
	rl.prune(now.Add(2 * time.Minute))

	rl.mu.Lock()
	_, staleKept := rl.buckets["stale-client"]
	_, freshKept := rl.buckets["fresh-client"]
	rl.mu.Unlock()

	if staleKept {
		t.Error("an idle bucket survived past its TTL")
	}
	if !freshKept {
		t.Error("a recently used bucket was pruned")
	}
}

func TestRateLimiterIsConcurrencySafe(t *testing.T) {
	rl := newTestLimiter(t, RateLimitConfig{Rate: 1000, Burst: 50})
	h := rl.Middleware(ClientKeyFunc(0))(okHandler())

	var allowed, throttled atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			// Half share one address, half get their own.
			addr := "192.0.2.1:1000"
			if i%2 == 0 {
				addr = "198.51.100." + strconv.Itoa(i) + ":1000"
			}
			if callFrom(h, addr).Code == http.StatusOK {
				allowed.Add(1)
			} else {
				throttled.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if total := allowed.Load() + throttled.Load(); total != 100 {
		t.Errorf("accounted for %d of 100 requests", total)
	}
	if allowed.Load() == 0 {
		t.Error("every concurrent request was throttled")
	}
}
