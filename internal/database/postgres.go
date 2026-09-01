// Package database owns the PostgreSQL connection pool.
//
// It deliberately does not import internal/config: the pool is configured
// through a local Config struct, so this package can be constructed in a test
// with a literal and carries no dependency on how the process reads its
// environment. main performs the mapping.
package database

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config describes the pool. Zero-valued fields fall back to the defaults in
// withDefaults, so a caller may set only DSN.
type Config struct {
	// DSN is a postgres:// URL or a libpq keyword/value string.
	DSN string

	// MaxConns bounds the pool. The ceiling that matters is PostgreSQL's
	// max_connections divided across every replica of every service - not what
	// this process would like to have.
	MaxConns int32
	// MinConns are kept warm. Connection setup costs a TLS handshake plus
	// authentication, which is latency paid on the request path if the pool is
	// allowed to drain to zero.
	MinConns int32

	// MaxConnLifetime recycles connections so that a long-lived pool follows a
	// failover or a rotated credential instead of pinning a dead backend.
	MaxConnLifetime time.Duration
	// MaxConnIdleTime releases connections the pool is not using.
	MaxConnIdleTime time.Duration
	// HealthCheckPeriod controls how often pgx sweeps idle connections.
	HealthCheckPeriod time.Duration
	// ConnectTimeout bounds both dialling and the startup Ping.
	ConnectTimeout time.Duration

	// StatementTimeout is applied server-side to every session. Without it a
	// single pathological query can hold a pool slot indefinitely and take the
	// service down by exhaustion rather than by error.
	StatementTimeout time.Duration
	// IdleInTxTimeout kills sessions left idle inside a transaction, which
	// otherwise hold locks and block VACUUM.
	IdleInTxTimeout time.Duration

	// ApplicationName appears in pg_stat_activity, so a DBA can attribute load.
	ApplicationName string

	// SlowQueryThreshold enables the query tracer. Zero disables tracing.
	SlowQueryThreshold time.Duration
}

func (c Config) withDefaults() Config {
	if c.MaxConns <= 0 {
		c.MaxConns = 25
	}
	if c.MinConns < 0 {
		c.MinConns = 0
	}
	if c.MaxConnLifetime <= 0 {
		c.MaxConnLifetime = time.Hour
	}
	if c.MaxConnIdleTime <= 0 {
		c.MaxConnIdleTime = 30 * time.Minute
	}
	if c.HealthCheckPeriod <= 0 {
		c.HealthCheckPeriod = time.Minute
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = 10 * time.Second
	}
	if c.ApplicationName == "" {
		c.ApplicationName = "stripe-payment-service"
	}
	if c.MinConns > c.MaxConns {
		c.MinConns = c.MaxConns
	}
	return c
}

// DB wraps a pgx pool with health checking and orderly shutdown.
type DB struct {
	pool *pgxpool.Pool
	log  *slog.Logger
	cfg  Config
}

// New builds the pool and verifies it with a Ping bounded by ConnectTimeout.
// A returned DB is ready for use; a returned error means nothing was left open.
func New(ctx context.Context, cfg Config, log *slog.Logger) (*DB, error) {
	if strings.TrimSpace(cfg.DSN) == "" {
		return nil, errors.New("database: DSN is empty")
	}
	if log == nil {
		return nil, errors.New("database: logger is nil")
	}
	cfg = cfg.withDefaults()

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		// The DSN carries the password. pgx does not echo it here, but wrap
		// defensively rather than trusting that to stay true.
		return nil, fmt.Errorf("database: parse DSN: %w", redactDSNError(err, cfg.DSN))
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolCfg.HealthCheckPeriod = cfg.HealthCheckPeriod
	poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout

	// Session parameters are set at startup rather than per query: they apply to
	// every connection the pool ever opens, including ones created later to meet
	// demand, and they cannot be forgotten at a call site.
	runtime := poolCfg.ConnConfig.RuntimeParams
	if runtime == nil {
		runtime = map[string]string{}
		poolCfg.ConnConfig.RuntimeParams = runtime
	}
	runtime["application_name"] = cfg.ApplicationName
	runtime["timezone"] = "UTC"
	if cfg.StatementTimeout > 0 {
		runtime["statement_timeout"] = millis(cfg.StatementTimeout)
	}
	if cfg.IdleInTxTimeout > 0 {
		runtime["idle_in_transaction_session_timeout"] = millis(cfg.IdleInTxTimeout)
	}

	if cfg.SlowQueryThreshold > 0 {
		poolCfg.ConnConfig.Tracer = &queryTracer{log: log, threshold: cfg.SlowQueryThreshold}
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("database: create pool: %w", err)
	}

	db := &DB{pool: pool, log: log, cfg: cfg}

	// Verify connectivity before declaring the process healthy. pgxpool.New is
	// lazy, so without this the first real request discovers the outage.
	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := db.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: initial ping: %w", err)
	}

	log.Info("database pool ready",
		slog.Int("max_conns", int(cfg.MaxConns)),
		slog.Int("min_conns", int(cfg.MinConns)),
		slog.String("max_conn_lifetime", cfg.MaxConnLifetime.String()),
		slog.String("statement_timeout", cfg.StatementTimeout.String()),
	)
	return db, nil
}

