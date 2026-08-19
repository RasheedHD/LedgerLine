// Command ingest runs the HTTP endpoint that accepts usage events, and the
// consumer that drains them into Postgres.
//
// One process for now. The two halves are independent -- ingest needs only the
// log, the consumer needs the log and the database -- so splitting them into
// separate processes later is a wiring change rather than a redesign.
package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Imported for its side effect only: registering the "pgx" driver name
	// with database/sql. Every line below is standard library API, so the
	// driver could be swapped without touching anything else.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/RasheedHD/LedgerLine/billing/consumer"
	"github.com/RasheedHD/LedgerLine/billing/ingest"
	brokerlog "github.com/RasheedHD/LedgerLine/broker/log"
)

const (
	// Matches docker-compose.yml. Acceptable as a fallback for a container
	// that only ever runs on a laptop; anywhere real this must come from the
	// environment and never be committed.
	devDSN = "postgres://ledgerline:ledgerline@localhost:5432/ledgerline?sslmode=disable"

	defaultLogDir = "data/log"

	// How often the consumer looks for new records. Short enough that events
	// reach the database promptly, long enough that an idle system is not
	// hammering Postgres.
	drainInterval = 500 * time.Millisecond
)

func main() {
	logDir := envOr("LEDGERLINE_LOG_DIR", defaultLogDir)
	dsn := envOr("DATABASE_URL", devDSN)

	// CRITICAL: SyncGroup, not SyncNever.
	//
	// Ingest returns 202 on the strength of the append, so the append has to be
	// genuinely durable before it returns. Under SyncNever the record may still
	// be only in the page cache and the 202 is a promise that does not survive
	// power loss -- invariant I3 broken by construction. SyncGroup gives the
	// same guarantee as fsyncing every append while letting concurrent requests
	// share one flush. See ADR-0008.
	brokerLog, err := brokerlog.OpenDurable(logDir, brokerlog.Options{Sync: brokerlog.SyncGroup})
	if err != nil {
		log.Fatalf("open log: %v", err)
	}
	defer brokerLog.Close()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	// Deliberately NOT fatal.
	//
	// This is the availability the log was added to buy: ingest needs only the
	// log, so the endpoint can accept and durably store usage while Postgres is
	// down, and the consumer catches up when it returns. Refusing to start here
	// would throw that away for no reason.
	if err := db.Ping(); err != nil {
		log.Printf("WARNING: database unreachable (%v); ingest will accept events and the consumer will catch up later", err)
	}

	// Cancelled on shutdown to stop the consumer loop.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		// .Log unwraps to the plain type: the consumer reads the log and
		// acknowledges nobody, so it has no need of the durable guarantee.
		runConsumer(ctx, brokerLog.Log, db)
	}()

	mux := http.NewServeMux()

	// Go 1.22+ lets the standard mux match on method, so a GET to /events gets
	// a 405 automatically and there is no hand-rolled method check.
	mux.Handle("POST /events", ingest.NewHandler(brokerLog))

	srv := &http.Server{
		Addr:    ":8080",
		Handler: mux,

		// http.Server has no timeouts by default. Without this, a client that
		// opens a connection and never finishes sending headers holds a
		// goroutine open indefinitely.
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("ingest listening on :8080, log at %s", logDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")

	// Shutdown stops accepting new connections and waits for in-flight requests
	// to finish. Without it, a request that has appended to the log but not yet
	// written its response is cut off, and the client retries an event that was
	// in fact stored -- a duplicate manufactured by our own shutdown.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}

	// Wait for the consumer to finish its last drain before closing the log
	// underneath it.
	<-drained
	log.Println("stopped")
}

// runConsumer drains the log into Postgres until the context is cancelled.
func runConsumer(ctx context.Context, brokerLog *brokerlog.Log, db *sql.DB) {
	c := consumer.New("billing", brokerLog, db, consumer.Options{})

	ticker := time.NewTicker(drainInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// One last drain on the way out, with a fresh context because the
			// original is already cancelled. Best-effort: anything still
			// unconsumed is picked up on the next start, which is exactly what
			// the committed offset is for.
			final, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if stats, err := drain(final, c); err != nil {
				log.Printf("final drain: %v", err)
			} else if stats.Read > 0 {
				log.Printf("final drain: %+v", stats)
			}
			return

		case <-ticker.C:
			if _, err := drain(ctx, c); err != nil {
				// Logged and retried on the next tick rather than fatal. The
				// database being briefly unavailable is an expected condition,
				// not a reason to stop accepting usage.
				log.Printf("drain: %v", err)
			}
		}
	}
}

func drain(ctx context.Context, c *consumer.Consumer) (consumer.Stats, error) {
	stats, err := c.Drain(ctx)
	if err != nil {
		return stats, err
	}
	if stats.Conflicts > 0 {
		log.Printf("WARNING: %d events dropped as idempotency-key reuse", stats.Conflicts)
	}
	return stats, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
