//go:build integration

// Package integration exercises the repositories against a real PostgreSQL 16.
//
// These tests assert behaviour that cannot be observed without a database:
// constraint translation, trigger side effects, and the two concurrency
// guarantees the service depends on - the webhook claim and the out-of-order
// event guard.
//
//	make test-integration
package integration

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var pool *pgxpool.Pool

const defaultTestDSN = "postgres://payments:local_dev_pw@localhost:5440/payments?sslmode=disable"

func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		dsn = defaultTestDSN
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var err error
	pool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: cannot build pool: %v\n", err)
		os.Exit(1)
	}
	if err := pool.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "integration: database unreachable at %s: %v\n"+
			"start it with `make up`, or set TEST_DATABASE_URL\n", redactDSN(dsn), err)
		os.Exit(1)
	}

	code := m.Run()
	pool.Close()
	os.Exit(code)
}

// truncate resets the tables between tests. CASCADE is safe here because the
// schema has no tables outside this set.
func truncate(t *testing.T) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`TRUNCATE processed_webhooks, refresh_tokens, subscriptions, users CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

func redactDSN(dsn string) string {
	if i := indexOf(dsn, '@'); i > 0 {
		return "postgres://[REDACTED]" + dsn[i:]
	}
	return "[REDACTED]"
}

func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