// Pool exposes the underlying pool for the repository layer.
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// Ping verifies that a connection can be acquired and reaches the server.
func (db *DB) Ping(ctx context.Context) error {
	if err := db.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	return nil
}

// HealthCheck is what the readiness endpoint calls. It runs a trivial query
// rather than only Ping: Ping can be satisfied by a cached connection, while
// `SELECT 1` proves the backend is actually answering.
func (db *DB) HealthCheck(ctx context.Context) error {
	var ok int
	if err := db.pool.QueryRow(ctx, "SELECT 1").Scan(&ok); err != nil {
		return fmt.Errorf("health check: %w", err)
	}
	if ok != 1 {
		return fmt.Errorf("health check: unexpected result %d", ok)
	}
	return nil
}

// Stats reports pool utilisation, suitable for a metrics exporter or a log line.
func (db *DB) Stats() map[string]any {
	s := db.pool.Stat()
	return map[string]any{
		"acquired":         s.AcquiredConns(),
		"idle":             s.IdleConns(),
		"total":            s.TotalConns(),
		"max":              s.MaxConns(),
		"acquire_count":    s.AcquireCount(),
		"canceled_acquire": s.CanceledAcquireCount(),
		"empty_acquire":    s.EmptyAcquireCount(),
	}
}

// Close drains the pool. It blocks until every checked-out connection is
// returned, so call it only after the HTTP server has finished draining -
// otherwise an in-flight request loses its connection mid-transaction.
func (db *DB) Close() {
	s := db.pool.Stat()
	db.log.Info("closing database pool",
		slog.Int("acquired_conns", int(s.AcquiredConns())),
		slog.Int("total_conns", int(s.TotalConns())),
	)
	db.pool.Close()
	db.log.Info("database pool closed")
}

// millis renders a duration as the integer milliseconds PostgreSQL expects for
// its timeout GUCs (statement_timeout and friends take a bare number as ms).
func millis(d time.Duration) string {
	return fmt.Sprintf("%d", d.Milliseconds())
}

// redactDSNError removes the DSN from an error string on the chance that a
// driver embeds it, since the DSN contains the password.
func redactDSNError(err error, dsn string) error {
	if dsn == "" {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, dsn) {
		return err
	}
	return errors.New(strings.ReplaceAll(msg, dsn, "[REDACTED DSN]"))
}

// queryTracer logs slow and failed queries. It is a pgx.QueryTracer, so it sees
// every query issued through the pool without any call-site cooperation.
type queryTracer struct {
	log       *slog.Logger
	threshold time.Duration
}

type traceKey struct{}

type traceState struct {
	start time.Time
	sql   string
}

func (t *queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, traceKey{}, &traceState{start: time.Now(), sql: data.SQL})
}

func (t *queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	st, ok := ctx.Value(traceKey{}).(*traceState)
	if !ok {
		return
	}
	elapsed := time.Since(st.start)

	switch {
	case data.Err != nil && !errors.Is(data.Err, pgx.ErrNoRows):
		// ErrNoRows is an ordinary outcome, not a fault.
		t.log.LogAttrs(ctx, slog.LevelError, "query failed",
			slog.String("sql", summarise(st.sql)),
			slog.Duration("elapsed", elapsed),
			slog.String("error", data.Err.Error()),
		)
	case elapsed >= t.threshold:
		t.log.LogAttrs(ctx, slog.LevelWarn, "slow query",
			slog.String("sql", summarise(st.sql)),
			slog.Duration("elapsed", elapsed),
			slog.String("threshold", t.threshold.String()),
		)
	default:
		t.log.LogAttrs(ctx, slog.LevelDebug, "query",
			slog.String("sql", summarise(st.sql)),
			slog.Duration("elapsed", elapsed),
		)
	}
}

// summarise collapses a statement to one line and caps its length, so that one
// large query cannot dominate a log budget.
func summarise(sql string) string {
	const max = 300
	s := strings.Join(strings.Fields(sql), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
