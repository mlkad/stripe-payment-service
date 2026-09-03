// Package config loads and validates all runtime configuration from the
// environment.
//
// Two rules drive the design:
//
//  1. Fail fast, and fail completely. Load reports every problem it finds in one
//     error rather than stopping at the first, so a misconfigured deployment is
//     fixed in one pass instead of one restart per typo.
//  2. Secrets never stringify. Sensitive values use the Secret type, which
//     redacts itself through fmt, slog and encoding/json. Leaking a Stripe key
//     into a log aggregator is not recoverable by rotating alone - it is an
//     incident - so the type system prevents it rather than a code review.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const redacted = "[REDACTED]"

// Secret is a configuration value that must never reach a log, an error string,
// or a JSON response. It implements fmt.Stringer, slog.LogValuer and
// json.Marshaler so that every ordinary path prints the placeholder; the real
// value is available only through the explicit Reveal call.
type Secret string

// Reveal returns the underlying value. Call it only when handing the secret to
// the client that needs it.
func (s Secret) Reveal() string { return string(s) }

func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return redacted
}

// GoString covers the %#v verb, which does not consult String.
func (s Secret) GoString() string { return s.String() }

func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

func (s Secret) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(s.String())), nil }

// Environment is the deployment tier. It gates the production safety checks in
// Validate.
type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvStaging     Environment = "staging"
	EnvProduction  Environment = "production"
)

func (e Environment) IsProduction() bool { return e == EnvProduction }

func (e Environment) valid() bool {
	switch e {
	case EnvDevelopment, EnvStaging, EnvProduction:
		return true
	}
	return false
}

// Config is the fully validated configuration for the process.
type Config struct {
	App       App
	HTTP      HTTP
	Database  Database
	Stripe    Stripe
	Auth      Auth
	Sweeper   Sweeper
	Retention Retention
	Log       Log
}

type Auth struct {
	// JWTSecret signs and verifies access tokens. Secret: anyone holding it can
	// mint a token for any user id.
	JWTSecret Secret

	// JWTIssuer and JWTAudience are verified on every parse, so a token minted
	// for staging cannot be replayed against production.
	JWTIssuer   string
	JWTAudience string

	// AccessTokenTTL bounds the damage from a stolen token. Tokens are stateless
	// and cannot be revoked before they expire, which is the reason to keep this
	// short rather than a reason to be relaxed about it.
	AccessTokenTTL time.Duration

	// RefreshTokenTTL is how long a session survives without renewal. Long,
	// because the refresh token is revocable - which is what makes a short
	// access token affordable.
	RefreshTokenTTL time.Duration

	// CookieSecure marks the refresh cookie Secure. False only for plain-HTTP
	// local development; a refresh token over HTTP is readable in transit.
	CookieSecure bool

	// CookieDomain is empty for a host-only cookie, which is the safer default:
	// a domain cookie reaches every subdomain.
	CookieDomain string

	BcryptCost int
}

type App struct {
	Name            string
	Environment     Environment
	ShutdownTimeout time.Duration
}

type HTTP struct {
	Port              int
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration

	// APITimeout and WebhookTimeout are the per-request deadlines the router
	// applies. Both must stay below WriteTimeout: the server closes the
	// connection at WriteTimeout regardless, so a longer request deadline would
	// never be reached and the caller would see a dropped connection instead of
	// the 503 the middleware wants to send.
	APITimeout     time.Duration
	WebhookTimeout time.Duration

	// CORSAllowedOrigins lists the browser origins permitted to call the API,
	// matched exactly. Empty disables cross-origin access entirely, which is the
	// right default for a deployment fronted by the same domain as its UI.
	CORSAllowedOrigins []string

	// TrustedProxies is the number of reverse proxies in front of this service.
	// It must be exact: the rate limiter counts that many hops from the right
	// of X-Forwarded-For to find the client, and everything further left is
	// caller-supplied. Too high and an attacker picks their own bucket; too low
	// and every client behind the proxy shares one.
	TrustedProxies int

	// AuthRateLimit throttles the credential endpoints.
	AuthRateLimitRPS   float64
	AuthRateLimitBurst int
}

