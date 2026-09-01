// Command api is the HTTP entrypoint for the Stripe Payment & Subscription
// Gateway.
//
// Everything is constructed in run and passed down explicitly. There is no
// package-level state beyond the build stamps injected by -ldflags, and
// slog.SetDefault is deliberately not called: a dependency that logs through
// the default logger should be visible as a missing wire, not silently absorbed.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/mlkad/stripe-payment-service/internal/config"
	"github.com/mlkad/stripe-payment-service/internal/database"
	"github.com/mlkad/stripe-payment-service/internal/handler"
	"github.com/mlkad/stripe-payment-service/internal/logger"
	"github.com/mlkad/stripe-payment-service/internal/repository/postgres"
	"github.com/mlkad/stripe-payment-service/internal/service"
	paystripe "github.com/mlkad/stripe-payment-service/internal/stripe"
)

// Injected at build time via -ldflags.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	var (
		healthcheck = flag.Bool("healthcheck", false, "probe the local liveness endpoint and exit; used by the container HEALTHCHECK")
		showVersion = flag.Bool("version", false, "print version information and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("stripe-payment-service %s (commit %s, built %s)\n", version, commit, buildDate)
		return
	}
	if *healthcheck {
		if err := probe(); err != nil {
			fmt.Fprintln(os.Stderr, "healthcheck failed:", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		// The logger may not exist yet when configuration fails, so report the
		// fatal path on stderr unconditionally.
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	log, err := logger.New(logger.Options{
		Level:       cfg.Log.Level,
		Format:      cfg.Log.Format,
		AddSource:   cfg.Log.AddSource,
		Service:     cfg.App.Name,
		Environment: string(cfg.App.Environment),
		Version:     version,
	})
	if err != nil {
		return fmt.Errorf("build logger: %w", err)
	}

	log.Info("starting",
		slog.String("commit", commit),
		slog.String("build_date", buildDate),
		slog.Any("config", cfg), // Config.LogValue redacts every secret.
	)

	// signalCtx is cancelled on the first SIGINT/SIGTERM. It is used to bound
	// startup and to wake the shutdown sequence - never as the server's
	// BaseContext, which would cancel in-flight requests the instant the signal
	// arrives and defeat the drain.
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.New(signalCtx, database.Config{
		DSN:                cfg.Database.DSN.Reveal(),
		MaxConns:           cfg.Database.MaxConns,
		MinConns:           cfg.Database.MinConns,
		MaxConnLifetime:    cfg.Database.MaxConnLifetime,
		MaxConnIdleTime:    cfg.Database.MaxConnIdleTime,
		HealthCheckPeriod:  cfg.Database.HealthCheckPeriod,
		ConnectTimeout:     cfg.Database.ConnectTimeout,
		StatementTimeout:   cfg.Database.StatementTimeout,
		IdleInTxTimeout:    cfg.Database.IdleInTxTimeout,
		ApplicationName:    cfg.App.Name,
		SlowQueryThreshold: cfg.Database.SlowQueryThreshold,
	}, log)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	// Closed explicitly at the end of the shutdown sequence, after the HTTP
	// server has drained. The defer is the safety net for the error paths below.
	defer db.Close()

	stripeClient, err := paystripe.New(paystripe.Config{
		SecretKey:                cfg.Stripe.SecretKey.Reveal(),
		WebhookSecret:            cfg.Stripe.WebhookSecret.Reveal(),
		APIVersion:               cfg.Stripe.APIVersion,
		MaxNetworkRetries:        cfg.Stripe.MaxNetworkRetries,
		HTTPTimeout:              cfg.Stripe.HTTPTimeout,
		WebhookTolerance:         cfg.Stripe.WebhookTolerance,
		IgnoreAPIVersionMismatch: cfg.Stripe.IgnoreAPIVersionMismatch,
	}, log)
	if err != nil {
		return fmt.Errorf("build stripe client: %w", err)
	}

	userRepo := postgres.NewUserRepo(db.Pool())
	subRepo := postgres.NewSubscriptionRepo(db.Pool())
	webhookRepo := postgres.NewWebhookRepo(db.Pool(), 0)

	webhookService := service.NewWebhookService(userRepo, subRepo, webhookRepo, stripeClient, log)
	checkoutService, err := service.NewCheckoutService(userRepo, stripeClient, service.CheckoutConfig{
		SuccessURL:      cfg.Stripe.CheckoutSuccessURL,
		CancelURL:       cfg.Stripe.CheckoutCancelURL,
		AllowedPriceIDs: cfg.Stripe.AllowedPriceIDs,
	}, log)
	if err != nil {
		return fmt.Errorf("build checkout service: %w", err)
	}

	stripeHandler := handler.NewStripeHandler(webhookService, checkoutService, log)

	srv := &http.Server{
		Addr:              cfg.HTTP.Addr(),
		Handler:           newRouter(db, stripeHandler, log, version),
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return context.Background() },
	}

	// Listen before reporting readiness so that a port collision is a startup
	// error rather than a silent failure inside the goroutine.
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", srv.Addr, err)
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("http server listening", slog.String("addr", srv.Addr))
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-signalCtx.Done():
		// Stop trapping signals: a second Ctrl-C now terminates immediately,
		// which is what an operator expects when a drain is taking too long.
		stop()
		log.Info("shutdown signal received, draining requests",
			slog.String("timeout", cfg.App.ShutdownTimeout.String()))
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.App.ShutdownTimeout)
	defer cancel()

	// Order is load-bearing: drain HTTP first, then close the pool. Closing the
	// pool first would tear connections out from under requests that are still
	// finishing, potentially mid-transaction.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed, forcing close", slog.String("error", err.Error()))
		if closeErr := srv.Close(); closeErr != nil {
			log.Error("forced close failed", slog.String("error", closeErr.Error()))
		}
	}
	if err := <-serveErr; err != nil {
		log.Error("http server stopped with error", slog.String("error", err.Error()))
	}

	log.Info("http server drained")
	return nil
}

