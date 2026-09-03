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
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var pool *pgxpool.Pool

// defaultTestDSN targets a database dedicated to tests, not the one used for
// development. These tests TRUNCATE every table between cases, so pointing them
// at a working database destroys whatever is in it.
const defaultTestDSN = "postgres://payments:local_dev_pw@localhost:5440/payments_test?sslmode=disable"

// DATABASE_URL is deliberately NOT consulted. It is the application's own
// connection string, so honouring it here means a developer with a shell
// configured for staging - or production - runs a suite whose first act is to
// truncate the users table. Only TEST_DATABASE_URL is read, and even that is
// checked below.
func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = defaultTestDSN
	}
	if err := refuseNonTestDatabase(dsn); err != nil {
		fmt.Fprintf(os.Stderr, "integration: %v\n", err)
		os.Exit(1)
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
			"create it with `make test-db`, or set TEST_DATABASE_URL\n", redactDSN(dsn), err)
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

// refuseNonTestDatabase is the last line of defence before the first TRUNCATE.
//
// The name has to say it is a test database. That is a blunt rule, and it is
// the point: any subtler check is one someone talks themselves past at 2am,
// and the cost of being wrong is a wiped table with no undo.
func refuseNonTestDatabase(dsn string) error {
	name, err := databaseName(dsn)
	if err != nil {
		return fmt.Errorf("cannot read the database name from the connection string: %w", err)
	}
	if !strings.Contains(strings.ToLower(name), "test") {
		return fmt.Errorf(
			"refusing to run: these tests TRUNCATE every table, and %q is not named "+
				"as a test database.\nUse a database with \"test\" in its name "+
				"(`make test-db` creates one), or set TEST_DATABASE_URL", name)
	}
	return nil
}

func databaseName(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(u.Path, "/"), nil
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
