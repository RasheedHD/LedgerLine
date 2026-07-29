// Command ingest runs the HTTP endpoint that accepts usage events.
package main

import (
	"database/sql"
	"log"
	"net/http"
	"os"
	"time"

	// Imported for its side effect only: registering the "pgx" driver name
	// with database/sql. The blank identifier means we never call this
	// package directly -- every line below is standard library API, so the
	// driver could be swapped without touching handler code.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/RasheedHD/LedgerLine/billing/ingest"
)

// Matches docker-compose.yml. Acceptable as a fallback for a container that
// only ever runs on a laptop; anywhere real this must come from the
// environment and never be committed.
const devDSN = "postgres://ledgerline:ledgerline@localhost:5432/ledgerline?sslmode=disable"

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = devDSN
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	// sql.Open does not connect. It validates the DSN and prepares a lazy
	// pool, nothing more. Without this ping a stopped container or a wrong
	// password would first appear as a failing request, which reads like a
	// handler bug rather than a startup problem.
	if err := db.Ping(); err != nil {
		log.Fatalf("ping database: %v", err)
	}

	mux := http.NewServeMux()

	// Go 1.22+ lets the standard mux match on method, so a GET to /events
	// gets a 405 automatically and there is no hand-rolled method check.
	mux.Handle("POST /events", ingest.NewHandler(db))

	srv := &http.Server{
		Addr:    ":8080",
		Handler: mux,

		// http.Server has no timeouts by default. Without this, a client that
		// opens a connection and never finishes sending headers holds a
		// goroutine open indefinitely -- a trivial way to exhaust the server.
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Println("ingest listening on :8080")
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server: %v", err)
	}
}
