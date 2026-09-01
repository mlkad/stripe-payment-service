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
	App      App
	HTTP     HTTP
	Database Database
	Stripe   Stripe
	Log      Log
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
			APIVersion:        l.str("STRIPE_API_VERSION", "2024-06-20"),
			WebhookTolerance:  l.duration("STRIPE_WEBHOOK_TOLERANCE", 5*time.Minute),
			MaxNetworkRetries: l.intVal("STRIPE_MAX_NETWORK_RETRIES", 3),
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
