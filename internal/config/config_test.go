package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// validEnv is a configuration that loads cleanly. Tests mutate one key at a
// time from here, so a failure names exactly one cause.
func validEnv() map[string]string {
	return map[string]string{
		"APP_ENV":                           "development",
		"DATABASE_URL":                      "postgres://payments:pw@localhost:5432/payments?sslmode=disable",
		"STRIPE_SECRET_KEY":                 "sk_test_abc123",
		"STRIPE_WEBHOOK_SECRET":             "whsec_abc123",
		"JWT_SECRET":                        "a-secret-that-is-at-least-32-bytes-long",
		"STRIPE_CHECKOUT_SUCCESS_URL":       "http://localhost:3000/ok",
		"STRIPE_CHECKOUT_CANCEL_URL":        "http://localhost:3000/cancel",
		"STRIPE_ALLOWED_PRICE_IDS":          "price_abc",
		"PAYLOAD_RETENTION_SETTLED_AFTER":   "720h",
		"PAYLOAD_RETENTION_UNSETTLED_AFTER": "2160h",
	}
}

func loadWith(t *testing.T, env map[string]string) (*Config, error) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	return Load()
}

// loadExpectingError returns the aggregated error text, failing if the load
// unexpectedly succeeded.
func loadExpectingError(t *testing.T, overrides map[string]string) string {
	t.Helper()
	env := validEnv()
	for k, v := range overrides {
		env[k] = v
	}
	cfg, err := loadWith(t, env)
	if err == nil {
		t.Fatalf("configuration loaded without error; expected a rejection (env: %v)", overrides)
	}
	if cfg != nil {
		t.Error("a rejected configuration was still returned")
	}
	return err.Error()
}

func TestLoad_ValidConfiguration(t *testing.T) {
	cfg, err := loadWith(t, validEnv())
	if err != nil {
		t.Fatalf("valid configuration was rejected: %v", err)
	}
	if cfg.App.Environment != EnvDevelopment {
		t.Errorf("environment = %q, want development", cfg.App.Environment)
	}
	if cfg.HTTP.Port != 8080 {
		t.Errorf("port = %d, want the 8080 default", cfg.HTTP.Port)
	}
	if cfg.Auth.AccessTokenTTL != time.Hour {
		t.Errorf("jwt ttl = %s, want the 1h default", cfg.Auth.AccessTokenTTL)
	}
}

// Load reports every problem at once, so a misconfigured deployment is fixed in
// one pass rather than one restart per typo.
func TestLoad_ReportsEveryProblemTogether(t *testing.T) {
	msg := loadExpectingError(t, map[string]string{
		"APP_ENV":               "bananas",
		"HTTP_PORT":             "70000",
		"JWT_SECRET":            "too-short",
		"STRIPE_SECRET_KEY":     "pk_test_wrong_prefix",
		"STRIPE_WEBHOOK_SECRET": "not_a_whsec",
	})

	for _, want := range []string{"APP_ENV", "HTTP_PORT", "JWT_SECRET", "STRIPE_SECRET_KEY", "STRIPE_WEBHOOK_SECRET"} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error is missing %s:\n%s", want, msg)
		}
	}
}

// A value that fails to parse must not suppress the semantic checks: the
// operator should see the whole picture, not one error per restart.
func TestLoad_ParseFailuresDoNotHideSemanticFailures(t *testing.T) {
	msg := loadExpectingError(t, map[string]string{
		"HTTP_PORT":  "not-a-number", // parse failure
		"APP_ENV":    "bananas",      // semantic failure
		"JWT_SECRET": "short",        // semantic failure
	})
	for _, want := range []string{"HTTP_PORT", "APP_ENV", "JWT_SECRET"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %s in:\n%s", want, msg)
		}
	}
}

func TestLoad_RequiredValuesAreReported(t *testing.T) {
	for _, key := range []string{"DATABASE_URL", "STRIPE_SECRET_KEY", "STRIPE_WEBHOOK_SECRET", "JWT_SECRET"} {
		t.Run(key, func(t *testing.T) {
			if msg := loadExpectingError(t, map[string]string{key: ""}); !strings.Contains(msg, key) {
				t.Errorf("missing %s not reported:\n%s", key, msg)
			}
		})
	}
}

