// Package ingest accepts usage events over HTTP and appends them to the broker
// log.
//
// Ingest does not talk to Postgres. It validates, appends, and answers. Every
// correctness concern beyond "is this request well formed" -- deduplication,
// reuse detection, pricing, posting -- happens downstream in the consumer.
//
// That division is the point: ingest stays available and fast when the database
// is slow or down, which is the reason for putting a log in front of it at all.
// The cost is that ingest can no longer tell a client whether its event was a
// duplicate, because it cannot see the events table. See ADR-0012.
//
// See ADR-0001 for the event schema and ADR-0004 for the rejection taxonomy.
package ingest

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/RasheedHD/LedgerLine/billing/event"
	brokerlog "github.com/RasheedHD/LedgerLine/broker/log"
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

	// Quantity stays a string from the wire all the way to storage and is
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

// acceptedResponse is returned once the event is durable in the log.
//
// The offset is the event's position in the log, which is the only identifier
// ingest can honestly hand out: the database row does not exist yet and may not
// for another few milliseconds.
//
// There is deliberately no `duplicate` field. Ingest cannot know -- that answer
// lives in the events table, which is downstream. ADR-0012 explains why that
// was the right trade and what it costs.
type acceptedResponse struct {
	Offset uint64 `json:"offset"`
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// Handler serves POST /events.
type Handler struct {
	log *brokerlog.Log

	// Injected so the clock-skew and backfill-window rules can be tested at
	// fixed instants instead of relative to whenever the suite happens to run.
	now func() time.Time
}

// NewHandler returns the POST /events handler.
//
// CRITICAL: the log passed here must use brokerlog.SyncGroup.
//
// This handler returns 202 on the strength of the append, so the append has to
// be genuinely durable before it returns. Under SyncNever or SyncEveryN the
// record may still be only in the page cache, and the 202 becomes a promise the
// system cannot keep across a power failure -- invariant I3 broken by
// construction. See ADR-0008.
func NewHandler(l *brokerlog.Log) *Handler {
	return &Handler{log: l, now: time.Now}
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

	// CRITICAL: received_at is stamped here, immediately before the append that
	// makes it durable.
	//
	// ADR-0001 defines it as "when we took durable custody". That moment used
	// to be the Postgres insert and is now the log append -- the event is safe
	// once it is in the log, whether or not the database has caught up. Every
	// late-event decision downstream is arithmetic on received_at minus
	// occurred_at, so this value is what the whole late-event policy rests on.
	receivedAt := h.now().UTC()

	e := &event.UsageEvent{
		TenantID:       req.TenantID,
		Meter:          req.Meter,
		Quantity:       req.Quantity,
		OccurredAt:     req.OccurredAt,
		ReceivedAt:     receivedAt,
		IdempotencyKey: req.IdempotencyKey,
	}

	// Computed at ingest rather than by the consumer so that the value stored
	// is the one derived from what the client actually sent, before any
	// downstream code has had a chance to reinterpret it.
	e.Fingerprint = event.Fingerprint(e)

	record, err := event.Encode(e)
	if err != nil {
		log.Printf("encode event: %v", err)
		writeError(w, internalError())
		return
	}

	offset, err := h.log.Append(record)
	if err != nil {
		log.Printf("append event: %v", err)
		writeError(w, internalError())
		return
	}

	writeJSON(w, http.StatusAccepted, acceptedResponse{Offset: offset})
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

func internalError() *rejection {
	return &rejection{
		status: http.StatusInternalServerError,
		code:   "internal_error",
		detail: "could not accept event",
	}
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

	// The error is ignored on purpose: the status line is already sent and the
	// record is already durable, so there is nothing useful left to say to a
	// client whose connection died mid-response.
	_ = json.NewEncoder(w).Encode(body)
}
