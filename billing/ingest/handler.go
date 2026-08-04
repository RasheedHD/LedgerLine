// Package ingest accepts usage events over HTTP and writes them to Postgres.
//
// See ADR-0001 for the event schema, ADR-0002 for where deduplication is
// enforced, and ADR-0004 for this endpoint's contract.
package ingest

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"
)

// A request body large enough for any legitimate single event and small enough
// that a hostile client cannot make us allocate for it.
const maxBodyBytes = 64 << 10

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

// acceptedResponse is returned for both a new event and a duplicate.
//
// The status code is 202 either way -- a retry gets the same answer as the
// original -- while Duplicate tells a client that cares which one happened.
// ADR-0004 explains why both properties are needed at once.
type acceptedResponse struct {
	ID        int64 `json:"id"`
	Duplicate bool  `json:"duplicate"`
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// CRITICAL: this statement is where idempotency is enforced.
//
// Two things are load-bearing and neither is obvious.
//
// ON CONFLICT ... DO UPDATE rather than DO NOTHING. DO NOTHING returns zero
// rows on conflict, so it cannot tell us the id of the row that already
// exists, and recovering it with a follow-up SELECT has a race: under READ
// COMMITTED the conflicting row may belong to a transaction that has not
// committed yet, so the SELECT finds nothing. DO UPDATE blocks until that
// transaction resolves and then returns the row. The SET is deliberately a
// no-op -- assigning the column to itself -- because the update exists only to
// make RETURNING fire.
//
// (xmax = 0) distinguishes an insert from a conflict. xmax is a Postgres system
// column holding the id of the transaction that deleted or locked the row; a
// freshly inserted tuple has no such transaction and so has xmax = 0, while the
// tuple returned from the DO UPDATE path was locked and so does not. This is a
// Postgres implementation detail rather than standard SQL, and it is the price
// of learning insert-versus-conflict in a single round trip.
// The returned payload_fingerprint is the STORED one, not the one just
// offered: the DO UPDATE touches only tenant_id, so every other column in the
// returned row still holds its original value. That is what makes reuse
// detectable -- we get back what the key was first used for and can compare.
const insertEvent = `
INSERT INTO events (tenant_id, meter, quantity, occurred_at, received_at, idempotency_key, payload_fingerprint)
VALUES ($1, $2, $3::numeric, $4, $5, $6, $7)
ON CONFLICT (tenant_id, idempotency_key)
DO UPDATE SET tenant_id = events.tenant_id
RETURNING id, (xmax = 0) AS inserted, payload_fingerprint`

// Handler serves POST /events.
type Handler struct {
	db *sql.DB

	// Injected so the clock-skew and backfill-window rules can be tested at
	// fixed instants instead of relative to whenever the suite happens to run.
	now func() time.Time
}

// NewHandler returns the POST /events handler.
//
// The database handle is a parameter rather than a package-level global so
// this can be tested against a throwaway database without touching process
// state.
func NewHandler(db *sql.DB) *Handler {
	return &Handler{db: db, now: time.Now}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req, rej := h.decode(w, r)
	if rej != nil {
		writeError(w, rej)
		return
	}

	if rej := validate(req, h.now()); rej != nil {
		writeError(w, rej)
		return
	}

	// CRITICAL: the server stamps received_at, and does it here rather than in
	// SQL. This is the ingest-time half of ADR-0001's two-clock design -- every
	// late-event decision downstream is arithmetic on received_at minus
	// occurred_at, so if this value is wrong or client-supplied, that whole
	// policy is built on sand.
	receivedAt := h.now().UTC()

	offered := fingerprint(req)

	var id int64
	var inserted bool
	var stored []byte
	err := h.db.QueryRowContext(r.Context(), insertEvent,
		req.TenantID,
		req.Meter,
		req.Quantity,
		req.OccurredAt,
		receivedAt,
		req.IdempotencyKey,
		offered,
	).Scan(&id, &inserted, &stored)
	if err != nil {
		log.Printf("insert event: %v", err)
		writeError(w, &rejection{
			status: http.StatusInternalServerError,
			code:   "internal_error",
			detail: "could not store event",
		})
		return
	}

	// CRITICAL: a conflicting key whose stored payload differs is a reused key,
	// not a retry. Accepting it would discard genuinely different usage while
	// telling the client it was stored -- undercharging silently, which nobody
	// ever reports. ADR-0005.
	//
	// A NULL stored fingerprint means the row predates migration 000002, so
	// there is nothing to compare against and we do not pretend otherwise.
	if !inserted && stored != nil && !bytes.Equal(stored, offered) {
		writeError(w, &rejection{
			status: http.StatusConflict,
			code:   codeKeyReuse,
			detail: "idempotency_key was already used for an event with different content",
		})
		return
	}

	writeJSON(w, http.StatusAccepted, acceptedResponse{ID: id, Duplicate: !inserted})
}

func (h *Handler) decode(w http.ResponseWriter, r *http.Request) (*eventRequest, *rejection) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)

	// Unknown fields are rejected rather than ignored. A client sending
	// "quanity" or "received_at" has a bug, and silently dropping the field
	// means it bills the wrong amount and gets a 202 saying everything is
	// fine.
	dec.DisallowUnknownFields()

	var req eventRequest
	if err := dec.Decode(&req); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return nil, &rejection{
				status: http.StatusRequestEntityTooLarge,
				code:   codeMalformed,
				detail: "request body too large",
			}
		}
		return nil, &rejection{
			status: http.StatusBadRequest,
			code:   codeMalformed,
			detail: err.Error(),
		}
	}
	return &req, nil
}

func writeError(w http.ResponseWriter, rej *rejection) {
	writeJSON(w, rej.status, errorResponse{Error: errorBody{
		Code:   rej.code,
		Detail: rej.detail,
	}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	// The error is ignored on purpose: the status line is already sent and any
	// database work is already committed, so there is nothing useful left to
	// say to a client whose connection died mid-response.
	_ = json.NewEncoder(w).Encode(body)
}