/* --- production safety ---------------------------------------------------- */

// A live key on a laptop moves real money; a test key in production silently
// accepts payments that never settle. Both refuse to start.
func TestValidate_StripeKeyModeMustMatchTheTier(t *testing.T) {
	tests := []struct {
		name, env, key string
		wantErr        bool
	}{
		{"test key in development", "development", "sk_test_abc", false},
		{"live key in production", "production", "sk_live_abc", false},
		{"restricted live key in production", "production", "rk_live_abc", false},
		{"live key in development", "development", "sk_live_abc", true},
		{"live key in staging", "staging", "sk_live_abc", true},
		{"test key in production", "production", "sk_test_abc", true},
		{"wrong prefix", "development", "pk_test_abc", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validEnv()
			env["APP_ENV"] = tt.env
			env["STRIPE_SECRET_KEY"] = tt.key
			if tt.env == "production" {
				env["DATABASE_URL"] = "postgres://u:p@db/payments?sslmode=verify-full"
				env["STRIPE_CHECKOUT_SUCCESS_URL"] = "https://app.example.com/ok"
				env["STRIPE_CHECKOUT_CANCEL_URL"] = "https://app.example.com/cancel"
				env["STRIPE_CHECKOUT_RETURN_URL"] = "https://app.example.com/return"
				env["STRIPE_PORTAL_RETURN_URL"] = "https://app.example.com/"
			}

			_, err := loadWith(t, env)
			if tt.wantErr && err == nil {
				t.Errorf("key %q in %s was accepted", tt.key, tt.env)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("key %q in %s was rejected: %v", tt.key, tt.env, err)
			}
		})
	}
}

func TestValidate_ProductionRefusesUnsafeSettings(t *testing.T) {
	prod := func(overrides map[string]string) map[string]string {
		env := validEnv()
		env["APP_ENV"] = "production"
		env["STRIPE_SECRET_KEY"] = "sk_live_abc"
		env["DATABASE_URL"] = "postgres://u:p@db/payments?sslmode=verify-full"
		env["STRIPE_CHECKOUT_SUCCESS_URL"] = "https://app.example.com/ok"
		env["STRIPE_CHECKOUT_CANCEL_URL"] = "https://app.example.com/cancel"
		env["STRIPE_CHECKOUT_RETURN_URL"] = "https://app.example.com/return"
		env["STRIPE_PORTAL_RETURN_URL"] = "https://app.example.com/"
		for k, v := range overrides {
			env[k] = v
		}
		return env
	}

	tests := []struct {
		name      string
		overrides map[string]string
		wantIn    string
	}{
		{"sslmode=disable", map[string]string{
			"DATABASE_URL": "postgres://u:p@db/payments?sslmode=disable"}, "sslmode"},
		{"no price allowlist", map[string]string{
			"STRIPE_ALLOWED_PRICE_IDS": ""}, "STRIPE_ALLOWED_PRICE_IDS"},
		{"http checkout url", map[string]string{
			"STRIPE_CHECKOUT_SUCCESS_URL": "http://app.example.com/ok"}, "https"},
		{"api version mismatch ignored", map[string]string{
			"STRIPE_IGNORE_API_VERSION_MISMATCH": "true"}, "STRIPE_IGNORE_API_VERSION_MISMATCH"},
		{"wildcard cors origin", map[string]string{
			"CORS_ALLOWED_ORIGINS": "*"}, "CORS_ALLOWED_ORIGINS"},
		{"http cors origin", map[string]string{
			"CORS_ALLOWED_ORIGINS": "http://app.example.com"}, "https"},
		{"retention disabled", map[string]string{
			"PAYLOAD_RETENTION_ENABLED": "false"}, "PAYLOAD_RETENTION_ENABLED"},
		{"token ttl beyond a day", map[string]string{
			"JWT_ACCESS_TOKEN_TTL": "48h"}, "JWT_ACCESS_TOKEN_TTL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadWith(t, prod(tt.overrides))
			if err == nil {
				t.Fatalf("production accepted %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("error does not mention %q:\n%s", tt.wantIn, err)
			}
		})
	}
}

/* --- cross-component constraints ------------------------------------------ */

