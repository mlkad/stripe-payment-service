// Package logger builds the process-wide structured logger on log/slog.
//
// The logger is constructed once in main and injected; nothing here touches
// slog.SetDefault, so there is no global to accidentally reconfigure from a
// library or a test.
//
// Three behaviours make it production-grade rather than merely structured:
//
//   - Request correlation. The handler pulls a request id out of the context on
//     every record, so a log line written five layers deep inside webhook
//     processing still carries the id of the delivery that caused it - without
//     threading a logger through every signature.
//   - Redaction. Attributes whose key looks sensitive are replaced before they
//     are encoded. It is a backstop, not the primary defence (config.Secret is),
//     but log statements are written under time pressure and this one catches
//     the mistakes.
//   - UTC timestamps. Containers disagree about local time; log aggregators do
//     not forgive that.
package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const redacted = "[REDACTED]"

// Options configures New. The zero value is usable: it yields an info-level
// JSON logger on stderr.
type Options struct {
	// Level is one of debug, info, warn, error. Empty means info.
	Level string
	// Format is json or text. Empty means json. Text is for a human at a
	// terminal; never ship it.
	Format string
	// AddSource attaches file:line. Useful in staging, costly in a hot path.
	AddSource bool
	// Service, Environment and Version are attached to every record so that
	// records from different deployments stay separable in one aggregator.
	Service     string
	Environment string
	Version     string
	// Output defaults to os.Stderr. Logs belong on stderr: stdout is for program
	// output, and conflating them corrupts piped data.
	Output io.Writer
}

// New builds a logger from opts. It returns an error only for an unparseable
// level, so that a typo in LOG_LEVEL is reported rather than silently coerced.
func New(opts Options) (*slog.Logger, error) {
	level, err := ParseLevel(opts.Level)
	if err != nil {
		return nil, err
	}

	out := opts.Output
	if out == nil {
		out = os.Stderr
	}

	handlerOpts := &slog.HandlerOptions{
		Level:       level,
		AddSource:   opts.AddSource,
		ReplaceAttr: replaceAttr,
	}

	var h slog.Handler
	if strings.EqualFold(opts.Format, "text") {
		h = slog.NewTextHandler(out, handlerOpts)
	} else {
		h = slog.NewJSONHandler(out, handlerOpts)
	}

	// Order matters: contextHandler must wrap the encoder so that it can inject
	// attributes before encoding.
	h = &contextHandler{Handler: h}

	base := make([]slog.Attr, 0, 3)
	if opts.Service != "" {
		base = append(base, slog.String("service", opts.Service))
	}
	if opts.Environment != "" {
		base = append(base, slog.String("env", opts.Environment))
	}
	if opts.Version != "" {
		base = append(base, slog.String("version", opts.Version))
	}
	if len(base) > 0 {
		h = h.WithAttrs(base)
	}

	return slog.New(h), nil
}

// ParseLevel maps a level name to a slog.Level. An empty name means info.
func ParseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logger: unknown level %q (want debug, info, warn or error)", name)
	}
}

// replaceAttr normalises the built-in keys and redacts sensitive values.
func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 {
		switch a.Key {
		case slog.TimeKey:
			if t, ok := a.Value.Any().(time.Time); ok {
				return slog.String("ts", t.UTC().Format(time.RFC3339Nano))
			}
		case slog.LevelKey:
			if lv, ok := a.Value.Any().(slog.Level); ok {
				return slog.String(slog.LevelKey, strings.ToLower(lv.String()))
			}
		case slog.SourceKey:
			// Absolute build paths leak the builder's filesystem and bloat
			// every record; package/file.go:line is what is actually useful.
			if src, ok := a.Value.Any().(*slog.Source); ok && src != nil {
				short := filepath.Join(filepath.Base(filepath.Dir(src.File)), filepath.Base(src.File))
				return slog.String(slog.SourceKey, fmt.Sprintf("%s:%d", short, src.Line))
			}
		}
	}
	if isSensitiveKey(a.Key) && a.Value.Kind() != slog.KindGroup {
		return slog.String(a.Key, redacted)
	}
	return a
}

// sensitiveKeys are matched as substrings of a lowercased attribute key.
var sensitiveKeys = []string{
	"password", "passwd", "secret", "token", "api_key", "apikey",
	"authorization", "cookie", "credential", "private_key",
	"dsn", "database_url", "signature",
}

func isSensitiveKey(key string) bool {
	k := strings.ToLower(key)
	for _, s := range sensitiveKeys {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// contextHandler enriches every record with values carried on the context.
type contextHandler struct{ slog.Handler }

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestIDFromContext(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs and WithGroup must rewrap, or the context enrichment is silently
// dropped the first time a caller derives a child logger.
func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup keeps the context enrichment alive across grouping, but the
// injected request_id lands *inside* the group rather than at the top level.
//
// That is a limitation of the handler model rather than an oversight: Handle
// receives a record and can only add attributes through the handler it holds,
// which by then is already grouped. Escaping the group would mean emitting a
// second record.
//
// The id is still present and still queryable, just as "<group>.request_id".
// Prefer With over WithGroup on a request-scoped logger if a flat field
// matters to your log aggregator.
func (h *contextHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &contextHandler{Handler: h.Handler.WithGroup(name)}
}

// ctxKey is unexported so no other package can collide with these context keys.
type ctxKey int

const requestIDKey ctxKey = iota

// WithRequestID returns a context carrying id, which the logger attaches to
// every record written with that context.
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFromContext returns the request id, or "" when none is set.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}
