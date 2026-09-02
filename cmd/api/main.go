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
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mlkad/stripe-payment-service/internal/auth"
	"github.com/mlkad/stripe-payment-service/internal/config"
	"github.com/mlkad/stripe-payment-service/internal/database"
	"github.com/mlkad/stripe-payment-service/internal/handler"
	"github.com/mlkad/stripe-payment-service/internal/handler/middleware"
	"github.com/mlkad/stripe-payment-service/internal/logger"
	"github.com/mlkad/stripe-payment-service/internal/repository/postgres"
	"github.com/mlkad/stripe-payment-service/internal/service"
	paystripe "github.com/mlkad/stripe-payment-service/internal/stripe"
	"github.com/mlkad/stripe-payment-service/internal/worker"
)

// Injected at build time via -ldflags.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	var (
		healthcheck   = flag.Bool("healthcheck", false, "probe the local liveness endpoint and exit; used by the container HEALTHCHECK")
		showVersion   = flag.Bool("version", false, "print version information and exit")
		webhookReport = flag.Bool("webhook-report", false, "print the unsettled webhook ledger and exit")
		webhookSweep  = flag.Bool("webhook-sweep", false, "run one webhook sweep and exit")
		retentionRun  = flag.Bool("retention-run", false, "run one payload retention pass and exit")
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

	if *retentionRun {
		if err := runRetentionTool(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if *webhookReport || *webhookSweep {
		if err := runWebhookTool(*webhookSweep); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
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

	hasher, err := auth.NewHasher(cfg.Auth.BcryptCost)
	if err != nil {
		return fmt.Errorf("build password hasher: %w", err)
	}
	tokens, err := auth.NewTokenService(auth.TokenConfig{
		Secret:   cfg.Auth.JWTSecret.Reveal(),
		Issuer:   cfg.Auth.JWTIssuer,
		Audience: cfg.Auth.JWTAudience,
		TTL:      cfg.Auth.AccessTokenTTL,
	})
	if err != nil {
		return fmt.Errorf("build token service: %w", err)
	}

	userRepo := postgres.NewUserRepo(db.Pool())
	subRepo := postgres.NewSubscriptionRepo(db.Pool())
	webhookRepo := postgres.NewWebhookRepo(db.Pool(), 0)

	webhookService := service.NewWebhookService(userRepo, subRepo, webhookRepo, stripeClient, log)
	checkoutService, err := service.NewCheckoutService(userRepo, stripeClient, service.CheckoutConfig{
		SuccessURL:      cfg.Stripe.CheckoutSuccessURL,
		CancelURL:       cfg.Stripe.CheckoutCancelURL,
		ReturnURL:       cfg.Stripe.CheckoutReturnURL,
		PortalReturnURL: cfg.Stripe.PortalReturnURL,
		AllowedPriceIDs: cfg.Stripe.AllowedPriceIDs,
	}, log)
	if err != nil {
		return fmt.Errorf("build checkout service: %w", err)
	}

	subscriptionService := service.NewSubscriptionService(subRepo, log)
	authService := service.NewAuthService(userRepo, hasher, tokens, log)

	authRateLimiter := middleware.NewRateLimiter(middleware.RateLimitConfig{
		Rate:  cfg.HTTP.AuthRateLimitRPS,
		Burst: cfg.HTTP.AuthRateLimitBurst,
	}, log)
	defer authRateLimiter.Close()

	if cfg.Retention.Enabled {
		retention := worker.NewRetentionWorker(webhookRepo, retentionConfig(cfg), log)
		go retention.Run(signalCtx)
	} else {
		log.Warn("payload retention is disabled; stored webhook payloads keep customer " +
			"email and billing address indefinitely")
	}

	if cfg.Sweeper.Enabled {
		sweeper := worker.NewWebhookSweeper(webhookRepo, webhookService, sweeperConfig(cfg), log)
		// Bound by signalCtx so the sweeper stops as soon as shutdown begins,
		// rather than starting a batch the drain would then wait on.
		go sweeper.Run(signalCtx)
	} else {
		log.Warn("webhook sweeper is disabled; failed events will not be retried or reported")
	}

	stripeHandler := handler.NewStripeHandler(webhookService, checkoutService, log)
	subscriptionHandler := handler.NewSubscriptionHandler(subscriptionService, log)
	authHandler := handler.NewAuthHandler(authService, log)
	healthHandler := handler.NewHealthHandler(db, log, version)

	srv := &http.Server{
		Addr: cfg.HTTP.Addr(),
		Handler: handler.NewRouter(stripeHandler, subscriptionHandler, authHandler, healthHandler, handler.RouterConfig{
			APITimeout:     cfg.HTTP.APITimeout,
			WebhookTimeout: cfg.HTTP.WebhookTimeout,
			CORS:           middleware.CORSConfig{AllowedOrigins: cfg.HTTP.CORSAllowedOrigins},
			Tokens:         tokens,
			AuthRateLimit:  authRateLimiter,
			TrustedProxies: cfg.HTTP.TrustedProxies,
		}, log),
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

func sweeperConfig(cfg *config.Config) worker.SweeperConfig {
	return worker.SweeperConfig{
		Interval:        cfg.Sweeper.Interval,
		StaleClaimAfter: cfg.Sweeper.StaleClaimAfter,
		MaxAttempts:     int32(cfg.Sweeper.MaxAttempts),
		BaseBackoff:     cfg.Sweeper.BaseBackoff,
		MaxBackoff:      cfg.Sweeper.MaxBackoff,
		BatchSize:       cfg.Sweeper.BatchSize,
		AlertAfter:      cfg.Sweeper.AlertAfter,
	}
}

func retentionConfig(cfg *config.Config) worker.RetentionConfig {
	return worker.RetentionConfig{
		Interval:       cfg.Retention.Interval,
		SettledAfter:   cfg.Retention.SettledPayloadAfter,
		UnsettledAfter: cfg.Retention.UnsettledPayloadAfter,
		BatchSize:      cfg.Retention.BatchSize,
	}
}

// runRetentionTool backs -retention-run: one pass, then report and exit. Needs
// no Stripe client, since minimisation happens entirely inside the database.
func runRetentionTool() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	log, err := logger.New(logger.Options{
		Level: cfg.Log.Level, Format: cfg.Log.Format,
		Service: cfg.App.Name, Environment: string(cfg.App.Environment), Version: version,
	})
	if err != nil {
		return fmt.Errorf("build logger: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	db, err := database.New(ctx, database.Config{
		DSN:              cfg.Database.DSN.Reveal(),
		MaxConns:         4,
		MinConns:         1,
		ConnectTimeout:   cfg.Database.ConnectTimeout,
		StatementTimeout: cfg.Database.StatementTimeout,
		ApplicationName:  cfg.App.Name + "-retention",
	}, log)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer db.Close()

	w := worker.NewRetentionWorker(
		postgres.NewWebhookRepo(db.Pool(), cfg.Sweeper.StaleClaimAfter),
		retentionConfig(cfg), log)

	result := w.RunOnce(ctx)
	stats, err := w.Stats(ctx)
	if err != nil {
		return fmt.Errorf("read retention stats: %w", err)
	}

	fmt.Printf("purged %d payload(s): %d settled, %d unresolved\n",
		result.Total(), result.Settled, result.Unsettled)
	fmt.Printf("still holding full payloads  %d\n", stats.WithPayload)
	fmt.Printf("already minimised            %d\n", stats.Purged)
	fmt.Printf("past window, not yet purged  %d\n", stats.DueNow)
	if stats.OldestPayload != nil {
		fmt.Printf("oldest full payload          %s (%s ago)\n",
			stats.OldestPayload.Format(time.RFC3339),
			time.Since(*stats.OldestPayload).Truncate(time.Second))
	}
	// Non-zero exit when data is being kept past its window, so a cron entry
	// or a compliance check can act without parsing the output.
	if stats.DueNow > 0 {
		os.Exit(2)
	}
	return nil
}

// runWebhookTool backs -webhook-report and -webhook-sweep.
//
// A CLI rather than an HTTP route on purpose: an admin endpoint needs an
// authorisation model this service does not have, and inventing one to expose
// diagnostics would be a larger security surface than the diagnostics are
// worth. This runs with the same configuration and database access as the
// service, which is what an operator or a cron job already has.
func runWebhookTool(sweep bool) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	log, err := logger.New(logger.Options{
		Level: cfg.Log.Level, Format: cfg.Log.Format,
		Service: cfg.App.Name, Environment: string(cfg.App.Environment), Version: version,
	})
	if err != nil {
		return fmt.Errorf("build logger: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db, err := database.New(ctx, database.Config{
		DSN:              cfg.Database.DSN.Reveal(),
		MaxConns:         4,
		MinConns:         1,
		ConnectTimeout:   cfg.Database.ConnectTimeout,
		StatementTimeout: cfg.Database.StatementTimeout,
		ApplicationName:  cfg.App.Name + "-webhook-tool",
	}, log)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer db.Close()

	webhookRepo := postgres.NewWebhookRepo(db.Pool(), cfg.Sweeper.StaleClaimAfter)

	if !sweep {
		stats, err := webhookRepo.LedgerStats(ctx, int32(cfg.Sweeper.MaxAttempts))
		if err != nil {
			return fmt.Errorf("read ledger: %w", err)
		}
		printLedger(stats)
		// Non-zero exit when a human is needed, so a cron entry or a monitoring
		// check can act on it without parsing the output.
		if stats.DeadLettered > 0 {
			os.Exit(2)
		}
		return nil
	}

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

	webhookService := service.NewWebhookService(
		postgres.NewUserRepo(db.Pool()),
		postgres.NewSubscriptionRepo(db.Pool()),
		webhookRepo, stripeClient, log)

	result := worker.NewWebhookSweeper(webhookRepo, webhookService, sweeperConfig(cfg), log).Sweep(ctx)
	fmt.Printf("reclaimed %d, retried %d, recovered %d\n",
		result.Reclaimed, result.Retried, result.Recovered)
	printLedger(result.Stats)
	if result.Stats.DeadLettered > 0 {
		os.Exit(2)
	}
	return nil
}

func printLedger(stats postgres.LedgerStats) {
	fmt.Printf("processing    %d\n", stats.Processing)
	fmt.Printf("retryable     %d\n", stats.Retryable)
	fmt.Printf("dead-lettered %d\n", stats.DeadLettered)
	if stats.OldestUnsettled != nil {
		fmt.Printf("oldest        %s (%s ago)\n",
			stats.OldestUnsettled.Format(time.RFC3339),
			time.Since(*stats.OldestUnsettled).Truncate(time.Second))
	}
	if stats.DeadLettered > 0 {
		fmt.Println("\nDead-lettered events will not be retried again. Inspect them with:")
		fmt.Println("  SELECT event_id, event_type, attempts, last_error, updated_at")
		fmt.Println("  FROM processed_webhooks WHERE status = 'failed' ORDER BY updated_at;")
		fmt.Println("\nAfter fixing the cause, reset their attempt count to make them retryable:")
		fmt.Println("  UPDATE processed_webhooks SET attempts = 0 WHERE event_id = '...';")
	}
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