// A request deadline that outlives WriteTimeout can never fire: the server
// closes the connection first and the caller sees a drop, not a 503.
func TestValidate_RequestDeadlinesMustFitInsideWriteTimeout(t *testing.T) {
	for _, key := range []string{"HTTP_API_TIMEOUT", "HTTP_WEBHOOK_TIMEOUT"} {
		t.Run(key, func(t *testing.T) {
			msg := loadExpectingError(t, map[string]string{
				"HTTP_WRITE_TIMEOUT": "10s",
				key:                  "30s",
			})
			if !strings.Contains(msg, key) || !strings.Contains(msg, "HTTP_WRITE_TIMEOUT") {
				t.Errorf("error does not explain the relationship:\n%s", msg)
			}
		})
	}
}

// A stale window shorter than the webhook deadline lets the sweeper reclaim a
// claim a handler is still working on, and then two workers process one event.
func TestValidate_StaleClaimWindowMustExceedTheWebhookDeadline(t *testing.T) {
	msg := loadExpectingError(t, map[string]string{
		"HTTP_WEBHOOK_TIMEOUT":      "25s",
		"WEBHOOK_STALE_CLAIM_AFTER": "10s",
	})
	if !strings.Contains(msg, "WEBHOOK_STALE_CLAIM_AFTER") {
		t.Errorf("error does not name the setting:\n%s", msg)
	}
}

// Retention must not destroy payloads the sweeper still needs to replay.
func TestValidate_RetentionMustOutlastTheSweeperRetryBudget(t *testing.T) {
	msg := loadExpectingError(t, map[string]string{
		"WEBHOOK_SWEEPER_MAX_ATTEMPTS":      "6",
		"WEBHOOK_SWEEPER_MAX_BACKOFF":       "30m",
		"PAYLOAD_RETENTION_UNSETTLED_AFTER": "1h", // budget is 3h; needs 10x
		"PAYLOAD_RETENTION_SETTLED_AFTER":   "30m",
	})
	if !strings.Contains(msg, "PAYLOAD_RETENTION_UNSETTLED_AFTER") {
		t.Errorf("error does not name the setting:\n%s", msg)
	}
}

// An unresolved event needs its payload for longer than a settled one, not less.
func TestValidate_UnsettledWindowMustNotBeShorterThanSettled(t *testing.T) {
	msg := loadExpectingError(t, map[string]string{
		"PAYLOAD_RETENTION_SETTLED_AFTER":   "2160h",
		"PAYLOAD_RETENTION_UNSETTLED_AFTER": "720h",
	})
	if !strings.Contains(msg, "PAYLOAD_RETENTION_UNSETTLED_AFTER") {
		t.Errorf("error does not name the setting:\n%s", msg)
	}
}

func TestValidate_JWTSecretRejectsPlaceholders(t *testing.T) {
	for _, secret := range []string{
		"change_me_generate_with_openssl_rand_base64_48",
		"this-is-an-example-secret-value-32-bytes",
	} {
		t.Run(secret[:12], func(t *testing.T) {
			if msg := loadExpectingError(t, map[string]string{"JWT_SECRET": secret}); !strings.Contains(msg, "JWT_SECRET") {
				t.Errorf("placeholder secret accepted:\n%s", msg)
			}
		})
	}
}

/* --- secret redaction ----------------------------------------------------- */

// A leaked Stripe key is an incident, not a rotation. The type system prevents
// it rather than a code review.
func TestSecretRedactsThroughEveryPath(t *testing.T) {
	const real = "sk_live_super_secret_value"
	secret := Secret(real)

	renderings := map[string]string{
		"String":     secret.String(),
		"fmt %v":     fmt.Sprintf("%v", secret),
		"fmt %s":     fmt.Sprintf("%s", secret),
		"fmt %+v":    fmt.Sprintf("%+v", secret),
		"fmt %#v":    fmt.Sprintf("%#v", secret),
		"fmt %q":     fmt.Sprintf("%q", secret),
		"error text": fmt.Errorf("failed with %v", secret).Error(),
	}
	for name, got := range renderings {
		if strings.Contains(got, real) {
			t.Errorf("%s leaked the secret: %s", name, got)
		}
		if !strings.Contains(got, redacted) {
			t.Errorf("%s = %q, want the redaction placeholder", name, got)
		}
	}

	encoded, err := json.Marshal(map[string]Secret{"key": secret})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), real) {
		t.Errorf("json leaked the secret: %s", encoded)
	}

	if v := secret.LogValue(); strings.Contains(v.String(), real) {
		t.Errorf("slog leaked the secret: %s", v)
	}
	// Reveal is the one deliberate way out.
	if secret.Reveal() != real {
		t.Error("Reveal did not return the underlying value")
	}
	// An empty secret must stay empty rather than rendering as [REDACTED],
	// which would make a missing value look like a present one.
	if Secret("").String() != "" {
		t.Errorf("empty secret rendered as %q", Secret("").String())
	}
}