// Addr renders the listen address for net/http.
func (h HTTP) Addr() string { return ":" + strconv.Itoa(h.Port) }

type Database struct {
	// DSN is secret: libpq connection strings carry the password inline.
	DSN                Secret
	MaxConns           int32
	MinConns           int32
	MaxConnLifetime    time.Duration
	MaxConnIdleTime    time.Duration
	HealthCheckPeriod  time.Duration
	ConnectTimeout     time.Duration
	StatementTimeout   time.Duration
	IdleInTxTimeout    time.Duration
	SlowQueryThreshold time.Duration
}

type Stripe struct {
	SecretKey         Secret
	WebhookSecret     Secret
	PublishableKey    string
	APIVersion        string
	WebhookTolerance  time.Duration
	MaxNetworkRetries int
	HTTPTimeout       time.Duration

	// IgnoreAPIVersionMismatch disables the SDK's check that an event was
	// rendered by the API release train it deserialises against. Enable it only
	// while migrating a webhook endpoint between versions.
	IgnoreAPIVersionMismatch bool

	CheckoutSuccessURL string
	CheckoutCancelURL  string

	// CheckoutReturnURL is where Stripe sends the browser after an embedded
	// checkout completes. Only needed when the frontend uses Stripe Elements.
	CheckoutReturnURL string

	// PortalReturnURL is where Stripe sends the browser back after the customer
	// finishes in the billing portal.
	PortalReturnURL string

	// AllowedPriceIDs restricts what a checkout request may ask for. Empty means
	// unrestricted, which Validate refuses in production: the price id arrives
	// in the request body, so without an allowlist any caller can subscribe
	// against any price in the account.
	AllowedPriceIDs []string
}

// Sweeper configures the background job that recovers and reports failed
// webhook deliveries.
type Sweeper struct {
	Enabled         bool
	Interval        time.Duration
	MaxAttempts     int
	BaseBackoff     time.Duration
	MaxBackoff      time.Duration
	BatchSize       int
	StaleClaimAfter time.Duration

	// AlertAfter is how old the oldest unsettled event may get before the
	// sweep logs at error level.
	AlertAfter time.Duration
}

// Retention configures data minimisation on stored webhook payloads.
type Retention struct {
	Enabled  bool
	Interval time.Duration

	// SettledPayloadAfter is how long a succeeded or skipped event keeps its
	// payload.
	SettledPayloadAfter time.Duration

	// UnsettledPayloadAfter is the outer bound for a failed event. It must
	// exceed the time the sweeper needs to exhaust its retry budget, or
	// recoverable events lose their payload before anything can replay them.
	UnsettledPayloadAfter time.Duration

	BatchSize int

	// RefreshTokenGrace is how long an expired refresh token is kept before
	// deletion, so reuse detection still fires on one that expired between
	// theft and use.
	RefreshTokenGrace time.Duration
}

type Log struct {
	Level     string
	Format    string
	AddSource bool
}

// LogValue renders the configuration for the boot log with every secret
// redacted, so operators can confirm what the process actually loaded.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("env", string(c.App.Environment)),
		slog.String("http_addr", c.HTTP.Addr()),
		slog.Int("db_max_conns", int(c.Database.MaxConns)),
		slog.Int("db_min_conns", int(c.Database.MinConns)),
		slog.String("db_statement_timeout", c.Database.StatementTimeout.String()),
		slog.String("stripe_api_version", c.Stripe.APIVersion),
		slog.String("stripe_key_mode", stripeKeyMode(c.Stripe.SecretKey)),
		slog.String("jwt_issuer", c.Auth.JWTIssuer),
		slog.String("jwt_ttl", c.Auth.AccessTokenTTL.String()),
		slog.Int("bcrypt_cost", c.Auth.BcryptCost),
		slog.String("log_level", c.Log.Level),
		slog.String("log_format", c.Log.Format),
		slog.String("shutdown_timeout", c.App.ShutdownTimeout.String()),
	)
}

// stripeKeyMode reports live/test without disclosing the key itself.
func stripeKeyMode(key Secret) string {
	switch {
	case strings.Contains(key.Reveal(), "_live_"):
		return "live"
	case strings.Contains(key.Reveal(), "_test_"):
		return "test"
	default:
		return "unknown"
	}
}