// newRouter builds the handler tree. Dependencies arrive as arguments; nothing
// is read from package scope.
func newRouter(db *database.DB, stripeHandler *handler.StripeHandler, log *slog.Logger, version string) http.Handler {
	h := &healthHandler{db: db, log: log, version: version, startedAt: time.Now()}

	mux := http.NewServeMux()
	stripeHandler.Register(mux)
	// Liveness: is the process running? Deliberately does not touch the
	// database. Restarting this container cannot fix a database outage, so
	// making liveness depend on the database converts a database blip into a
	// cluster-wide restart loop.
	mux.HandleFunc("GET /livez", h.live)
	// Readiness: should this instance receive traffic? Checks the database, as
	// every meaningful request needs it.
	mux.HandleFunc("GET /healthz", h.ready)

	// Applied outermost-first: recovery must wrap the logger so that a panic is
	// still recorded with its request id.
	return requestIDMiddleware(recoveryMiddleware(log)(accessLogMiddleware(log)(mux)))
}

type healthHandler struct {
	db        *database.DB
	log       *slog.Logger
	version   string
	startedAt time.Time
}

type healthResponse struct {
	Status  string           `json:"status"`
	Version string           `json:"version"`
	Uptime  string           `json:"uptime"`
	Checks  map[string]check `json:"checks,omitempty"`
}

type check struct {
	Status    string  `json:"status"`
	LatencyMS float64 `json:"latency_ms"`
	Error     string  `json:"error,omitempty"`
}

func (h *healthHandler) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{
		Status:  "ok",
		Version: h.version,
		Uptime:  h.uptime(),
	})
}

func (h *healthHandler) ready(w http.ResponseWriter, r *http.Request) {
	// Bound the probe independently of the request: a health endpoint that
	// blocks for the full statement timeout is useless to a load balancer.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	start := time.Now()
	err := h.db.HealthCheck(ctx)
	latency := float64(time.Since(start).Microseconds()) / 1000

	resp := healthResponse{
		Status:  "ok",
		Version: h.version,
		Uptime:  h.uptime(),
		Checks:  map[string]check{"database": {Status: "ok", LatencyMS: latency}},
	}
	status := http.StatusOK

	if err != nil {
		// The response carries a generic reason only. pgx connection errors
		// embed the database user, database name and host, and this endpoint is
		// routinely reachable from outside the cluster. The detail goes to the
		// log, where the operator who needs it can actually see it.
		resp.Status = "unavailable"
		resp.Checks["database"] = check{Status: "fail", LatencyMS: latency, Error: "unreachable"}
		status = http.StatusServiceUnavailable
		h.log.ErrorContext(ctx, "readiness check failed", slog.String("error", err.Error()))
	}
	writeJSON(w, status, resp)
}

func (h *healthHandler) uptime() string {
	return time.Since(h.startedAt).Truncate(time.Second).String()
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Health responses must never be served from a cache.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// --- middleware --------------------------------------------------------------
//
// These live here rather than in internal/handler because they serve the server
// itself: every route gets them, including the health probes, and they must wrap
// the Stripe surface from the outside.

const requestIDHeader = "X-Request-Id"

// requestIDMiddleware honours an inbound request id (so a trace survives a
// proxy hop) and otherwise mints one, then puts it on the context where the
// logger picks it up automatically.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if id == "" || len(id) > 128 {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(logger.WithRequestID(r.Context(), id)))
	})
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; a timestamp keeps the request
		// traceable rather than dropping correlation entirely.
		return fmt.Sprintf("ts-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// recoveryMiddleware converts a panic into a 500 and keeps the process alive.
func recoveryMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// A panic after the client disconnected is noise, not a defect.
				if errors.Is(r.Context().Err(), context.Canceled) {
					return
				}
				log.ErrorContext(r.Context(), "panic recovered",
					slog.Any("panic", rec),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("stack", string(debug.Stack())),
				)
				// The panic may have fired before or after the header was
				// written; net/http tolerates the redundant call with a log line
				// rather than corrupting the response.
				writeJSON(w, http.StatusInternalServerError, map[string]string{
					"error": "internal server error",
				})
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// accessLogMiddleware records one line per request at completion.
func accessLogMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			// Health probes fire every few seconds; logging them at info level
			// buries real traffic.
			level := slog.LevelInfo
			switch {
			case rec.status >= http.StatusInternalServerError:
				level = slog.LevelError
			case rec.status >= http.StatusBadRequest:
				level = slog.LevelWarn
			case r.URL.Path == "/livez" || r.URL.Path == "/healthz":
				level = slog.LevelDebug
			}

			log.LogAttrs(r.Context(), level, "request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int64("bytes", rec.written),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote_addr", clientIP(r)),
				slog.String("user_agent", r.UserAgent()),
			)
		})
	}
}

// statusRecorder captures the status code and response size for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	written     int64
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, so that
// flushing and deadline control keep working through the wrapper.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// probe backs the container HEALTHCHECK. It targets liveness rather than
// readiness: Docker restarts an unhealthy container, and restarting the API
// cannot repair a database outage.
func probe() error {
	port := os.Getenv("HTTP_PORT")
	if port == "" {
		port = "8080"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/livez")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}
