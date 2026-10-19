// Package adapters holds the clients through which the relay performs effects in
// other systems. The gateway only calls the addresses it was configured with
// (EDIGATE_ONEC_URL); nothing in a document or a tool argument chooses an address.
//
// Input: an Effect read from the outbox. Output: the reference the other system
// gave the effect, or an error. An error that wraps *StatusError carries the HTTP
// status, which tells a permanent refusal from a temporary fault.
package adapters

import (
	"context"
	"encoding/json"
	"fmt"
)

// Effect is one outbox row as an adapter sees it. ExternalID is the idempotency
// key in hex: the other system uses it to take a repeated delivery only once (I4).
type Effect struct {
	Type       string
	TenantID   string
	ExternalID string
	Payload    json.RawMessage
}

// Adapter performs effects in one other system.
type Adapter interface {
	// Apply performs the effect and returns the reference of the other system.
	// A second Apply with the same ExternalID returns the same reference.
	Apply(ctx context.Context, e Effect) (ref string, err error)
	// Exists asks whether an effect with this external id is already there.
	Exists(ctx context.Context, externalID string) (found bool, ref string, err error)
}

// StatusError is an answer of the other system with an unexpected HTTP status.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("unexpected HTTP status %d", e.Code) }

// Permanent reports whether repeating the request cannot help: a 4xx answer
// other than 408 (timeout) and 429 (too many requests).
func (e *StatusError) Permanent() bool {
	return e.Code >= 400 && e.Code < 500 && e.Code != 408 && e.Code != 429
}