// Load reads configuration from the environment and validates it. The returned
// error aggregates every problem found.
func Load() (*Config, error) {
	l := &loader{}

	cfg := &Config{
		App: App{
			Name:            l.str("APP_NAME", "stripe-payment-service"),
			Environment:     Environment(strings.ToLower(l.str("APP_ENV", string(EnvDevelopment)))),
			ShutdownTimeout: l.duration("SHUTDOWN_TIMEOUT", 15*time.Second),
		},
		HTTP: HTTP{
			Port:              l.intVal("HTTP_PORT", 8080),
			ReadHeaderTimeout: l.duration("HTTP_READ_HEADER_TIMEOUT", 5*time.Second),
			ReadTimeout:       l.duration("HTTP_READ_TIMEOUT", 15*time.Second),
			WriteTimeout:      l.duration("HTTP_WRITE_TIMEOUT", 30*time.Second),
			IdleTimeout:       l.duration("HTTP_IDLE_TIMEOUT", 60*time.Second),
			APITimeout:        l.duration("HTTP_API_TIMEOUT", 10*time.Second),
			WebhookTimeout:    l.duration("HTTP_WEBHOOK_TIMEOUT", 25*time.Second),

			CORSAllowedOrigins: l.csv("CORS_ALLOWED_ORIGINS"),
			TrustedProxies:     l.intVal("TRUSTED_PROXIES", 0),
			AuthRateLimitRPS:   l.float("AUTH_RATE_LIMIT_RPS", 0.2),
			AuthRateLimitBurst: l.intVal("AUTH_RATE_LIMIT_BURST", 5),
		},
		Database: Database{
			DSN:                Secret(l.required("DATABASE_URL")),
			MaxConns:           int32(l.intVal("DB_MAX_CONNS", 25)),
			MinConns:           int32(l.intVal("DB_MIN_CONNS", 2)),
			MaxConnLifetime:    l.duration("DB_MAX_CONN_LIFETIME", time.Hour),
			MaxConnIdleTime:    l.duration("DB_MAX_CONN_IDLE_TIME", 30*time.Minute),
			HealthCheckPeriod:  l.duration("DB_HEALTH_CHECK_PERIOD", time.Minute),
			ConnectTimeout:     l.duration("DB_CONNECT_TIMEOUT", 10*time.Second),
			StatementTimeout:   l.duration("DB_STATEMENT_TIMEOUT", 30*time.Second),
			IdleInTxTimeout:    l.duration("DB_IDLE_IN_TX_TIMEOUT", 60*time.Second),
			SlowQueryThreshold: l.duration("DB_SLOW_QUERY_THRESHOLD", 200*time.Millisecond),
		},
		Stripe: Stripe{
			SecretKey:         Secret(l.required("STRIPE_SECRET_KEY")),
			WebhookSecret:     Secret(l.required("STRIPE_WEBHOOK_SECRET")),
			PublishableKey:    l.str("STRIPE_PUBLISHABLE_KEY", ""),
			APIVersion:        l.str("STRIPE_API_VERSION", "2026-08-26.dahlia"),
			WebhookTolerance:  l.duration("STRIPE_WEBHOOK_TOLERANCE", 5*time.Minute),
			MaxNetworkRetries: l.intVal("STRIPE_MAX_NETWORK_RETRIES", 3),
			HTTPTimeout:       l.duration("STRIPE_HTTP_TIMEOUT", 20*time.Second),

			IgnoreAPIVersionMismatch: l.boolVal("STRIPE_IGNORE_API_VERSION_MISMATCH", false),

			CheckoutSuccessURL: l.str("STRIPE_CHECKOUT_SUCCESS_URL", "http://localhost:3000/billing/success?session_id={CHECKOUT_SESSION_ID}"),
			CheckoutCancelURL:  l.str("STRIPE_CHECKOUT_CANCEL_URL", "http://localhost:3000/billing/cancel"),
			CheckoutReturnURL:  l.str("STRIPE_CHECKOUT_RETURN_URL", "http://localhost:5173/billing/return?session_id={CHECKOUT_SESSION_ID}"),
			PortalReturnURL:    l.str("STRIPE_PORTAL_RETURN_URL", "http://localhost:5173/"),
			AllowedPriceIDs:    l.csv("STRIPE_ALLOWED_PRICE_IDS"),
		},
		Auth: Auth{
			JWTSecret:   Secret(l.required("JWT_SECRET")),
			JWTIssuer:   l.str("JWT_ISSUER", "stripe-payment-service"),
			JWTAudience: l.str("JWT_AUDIENCE", "stripe-payment-service-api"),
			// Fifteen minutes rather than an hour: the access token is stateless
			// and cannot be revoked, so its lifetime is the containment window
			// for a stolen one. A refresh token makes that affordable.
			AccessTokenTTL:  l.duration("JWT_ACCESS_TOKEN_TTL", 15*time.Minute),
			RefreshTokenTTL: l.duration("REFRESH_TOKEN_TTL", 30*24*time.Hour),
			CookieSecure:    l.boolVal("AUTH_COOKIE_SECURE", false),
			CookieDomain:    l.str("AUTH_COOKIE_DOMAIN", ""),
			BcryptCost:      l.intVal("BCRYPT_COST", 12),
		},
		Sweeper: Sweeper{
			Enabled:         l.boolVal("WEBHOOK_SWEEPER_ENABLED", true),
			Interval:        l.duration("WEBHOOK_SWEEPER_INTERVAL", time.Minute),
			MaxAttempts:     l.intVal("WEBHOOK_SWEEPER_MAX_ATTEMPTS", 6),
			BaseBackoff:     l.duration("WEBHOOK_SWEEPER_BASE_BACKOFF", 30*time.Second),
			MaxBackoff:      l.duration("WEBHOOK_SWEEPER_MAX_BACKOFF", 30*time.Minute),
			BatchSize:       l.intVal("WEBHOOK_SWEEPER_BATCH_SIZE", 100),
			StaleClaimAfter: l.duration("WEBHOOK_STALE_CLAIM_AFTER", 5*time.Minute),
			AlertAfter:      l.duration("WEBHOOK_SWEEPER_ALERT_AFTER", time.Hour),
		},
		Retention: Retention{
			Enabled:               l.boolVal("PAYLOAD_RETENTION_ENABLED", true),
			Interval:              l.duration("PAYLOAD_RETENTION_INTERVAL", 6*time.Hour),
			SettledPayloadAfter:   l.duration("PAYLOAD_RETENTION_SETTLED_AFTER", 30*24*time.Hour),
			UnsettledPayloadAfter: l.duration("PAYLOAD_RETENTION_UNSETTLED_AFTER", 90*24*time.Hour),
			BatchSize:             l.intVal("PAYLOAD_RETENTION_BATCH_SIZE", 500),
			RefreshTokenGrace:     l.duration("REFRESH_TOKEN_GRACE", 7*24*time.Hour),
		},
		Log: Log{
			Level:     strings.ToLower(l.str("LOG_LEVEL", "info")),
			Format:    strings.ToLower(l.str("LOG_FORMAT", "json")),
			AddSource: l.boolVal("LOG_ADD_SOURCE", false),
		},
	}

	// Report parse failures and semantic failures together. A value that failed
	// to parse fell back to its (valid) default, so validating alongside does
	// not manufacture spurious errors - and the operator sees the whole picture
	// in one run instead of one restart per typo.
	if err := errors.Join(l.err(), cfg.Validate()); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate enforces every invariant the process depends on, including the
// production safety rules. All violations are reported together.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if !c.App.Environment.valid() {
		add("APP_ENV must be one of development, staging, production (got %q)", c.App.Environment)
	}
	if c.App.ShutdownTimeout <= 0 {
		add("SHUTDOWN_TIMEOUT must be positive (got %s)", c.App.ShutdownTimeout)
	}

	if c.HTTP.Port < 1 || c.HTTP.Port > 65535 {
		add("HTTP_PORT must be between 1 and 65535 (got %d)", c.HTTP.Port)
	}
	if c.HTTP.ReadHeaderTimeout <= 0 {
		add("HTTP_READ_HEADER_TIMEOUT must be positive - a zero value invites Slowloris")
	}

	if c.HTTP.TrustedProxies < 0 {
		add("TRUSTED_PROXIES must not be negative (got %d)", c.HTTP.TrustedProxies)
	}
	if c.HTTP.AuthRateLimitRPS <= 0 {
		add("AUTH_RATE_LIMIT_RPS must be positive")
	}
	if c.HTTP.AuthRateLimitBurst < 1 {
		add("AUTH_RATE_LIMIT_BURST must be at least 1 (got %d)", c.HTTP.AuthRateLimitBurst)
	}

	// A request deadline that outlives WriteTimeout can never fire: the server
	// tears the connection down first, so the caller sees a dropped connection
	// instead of the 503 the timeout middleware wants to send.
	for name, d := range map[string]time.Duration{
		"HTTP_API_TIMEOUT":     c.HTTP.APITimeout,
		"HTTP_WEBHOOK_TIMEOUT": c.HTTP.WebhookTimeout,
	} {
		switch {
		case d <= 0:
			add("%s must be positive", name)
		case c.HTTP.WriteTimeout > 0 && d >= c.HTTP.WriteTimeout:
			add("%s (%s) must be shorter than HTTP_WRITE_TIMEOUT (%s)", name, d, c.HTTP.WriteTimeout)
		}
	}

	// An empty DSN is already reported as a missing required variable; adding
	// "must be a postgres:// URL" on top of that is noise.
	dsn := c.Database.DSN.Reveal()
	switch {
	case dsn == "":
	case !strings.HasPrefix(dsn, "postgres://") &&
		!strings.HasPrefix(dsn, "postgresql://") &&
		!strings.Contains(dsn, "="):
		add("DATABASE_URL must be a postgres:// URL or a libpq keyword/value string")
	case c.App.Environment.IsProduction() && strings.Contains(dsn, "sslmode=disable"):
		// Billing data crossing an unencrypted link is not acceptable, and this
		// is the kind of default that survives from a dev .env into production.
		add("DATABASE_URL must not use sslmode=disable in production")
	}

	if c.Database.MaxConns < 1 {
		add("DB_MAX_CONNS must be at least 1 (got %d)", c.Database.MaxConns)
	}
	if c.Database.MinConns < 0 {
		add("DB_MIN_CONNS must not be negative (got %d)", c.Database.MinConns)
	}
	if c.Database.MinConns > c.Database.MaxConns {
		add("DB_MIN_CONNS (%d) must not exceed DB_MAX_CONNS (%d)", c.Database.MinConns, c.Database.MaxConns)
	}
	if c.Database.ConnectTimeout <= 0 {
		add("DB_CONNECT_TIMEOUT must be positive (got %s)", c.Database.ConnectTimeout)
	}

	errs = append(errs, c.validateStripe()...)
	errs = append(errs, c.validateAuth()...)
	errs = append(errs, c.validateSweeper()...)
	errs = append(errs, c.validateRetention()...)

	if _, err := parseLevelName(c.Log.Level); err != nil {
		add("LOG_LEVEL must be one of debug, info, warn, error (got %q)", c.Log.Level)
	}
	if c.Log.Format != "json" && c.Log.Format != "text" {
		add("LOG_FORMAT must be json or text (got %q)", c.Log.Format)
	}

	return errors.Join(errs...)
}

