// Package testdb gives integration tests a real Postgres to run against.
//
// The deliberate choice here is to test against a real database rather than a
// mock. Every correctness claim in this project lives in a constraint, a
// transaction, or a type — none of which a mock can reproduce. A mocked
// "insert" that returns a canned duplicate error proves nothing about whether
// the unique constraint is actually there.
package testdb

import (
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Tests run against their own database, never the one used for manual poking.
// The schema is dropped and rebuilt on first use, so pointing this at the dev
// database would destroy whatever is in it.
const (
	adminDSN = "postgres://ledgerline:ledgerline@localhost:5432/ledgerline?sslmode=disable"
	testDSN  = "postgres://ledgerline:ledgerline@localhost:5432/ledgerline_test?sslmode=disable"
	testName = "ledgerline_test"
)

// schemaOnce guards the drop-and-rebuild so it happens once per test binary
// rather than once per test. Individual tests get isolation from truncation,
// which is far cheaper than re-running migrations.
var schemaOnce sync.Once

// New returns a handle to the test database with the schema applied and every
// table empty.
//
// If no database is reachable the test is skipped, not failed. That keeps
// `go test ./...` useful on a machine where Docker is not running, at the cost
// of silently proving nothing — so CI must assert that these tests actually
// ran rather than trusting a green result.
func New(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if !ensureDatabaseExists(t) {
			return nil
		}
		dsn = testDSN
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		t.Skipf("no test database reachable (try: docker compose up -d): %v", err)
	}
	t.Cleanup(func() { db.Close() })

	schemaOnce.Do(func() { rebuildSchema(t, db) })
	truncateAll(t, db)

	return db
}

// ensureDatabaseExists creates ledgerline_test if it is not already there,
// connecting via the main database to do it. Returns false if the server is
// unreachable, having already skipped the test.
func ensureDatabaseExists(t *testing.T) bool {
	t.Helper()

	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()

	if err := admin.Ping(); err != nil {
		t.Skipf("no Postgres reachable (try: docker compose up -d): %v", err)
		return false
	}

	var exists bool
	err = admin.QueryRow("SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", testName).Scan(&exists)
	if err != nil {
		t.Fatalf("check for test database: %v", err)
	}
	if exists {
		return true
	}

	// CREATE DATABASE cannot run inside a transaction block and cannot take a
	// placeholder for the name, so this is string-interpolated. Safe only
	// because testName is a compile-time constant.
	if _, err := admin.Exec("CREATE DATABASE " + testName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	return true
}

// rebuildSchema drops everything and replays the migrations from scratch, so
// the tests always run against exactly what migrations/ describes. A stale
// hand-patched test schema is a class of bug worth designing out entirely.
func rebuildSchema(t *testing.T, db *sql.DB) {
	t.Helper()

	dir := migrationsDir(t)

	// Down migrations in reverse order, then up migrations in order. The down
	// files use DROP ... IF EXISTS, so running them against an empty database
	// is a no-op rather than an error.
	for _, path := range migrationFiles(t, dir, ".down.sql", true) {
		exec(t, db, path)
	}
	for _, path := range migrationFiles(t, dir, ".up.sql", false) {
		exec(t, db, path)
	}
}

// truncateAll empties every table between tests. RESTART IDENTITY resets the
// id sequence so tests can assert on specific ids without depending on how
// many tests ran before them.
func truncateAll(t *testing.T, db *sql.DB) {
	t.Helper()

	rows, err := db.Query(`
		SELECT tablename FROM pg_tables
		WHERE schemaname = 'public' AND tablename <> 'schema_migrations'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
	if len(names) == 0 {
		return
	}

	stmt := "TRUNCATE " + strings.Join(names, ", ") + " RESTART IDENTITY CASCADE"
	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func exec(t *testing.T, db *sql.DB, path string) {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if _, err := db.Exec(string(body)); err != nil {
		t.Fatalf("apply %s: %v", filepath.Base(path), err)
	}
}

func migrationFiles(t *testing.T, dir, suffix string, reverse bool) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}

	var paths []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}

	// Filenames are zero-padded (000001_, 000002_), so lexical order is
	// migration order.
	sort.Strings(paths)
	if reverse {
		for i, j := 0, len(paths)-1; i < j; i, j = i+1, j-1 {
			paths[i], paths[j] = paths[j], paths[i]
		}
	}
	return paths
}

// migrationsDir walks up from the test's working directory looking for go.mod,
// so this works regardless of which package's directory `go test` ran from.
func migrationsDir(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "migrations")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod above the working directory")
		}
		dir = parent
	}
}
