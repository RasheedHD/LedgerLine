// Package ingest accepts usage events over HTTP and writes them to Postgres.
//
// Scope right now is deliberately small: decode, insert, respond. No dedup,
// no validation, no ledger posting. See ADR-0001 for the event schema.
package ingest

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// eventRequest is the JSON body of POST /events.
//
// Note what is absent: received_at. That column is ours, not the client's --
// it records when we took durable custody, which the client cannot know.
type eventRequest struct {
	TenantID string `json:"tenant_id"`
	Meter    string `json:"meter"`

	// Quantity stays a string from the wire all the way to Postgres and is
	// never parsed into a Go number. ADR-0001 section 3: converting to
	// float64 here would reintroduce exactly the precision loss NUMERIC
	// exists to prevent, and it would happen silently.
	Quantity string `json:"quantity"`

	// encoding/json parses RFC 3339 into time.Time natively, so
	// "2026-07-28T10:00:00Z" needs no manual handling.
	OccurredAt time.Time `json:"occurred_at"`

	// Client-generated and reused verbatim on retry. ADR-0001 section 2.
	IdempotencyKey string `json:"idempotency_key"`
}

// $3::numeric is an explicit cast, not decoration. We hand Postgres a Go
// string for a NUMERIC(38,9) column; the cast states how to parse it rather
// than leaving it to driver type inference.
//
// RETURNING id gives us something to show the caller, which is what makes a
// duplicate visible: the first request returns an id, the second does not.
const insertEvent = `
INSERT INTO events (tenant_id, meter, quantity, occurred_at, received_at, idempotency_key)
VALUES ($1, $2, $3::numeric, $4, $5, $6)
RETURNING id`

// NewHandler returns the POST /events handler.
//
// The database handle is a parameter rather than a package-level global so
// this can later be tested against a throwaway database without touching
// process state.
func NewHandler(db *sql.DB) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req eventRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "malformed JSON body", http.StatusBadRequest)
			return
		}

		// CRITICAL: the server stamps received_at, and does it here rather
		// than in SQL. This is the ingest-time half of ADR-0001's two-clock
		// design -- every late-event decision downstream is arithmetic on
		// received_at minus occurred_at, so if this value is wrong or
		// client-supplied, that whole policy is built on sand.
		receivedAt := time.Now().UTC()

		var id int64
		err := db.QueryRowContext(r.Context(), insertEvent,
			req.TenantID,
			req.Meter,
			req.Quantity,
			req.OccurredAt,
			receivedAt,
			req.IdempotencyKey,
		).Scan(&id)
		if err != nil {
			// Deliberately naive: the error is not inspected, so a duplicate
			// idempotency key surfaces as a 500. That is the bug Phase 1
			// exists to fix -- a client that did nothing wrong, retrying a
			// request it never got a response to, is told the server broke.
			// Logged in full here, terse to the caller, so the constraint
			// name is visible in your terminal.
			log.Printf("insert event: %v", err)
			http.Error(w, "could not store event", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)

		// Encode's error is ignored on purpose: the row is already committed
		// and the status line is already sent, so there is nothing useful
		// left to say to a client whose connection died mid-response.
		_ = json.NewEncoder(w).Encode(map[string]int64{"id": id})
	})
}