// validateStripe checks key shape and, critically, that the key's mode matches
// the deployment tier.
func (c *Config) validateStripe() []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	// Absent keys are already reported by the loader as missing required
	// variables; piling shape and mode errors on top of that is noise.
	if key := c.Stripe.SecretKey.Reveal(); key != "" {
		// rk_ is a restricted key, which is a legitimate and safer choice here.
		if !strings.HasPrefix(key, "sk_") && !strings.HasPrefix(key, "rk_") {
			add("STRIPE_SECRET_KEY must begin with sk_ or rk_")
		}

		// Mode/tier mismatches are the expensive ones. A live key on a
		// developer's laptop moves real money against real customers; a test key
		// in production silently accepts payments that never settle. Refuse to
		// start either way.
		live := strings.Contains(key, "_live_")
		test := strings.Contains(key, "_test_")
		switch {
		case c.App.Environment.IsProduction() && test:
			add("STRIPE_SECRET_KEY is a test key but APP_ENV is production")
		case !c.App.Environment.IsProduction() && live:
			add("STRIPE_SECRET_KEY is a live key but APP_ENV is %s - refusing to start against live Stripe", c.App.Environment)
		}
	}

	if ws := c.Stripe.WebhookSecret.Reveal(); ws != "" && !strings.HasPrefix(ws, "whsec_") {
		add("STRIPE_WEBHOOK_SECRET must begin with whsec_")
	}
	if c.Stripe.PublishableKey != "" && !strings.HasPrefix(c.Stripe.PublishableKey, "pk_") {
		add("STRIPE_PUBLISHABLE_KEY must begin with pk_")
	}
	if c.Stripe.WebhookTolerance <= 0 {
		add("STRIPE_WEBHOOK_TOLERANCE must be positive - it bounds the signature replay window")
	}
	if c.Stripe.MaxNetworkRetries < 0 {
		add("STRIPE_MAX_NETWORK_RETRIES must not be negative (got %d)", c.Stripe.MaxNetworkRetries)
	}
	if c.Stripe.HTTPTimeout <= 0 {
		add("STRIPE_HTTP_TIMEOUT must be positive")
	}

	for name, raw := range map[string]string{
		"STRIPE_CHECKOUT_SUCCESS_URL": c.Stripe.CheckoutSuccessURL,
		"STRIPE_CHECKOUT_CANCEL_URL":  c.Stripe.CheckoutCancelURL,
	} {
		u, err := url.Parse(raw)
		switch {
		case raw == "":
			add("%s is required", name)
		case err != nil || !u.IsAbs():
			add("%s must be an absolute URL (got %q)", name, raw)
		case c.App.Environment.IsProduction() && u.Scheme != "https":
			// The session id is appended to the success URL; over plain HTTP it
			// is readable by anything on the path.
			add("%s must use https in production (got scheme %q)", name, u.Scheme)
		}
	}

	for _, origin := range c.HTTP.CORSAllowedOrigins {
		u, err := url.Parse(origin)
		switch {
		case origin == "*":
			// A wildcard cannot be combined with credentialed requests, and this
			// API is called with them. Reflecting any origin would let any site
			// read a logged-in user's billing data.
			add("CORS_ALLOWED_ORIGINS must not contain '*'; list origins explicitly")
		case err != nil || !u.IsAbs() || u.Host == "":
			add("CORS_ALLOWED_ORIGINS entry %q must be an absolute origin like https://app.example.com", origin)
		case u.Path != "" && u.Path != "/":
			add("CORS_ALLOWED_ORIGINS entry %q must be scheme://host[:port] with no path", origin)
		case c.App.Environment.IsProduction() && u.Scheme != "https":
			add("CORS_ALLOWED_ORIGINS entry %q must use https in production", origin)
		}
	}

	for name, raw := range map[string]string{
		"STRIPE_CHECKOUT_RETURN_URL": c.Stripe.CheckoutReturnURL,
		"STRIPE_PORTAL_RETURN_URL":   c.Stripe.PortalReturnURL,
	} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		switch {
		case err != nil || !u.IsAbs():
			add("%s must be an absolute URL (got %q)", name, raw)
		case c.App.Environment.IsProduction() && u.Scheme != "https":
			add("%s must use https in production", name)
		}
	}

	for _, id := range c.Stripe.AllowedPriceIDs {
		if !strings.HasPrefix(id, "price_") {
			add("STRIPE_ALLOWED_PRICE_IDS entry %q must begin with price_", id)
		}
	}
	// The price id arrives in the request body. Without an allowlist, anyone who
	// can reach /api/v1/checkout can subscribe against any price in the account,
	// including one belonging to a different product or tier.
	if c.App.Environment.IsProduction() && len(c.Stripe.AllowedPriceIDs) == 0 {
		add("STRIPE_ALLOWED_PRICE_IDS must not be empty in production")
	}

	if c.App.Environment.IsProduction() && c.Stripe.IgnoreAPIVersionMismatch {
		add("STRIPE_IGNORE_API_VERSION_MISMATCH must not be enabled in production - " +
			"events would be deserialised against a version they were not rendered for")
	}
	return errs
}

