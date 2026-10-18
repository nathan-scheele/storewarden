// Package audit writes Audit records: one JSON line for every attempted Tool
// call, kept apart from the operational logs. A write that fails is reported
// to the caller, so that a call that can't be audited doesn't happen.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Phase says where in a Tool call a Record was written.
type Phase string

const (
	Denied  Phase = "denied"  // the call was refused; no other record follows
	Attempt Phase = "attempt" // the call was allowed and Shopify is about to be called
	Result  Phase = "result"  // the call has finished
)

// Record is one Audit record. It never holds credentials or Shopify
// response bodies, which can contain customer data.
type Record struct {
	Time      time.Time `json:"time"`
	RequestID string    `json:"request_id"`
	Phase     Phase     `json:"phase"`
	Caller    Caller    `json:"caller"`

	Tool     string          `json:"tool"`
	Args     json.RawMessage `json:"args"` // as the Caller sent them
	Decision Decision        `json:"decision"`
	Reason   string          `json:"reason"`

	DryRun         bool   `json:"dry_run"`
	IdempotencyKey string `json:"idempotency_key"`

	Outcome Outcome `json:"outcome"` // "" until the result record
	Error   string  `json:"error"`

	Shopify *Shopify `json:"shopify,omitempty"` // only once Shopify has been called

	DurationMS float64 `json:"duration_ms"`
}

// Decision says whether the call was allowed.
type Decision string

const (
	Allow Decision = "allow"
	Deny  Decision = "deny"
)

// Outcome says how an allowed call ended, on its result record.
type Outcome string

const (
	OK    Outcome = "ok"
	Error Outcome = "error" // the Caller got an error; Record.Error says which
)

// Caller is who made the call, as Tailscale identified them.
type Caller struct {
	NodeID   string   `json:"node_id"`
	NodeName string   `json:"node_name"`
	Tags     []string `json:"tags"`
	User     string   `json:"user"` // login name, or "tagged" for a tagged node
}

// Shopify is what Shopify reported about the queries one call made.
type Shopify struct {
	RequestedCost float64 `json:"requested_cost"`
	ActualCost    float64 `json:"actual_cost"`
	RequestID     string  `json:"request_id"` // of the last query
}

// Log writes Records as JSON Lines. It is safe for concurrent use.
type Log struct {
	logger *slog.Logger

	mu      sync.Mutex
	w       io.Writer
	file    *os.File // the file behind w, or nil when the Log doesn't own it
	healthy bool
}

// New returns a Log that writes to w. Write failures are logged to logger.
func New(w io.Writer, logger *slog.Logger) *Log {
	return &Log{w: w, logger: logger, healthy: true}
}

// Open returns a Log that appends to the file at path, or writes to stdout
// when path is "-".
func Open(path string, logger *slog.Logger) (*Log, error) {
	if path == "-" {
		return New(os.Stdout, logger), nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening audit log: %w", err)
	}
	l := New(f, logger)
	l.file = f
	return l, nil
}

// Write writes r as one line. If it returns an error, the record wasn't
// written, the error has been logged, and the Log reports itself unhealthy
// from then on.
func (l *Log) Write(r Record) error {
	if r.Time.IsZero() {
		r.Time = time.Now().UTC()
	}
	if err := l.write(r); err != nil {
		l.logger.Error("writing audit record failed", "request_id", r.RequestID, "phase", r.Phase, "error", err)
		return fmt.Errorf("writing audit record: %w", err)
	}
	return nil
}

func (l *Log) write(r Record) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// One Write per record, so concurrent records never interleave.
	if _, err := l.w.Write(append(line, '\n')); err != nil {
		l.healthy = false
		return err
	}
	return nil
}

// Healthy reports whether every record so far has been written.
func (l *Log) Healthy() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.healthy
}

// Close flushes the audit log to disk and closes it. It leaves stdout open.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	if err := l.file.Sync(); err != nil {
		l.file.Close()
		return fmt.Errorf("flushing audit log: %w", err)
	}
	return l.file.Close()
}

// NewRequestID returns a new random ID that names one request in the Audit
// records.
func NewRequestID() string {
	return rand.Text()
}

type requestIDKey struct{}

// WithRequestID returns a copy of ctx that carries the request ID.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the request ID in ctx, or "" if there is none.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}