// The boot log is the one place the whole configuration is printed.
func TestConfigLogValueRedactsEverySecret(t *testing.T) {
	cfg, err := loadWith(t, validEnv())
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	rendered := fmt.Sprintf("%v", cfg.LogValue())
	for _, secret := range []string{
		cfg.Stripe.SecretKey.Reveal(),
		cfg.Stripe.WebhookSecret.Reveal(),
		cfg.Auth.JWTSecret.Reveal(),
		cfg.Database.DSN.Reveal(),
	} {
		if secret != "" && strings.Contains(rendered, secret) {
			t.Errorf("boot log leaked a secret:\n%s", rendered)
		}
	}
	// It still has to be useful.
	if !strings.Contains(rendered, "development") {
		t.Errorf("boot log lost the environment:\n%s", rendered)
	}
}

func TestStripeKeyMode(t *testing.T) {
	tests := []struct{ key, want string }{
		{"sk_live_abc", "live"},
		{"rk_live_abc", "live"},
		{"sk_test_abc", "test"},
		{"", "unknown"},
		{"garbage", "unknown"},
	}
	for _, tt := range tests {
		if got := stripeKeyMode(Secret(tt.key)); got != tt.want {
			t.Errorf("stripeKeyMode(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

/* --- loader --------------------------------------------------------------- */

func TestLoaderCSV(t *testing.T) {
	tests := []struct {
		name, raw string
		want      []string
	}{
		{"simple", "a,b,c", []string{"a", "b", "c"}},
		{"spaces trimmed", " a , b ", []string{"a", "b"}},
		{"trailing comma dropped", "a,b,", []string{"a", "b"}},
		{"blank entries dropped", "a,,b", []string{"a", "b"}},
		{"only separators", ",,,", nil},
		{"empty", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CSV_TEST_KEY", tt.raw)
			got := (&loader{}).csv("CSV_TEST_KEY")
			if len(got) != len(tt.want) {
				t.Fatalf("csv(%q) = %v, want %v", tt.raw, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("csv(%q)[%d] = %q, want %q", tt.raw, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestLoaderTypedAccessorsReportBadInput(t *testing.T) {
	tests := []struct {
		name string
		read func(*loader)
	}{
		{"int", func(l *loader) { l.intVal("TYPED_TEST_KEY", 1) }},
		{"duration", func(l *loader) { l.duration("TYPED_TEST_KEY", time.Second) }},
		{"bool", func(l *loader) { l.boolVal("TYPED_TEST_KEY", false) }},
		{"float", func(l *loader) { l.float("TYPED_TEST_KEY", 1.0) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TYPED_TEST_KEY", "definitely-not-valid")
			l := &loader{}
			tt.read(l)
			if l.err() == nil {
				t.Errorf("%s accessor silently swallowed unparseable input", tt.name)
			}
		})
	}
}

func TestLoaderDefaultsApplyWhenUnset(t *testing.T) {
	l := &loader{}
	if got := l.str("UNSET_TEST_KEY", "fallback"); got != "fallback" {
		t.Errorf("str = %q, want the default", got)
	}
	if got := l.intVal("UNSET_TEST_KEY", 42); got != 42 {
		t.Errorf("intVal = %d, want the default", got)
	}
	if got := l.duration("UNSET_TEST_KEY", time.Minute); got != time.Minute {
		t.Errorf("duration = %s, want the default", got)
	}
	if l.err() != nil {
		t.Errorf("unset values produced errors: %v", l.err())
	}
}

func TestHTTPAddr(t *testing.T) {
	if got := (HTTP{Port: 9000}).Addr(); got != ":9000" {
		t.Errorf("Addr = %q, want :9000", got)
	}
}

func TestEnvironmentHelpers(t *testing.T) {
	if !EnvProduction.IsProduction() {
		t.Error("production is not reported as production")
	}
	if EnvStaging.IsProduction() {
		t.Error("staging is reported as production")
	}
	for _, e := range []Environment{EnvDevelopment, EnvStaging, EnvProduction} {
		if !e.valid() {
			t.Errorf("%q is not valid", e)
		}
	}
	if Environment("bananas").valid() {
		t.Error("an unknown environment is valid")
	}
}

func TestParseLevelNameMatchesTheLoggerContract(t *testing.T) {
	for name, want := range map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	} {
		got, err := parseLevelName(name)
		if err != nil || got != want {
			t.Errorf("parseLevelName(%q) = %v, %v; want %v", name, got, err, want)
		}
	}
	if _, err := parseLevelName("chatty"); err == nil {
		t.Error("an unknown level was accepted")
	}
}

// Every remaining bounds check, in one table. These are individually dull and
// collectively the difference between a typo caught at boot and a service that
// starts with a sweeper that never sweeps.
func TestValidate_BoundsChecks(t *testing.T) {
	tests := []struct {
		key, value, wantIn string
	}{
		{"HTTP_PORT", "0", "HTTP_PORT"},
		{"HTTP_PORT", "70000", "HTTP_PORT"},
		{"HTTP_READ_HEADER_TIMEOUT", "0s", "Slowloris"},
		{"HTTP_API_TIMEOUT", "0s", "HTTP_API_TIMEOUT"},
		{"TRUSTED_PROXIES", "-1", "TRUSTED_PROXIES"},
		{"AUTH_RATE_LIMIT_RPS", "0", "AUTH_RATE_LIMIT_RPS"},
		{"AUTH_RATE_LIMIT_BURST", "0", "AUTH_RATE_LIMIT_BURST"},

		{"STRIPE_PUBLISHABLE_KEY", "sk_test_wrong", "STRIPE_PUBLISHABLE_KEY"},
		{"STRIPE_WEBHOOK_TOLERANCE", "0s", "STRIPE_WEBHOOK_TOLERANCE"},
		{"STRIPE_MAX_NETWORK_RETRIES", "-1", "STRIPE_MAX_NETWORK_RETRIES"},
		{"STRIPE_HTTP_TIMEOUT", "0s", "STRIPE_HTTP_TIMEOUT"},
		{"STRIPE_CHECKOUT_SUCCESS_URL", "/relative/path", "absolute"},
		{"STRIPE_CHECKOUT_RETURN_URL", "not-a-url", "absolute"},
		{"STRIPE_PORTAL_RETURN_URL", "also-not-a-url", "absolute"},
		{"STRIPE_ALLOWED_PRICE_IDS", "prod_wrong_prefix", "price_"},
		{"CORS_ALLOWED_ORIGINS", "http://app.example.com/with/path", "no path"},
		{"CORS_ALLOWED_ORIGINS", "not-an-origin", "absolute origin"},

		{"JWT_ACCESS_TOKEN_TTL", "0s", "JWT_ACCESS_TOKEN_TTL"},
		{"BCRYPT_COST", "4", "BCRYPT_COST"},
		{"BCRYPT_COST", "40", "BCRYPT_COST"},

		{"WEBHOOK_SWEEPER_INTERVAL", "0s", "WEBHOOK_SWEEPER_INTERVAL"},
		{"WEBHOOK_SWEEPER_MAX_ATTEMPTS", "0", "WEBHOOK_SWEEPER_MAX_ATTEMPTS"},
		{"WEBHOOK_SWEEPER_BATCH_SIZE", "0", "WEBHOOK_SWEEPER_BATCH_SIZE"},
		{"WEBHOOK_SWEEPER_BASE_BACKOFF", "0s", "WEBHOOK_SWEEPER_BASE_BACKOFF"},

		{"PAYLOAD_RETENTION_INTERVAL", "0s", "PAYLOAD_RETENTION_INTERVAL"},
		{"PAYLOAD_RETENTION_BATCH_SIZE", "0", "PAYLOAD_RETENTION_BATCH_SIZE"},
		{"PAYLOAD_RETENTION_SETTLED_AFTER", "0s", "PAYLOAD_RETENTION_SETTLED_AFTER"},
	}
	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			msg := loadExpectingError(t, map[string]string{tt.key: tt.value})
			if !strings.Contains(msg, tt.wantIn) {
				t.Errorf("error does not mention %q:\n%s", tt.wantIn, msg)
			}
		})
	}
}

// The issuer and audience checks cannot be reached through the environment:
// loader.lookup treats an empty variable as unset, so both fall back to their
// defaults. They still guard a Config built directly in code, which is how a
// test or a future embedding would construct one.
func TestValidateAuth_RejectsEmptyIssuerAndAudience(t *testing.T) {
	base := func() *Config {
		return &Config{
			App: App{Environment: EnvDevelopment},
			Auth: Auth{
				JWTSecret:      Secret("a-secret-that-is-at-least-32-bytes-long"),
				JWTIssuer:      "iss",
				JWTAudience:    "aud",
				AccessTokenTTL: time.Hour,
				BcryptCost:     12,
			},
		}
	}

	if errs := base().validateAuth(); len(errs) != 0 {
		t.Fatalf("a valid auth config was rejected: %v", errs)
	}

	noIssuer := base()
	noIssuer.Auth.JWTIssuer = ""
	if errs := noIssuer.validateAuth(); len(errs) == 0 {
		t.Error("an empty issuer was accepted")
	}

	noAudience := base()
	noAudience.Auth.JWTAudience = ""
	if errs := noAudience.validateAuth(); len(errs) == 0 {
		t.Error("an empty audience was accepted")
	}
}

// An empty environment variable means "unset", so a defaulted setting falls
// back rather than becoming empty. Worth pinning: the opposite behaviour would
// let a blank line in a .env file silently blank a required default.
func TestLoaderTreatsEmptyEnvironmentVariablesAsUnset(t *testing.T) {
	t.Setenv("EMPTY_TEST_KEY", "")
	if got := (&loader{}).str("EMPTY_TEST_KEY", "fallback"); got != "fallback" {
		t.Errorf("str = %q, want the default", got)
	}

	t.Setenv("WHITESPACE_TEST_KEY", "   ")
	if got := (&loader{}).str("WHITESPACE_TEST_KEY", "fallback"); got != "fallback" {
		t.Errorf("whitespace-only value = %q, want the default", got)
	}
}

// MaxBackoff shorter than BaseBackoff means the cap is below the floor, so
// backoff never grows the way the operator asked for.
func TestValidate_SweeperBackoffCapMustNotBeBelowTheFloor(t *testing.T) {
	msg := loadExpectingError(t, map[string]string{
		"WEBHOOK_SWEEPER_BASE_BACKOFF": "10m",
		"WEBHOOK_SWEEPER_MAX_BACKOFF":  "1m",
	})
	if !strings.Contains(msg, "WEBHOOK_SWEEPER_MAX_BACKOFF") {
		t.Errorf("error does not name the setting:\n%s", msg)
	}
}

// Disabling the sweeper skips its bounds checks entirely; a deployment that
// does not run it should not be blocked by its tuning.
func TestValidate_DisabledSubsystemsSkipTheirChecks(t *testing.T) {
	env := validEnv()
	env["WEBHOOK_SWEEPER_ENABLED"] = "false"
	env["WEBHOOK_SWEEPER_MAX_ATTEMPTS"] = "0" // would fail if checked
	env["WEBHOOK_SWEEPER_INTERVAL"] = "0s"

	if _, err := loadWith(t, env); err != nil {
		t.Errorf("a disabled sweeper's tuning was still validated: %v", err)
	}
}

// A DSN that is neither a URL nor a keyword string is a typo worth catching.
func TestValidate_DatabaseURLShape(t *testing.T) {
	if msg := loadExpectingError(t, map[string]string{
		"DATABASE_URL": "just-some-text"}); !strings.Contains(msg, "DATABASE_URL") {
		t.Errorf("malformed DSN accepted:\n%s", msg)
	}

	// libpq keyword/value form is legitimate and must not be rejected.
	env := validEnv()
	env["DATABASE_URL"] = "host=localhost port=5432 user=payments dbname=payments"
	if _, err := loadWith(t, env); err != nil {
		t.Errorf("a libpq keyword/value DSN was rejected: %v", err)
	}
}