// validateAuth checks the signing key and the token lifetime.
func (c *Config) validateAuth() []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	// An absent secret is already reported as a missing required variable.
	if secret := c.Auth.JWTSecret.Reveal(); secret != "" {
		if len(secret) < 32 {
			add("JWT_SECRET must be at least 32 bytes (got %d) - a shorter HMAC key "+
				"adds nothing over a 256-bit one and is usually a guessable passphrase", len(secret))
		}
		// The value shipped in .env.example, so it is public. Anyone holding it
		// can mint a token for any user id.
		if strings.Contains(secret, "change_me") || strings.Contains(secret, "example") {
			add("JWT_SECRET still holds a placeholder value - generate one with `openssl rand -base64 48`")
		}
	}

	if c.Auth.JWTIssuer == "" {
		add("JWT_ISSUER must not be empty")
	}
	if c.Auth.JWTAudience == "" {
		add("JWT_AUDIENCE must not be empty")
	}

	switch ttl := c.Auth.AccessTokenTTL; {
	case ttl <= 0:
		add("JWT_ACCESS_TOKEN_TTL must be positive")
	case c.App.Environment.IsProduction() && ttl > 24*time.Hour:
		// Tokens are stateless: there is no way to revoke one early, so the TTL
		// is the entire containment window for a stolen credential.
		add("JWT_ACCESS_TOKEN_TTL is %s; tokens cannot be revoked before expiry, "+
			"so production must not exceed 24h", ttl)
	}

	switch ttl := c.Auth.RefreshTokenTTL; {
	case ttl <= 0:
		add("REFRESH_TOKEN_TTL must be positive")
	case ttl <= c.Auth.AccessTokenTTL:
		add("REFRESH_TOKEN_TTL (%s) must exceed JWT_ACCESS_TOKEN_TTL (%s); a refresh "+
			"token that expires first cannot renew anything",
			ttl, c.Auth.AccessTokenTTL)
	}

	// A refresh token sent over plain HTTP is readable by anything on the path,
	// and it is the credential that outlives every access token.
	if c.App.Environment.IsProduction() && !c.Auth.CookieSecure {
		add("AUTH_COOKIE_SECURE must be true in production; the refresh cookie would " +
			"otherwise be sent over plain HTTP")
	}

	if cost := c.Auth.BcryptCost; cost < 10 || cost > 31 {
		add("BCRYPT_COST must be between 10 and 31 (got %d)", cost)
	}
	return errs
}

