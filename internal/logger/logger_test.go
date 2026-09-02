package logger

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func capture(t *testing.T, opts Options) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	opts.Output = buf
	if opts.Level == "" {
		opts.Level = "debug"
	}
	if opts.Format == "" {
		opts.Format = "json"
	}
	log, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return log, buf
}

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("nothing was logged")
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.Split(line, "\n")[0]), &rec); err != nil {
		t.Fatalf("log line is not JSON: %q", line)
	}
	return rec
}

// The last line of defence. Secrets are supposed to be wrapped in config.Secret
// long before they reach here, but a raw string attached to a
// sensitively-named key must not survive either.
func TestRedactsSensitiveKeys(t *testing.T) {
	log, buf := capture(t, Options{})

	log.Info("boot",
		slog.String("password", "hunter2"),
		slog.String("api_key", "sk_live_abc"),
		slog.String("Authorization", "Bearer abc.def.ghi"),
		slog.String("database_url", "postgres://u:pw@host/db"),
		slog.String("stripe_signature", "t=1,v1=abc"),
		slog.String("private_key", "-----BEGIN"),
		slog.String("email", "ada@example.com"),
	)

	rendered := buf.String()
	for _, secret := range []string{
		"hunter2", "sk_live_abc", "Bearer abc.def.ghi",
		"postgres://u:pw@host/db", "t=1,v1=abc", "-----BEGIN",
	} {
		if strings.Contains(rendered, secret) {
			t.Errorf("a sensitive value survived redaction: %q in\n%s", secret, rendered)
		}
	}
	// Non-sensitive fields must still come through, or the log is useless.
	if !strings.Contains(rendered, "ada@example.com") {
		t.Errorf("an ordinary field was redacted:\n%s", rendered)
	}
}

func TestIsSensitiveKey(t *testing.T) {
	sensitive := []string{
		"password", "PASSWORD", "user_password", "passwd",
		"secret", "client_secret", "token", "access_token",
		"api_key", "apikey", "Authorization", "cookie",
		"credential", "private_key", "dsn", "DATABASE_URL", "signature",
	}
	for _, k := range sensitive {
		if !isSensitiveKey(k) {
			t.Errorf("%q is not treated as sensitive", k)
		}
	}
	for _, k := range []string{"email", "user_id", "status", "duration", "path"} {
		if isSensitiveKey(k) {
			t.Errorf("%q was redacted unnecessarily", k)
		}
	}
}

// Every record for a request carries its correlation id, without each call
// site having to remember to attach it.
func TestRequestIDFlowsFromContext(t *testing.T) {
	log, buf := capture(t, Options{})
	ctx := WithRequestID(t.Context(), "trace-abc-123")

	log.InfoContext(ctx, "handling")

	if got := decode(t, buf)["request_id"]; got != "trace-abc-123" {
		t.Errorf("request_id = %v, want trace-abc-123", got)
	}
}

// Enrichment must survive With and WithGroup, or a logger derived inside a
// handler silently loses its correlation.
func TestRequestIDSurvivesWithAndWithGroup(t *testing.T) {
	ctx := WithRequestID(t.Context(), "trace-xyz")

	t.Run("With", func(t *testing.T) {
		log, buf := capture(t, Options{})
		log.With(slog.String("component", "billing")).InfoContext(ctx, "msg")

		rec := decode(t, buf)
		if rec["request_id"] != "trace-xyz" {
			t.Errorf("request_id lost through With: %v", rec)
		}
		if rec["component"] != "billing" {
			t.Errorf("attribute lost through With: %v", rec)
		}
	})

	// Under WithGroup the id lands inside the group rather than at the top
	// level. Handle can only add attributes through the handler it holds, which
	// is already grouped by then; escaping would mean emitting a second record.
	// Pinned here so the behaviour is a documented choice, not a surprise.
	t.Run("WithGroup nests the id but does not lose it", func(t *testing.T) {
		log, buf := capture(t, Options{})
		log.WithGroup("stripe").InfoContext(ctx, "msg", slog.String("event", "evt_1"))

		group, ok := decode(t, buf)["stripe"].(map[string]any)
		if !ok {
			t.Fatalf("no stripe group in the record: %s", buf)
		}
		if group["request_id"] != "trace-xyz" {
			t.Errorf("request_id lost through WithGroup: %v", group)
		}
	})
}

func TestRequestIDFromContext(t *testing.T) {
	if got := RequestIDFromContext(t.Context()); got != "" {
		t.Errorf("a bare context yielded %q", got)
	}
	ctx := WithRequestID(t.Context(), "abc")
	if got := RequestIDFromContext(ctx); got != "abc" {
		t.Errorf("RequestIDFromContext = %q, want abc", got)
	}
}

// Timestamps are normalised to UTC so records from instances in different
// zones sort against each other.
func TestTimeIsNormalisedToUTC(t *testing.T) {
	log, buf := capture(t, Options{})
	log.Info("msg")

	raw, ok := decode(t, buf)["ts"].(string)
	if !ok {
		t.Fatal("record carries no time")
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("time %q is not RFC3339: %v", raw, err)
	}
	if _, offset := parsed.Zone(); offset != 0 {
		t.Errorf("time %q is not UTC", raw)
	}
}

func TestLevelFiltering(t *testing.T) {
	log, buf := capture(t, Options{Level: "warn"})

	log.Debug("debug")
	log.Info("info")
	log.Warn("warn")
	log.Error("error")

	rendered := buf.String()
	for _, dropped := range []string{`"debug"`, `"info"`} {
		if strings.Contains(rendered, dropped) {
			t.Errorf("a record below the threshold was emitted: %s", rendered)
		}
	}
	for _, kept := range []string{"warn", "error"} {
		if !strings.Contains(rendered, kept) {
			t.Errorf("a record at or above the threshold was dropped: %s", rendered)
		}
	}
}

func TestParseLevel(t *testing.T) {
	for name, want := range map[string]slog.Level{
		"debug": slog.LevelDebug, "DEBUG": slog.LevelDebug,
		"info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError,
	} {
		got, err := ParseLevel(name)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", name, got, err, want)
		}
	}
	if _, err := ParseLevel("verbose"); err == nil {
		t.Error("an unknown level was accepted")
	}
}

func TestNewRejectsAnUnknownLevel(t *testing.T) {
	if _, err := New(Options{Level: "chatty", Format: "json"}); err == nil {
		t.Error("an unknown level was accepted")
	}
}

// An unrecognised format falls back to JSON rather than failing. That is
// deliberate layering: config.Validate rejects anything but json or text, so
// this package does not need to duplicate the check, and a logger that refuses
// to build is a worse failure than one that ships structured output.
func TestUnknownFormatFallsBackToJSON(t *testing.T) {
	log, buf := capture(t, Options{Format: "yaml"})
	log.Info("msg")

	if _, err := json.Marshal(decode(t, buf)); err != nil {
		t.Errorf("fallback did not produce JSON: %s", buf)
	}
}

// Service identity is attached once so every record carries it, which is what
// makes logs from several deployments separable in one aggregator.
func TestServiceIdentityIsAttachedToEveryRecord(t *testing.T) {
	log, buf := capture(t, Options{
		Service: "payments", Environment: "staging", Version: "v1.2.3",
	})
	log.Info("msg")

	rec := decode(t, buf)
	for key, want := range map[string]string{
		"service": "payments", "env": "staging", "version": "v1.2.3",
	} {
		if rec[key] != want {
			t.Errorf("%s = %v, want %q", key, rec[key], want)
		}
	}
}