func (c *Config) validateSweeper() []error {
	if !c.Sweeper.Enabled {
		return nil
	}
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Sweeper.Interval <= 0 {
		add("WEBHOOK_SWEEPER_INTERVAL must be positive")
	}
	if c.Sweeper.MaxAttempts < 1 {
		add("WEBHOOK_SWEEPER_MAX_ATTEMPTS must be at least 1 (got %d)", c.Sweeper.MaxAttempts)
	}
	if c.Sweeper.BatchSize < 1 {
		add("WEBHOOK_SWEEPER_BATCH_SIZE must be at least 1 (got %d)", c.Sweeper.BatchSize)
	}
	if c.Sweeper.BaseBackoff <= 0 {
		add("WEBHOOK_SWEEPER_BASE_BACKOFF must be positive")
	}
	if c.Sweeper.MaxBackoff < c.Sweeper.BaseBackoff {
		add("WEBHOOK_SWEEPER_MAX_BACKOFF (%s) must not be shorter than "+
			"WEBHOOK_SWEEPER_BASE_BACKOFF (%s)", c.Sweeper.MaxBackoff, c.Sweeper.BaseBackoff)
	}
	// A stale window shorter than the request deadline lets the sweeper steal
	// work from a handler that is still running it, and then two workers
	// process the same event at once.
	if c.Sweeper.StaleClaimAfter <= c.HTTP.WebhookTimeout {
		add("WEBHOOK_STALE_CLAIM_AFTER (%s) must exceed HTTP_WEBHOOK_TIMEOUT (%s), "+
			"or the sweeper reclaims claims that are still being worked on",
			c.Sweeper.StaleClaimAfter, c.HTTP.WebhookTimeout)
	}
	return errs
}

func (c *Config) validateRetention() []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if !c.Retention.Enabled {
		// Not an error - a deployment may minimise through some other pipeline -
		// but it is worth refusing to let it pass unnoticed in production,
		// where the payload column accumulates personal data indefinitely.
		if c.App.Environment.IsProduction() {
			add("PAYLOAD_RETENTION_ENABLED is false in production; webhook payloads " +
				"hold customer email and billing address and would be kept indefinitely")
		}
		return errs
	}

	if c.Retention.Interval <= 0 {
		add("PAYLOAD_RETENTION_INTERVAL must be positive")
	}
	if c.Retention.BatchSize < 1 {
		add("PAYLOAD_RETENTION_BATCH_SIZE must be at least 1 (got %d)", c.Retention.BatchSize)
	}
	if c.Retention.SettledPayloadAfter <= 0 {
		add("PAYLOAD_RETENTION_SETTLED_AFTER must be positive")
	}
	if c.Retention.UnsettledPayloadAfter < c.Retention.SettledPayloadAfter {
		add("PAYLOAD_RETENTION_UNSETTLED_AFTER (%s) must not be shorter than "+
			"PAYLOAD_RETENTION_SETTLED_AFTER (%s): an unresolved event needs its payload "+
			"for longer than a settled one, not less",
			c.Retention.UnsettledPayloadAfter, c.Retention.SettledPayloadAfter)
	}

	// The sweeper replays failed events from the payload. Purging one before
	// the sweeper has finished with it turns a recoverable event into a
	// permanently lost one, so the outer bound has to clear the retry budget by
	// a wide margin.
	if c.Sweeper.Enabled {
		exhaust := time.Duration(c.Sweeper.MaxAttempts) * c.Sweeper.MaxBackoff
		if c.Retention.UnsettledPayloadAfter < 10*exhaust {
			add("PAYLOAD_RETENTION_UNSETTLED_AFTER (%s) is too close to the sweeper's retry "+
				"budget (%d attempts x %s = %s); failed events would lose their payload "+
				"before the sweeper is done with them",
				c.Retention.UnsettledPayloadAfter, c.Sweeper.MaxAttempts,
				c.Sweeper.MaxBackoff, exhaust)
		}
	}
	return errs
}

// parseLevelName is duplicated in the logger package deliberately: config must
// not import logger, and logger must not import config. Both layers need to
// agree on the same four names, and the list is stable.
func parseLevelName(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q", name)
	}
}

// loader reads environment variables and accumulates parse failures so that
// Load can report them all at once.
type loader struct{ errs []error }

func (l *loader) err() error { return errors.Join(l.errs...) }

func (l *loader) fail(format string, a ...any) { l.errs = append(l.errs, fmt.Errorf(format, a...)) }

func (l *loader) lookup(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

func (l *loader) str(key, def string) string {
	if v, ok := l.lookup(key); ok {
		return v
	}
	return def
}

func (l *loader) required(key string) string {
	v, ok := l.lookup(key)
	if !ok {
		l.fail("required environment variable %s is not set", key)
	}
	return v
}

func (l *loader) intVal(key string, def int) int {
	v, ok := l.lookup(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.fail("%s must be an integer (got %q)", key, v)
		return def
	}
	return n
}

func (l *loader) duration(key string, def time.Duration) time.Duration {
	v, ok := l.lookup(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.fail("%s must be a duration such as 30s or 5m (got %q)", key, v)
		return def
	}
	return d
}

func (l *loader) boolVal(key string, def bool) bool {
	v, ok := l.lookup(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.fail("%s must be a boolean (got %q)", key, v)
		return def
	}
	return b
}

func (l *loader) float(key string, def float64) float64 {
	v, ok := l.lookup(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		l.fail("%s must be a number (got %q)", key, v)
		return def
	}
	return f
}

// csv reads a comma-separated list, discarding blank entries so a trailing
// comma or a stray space does not become an empty allowlist entry.
func (l *loader) csv(key string) []string {
	raw, ok := l.lookup(key)
	if !ok {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
