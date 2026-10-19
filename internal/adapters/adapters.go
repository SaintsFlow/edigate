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

// The effect types of the outbox. The first two are the order and the supplier invoice
// (mock-1c); the rest come with the sagas: the compensations (cancel, release), the
// warehouse (mock-wms) and the documents sent to a partner.
const (
	EffectCreateOrder = "onec.create_order"
	EffectCancelOrder = "onec.cancel_order"
	EffectWMSReserve  = "wms.reserve"
	EffectWMSRelease  = "wms.release"
	EffectWMSShip     = "wms.ship"
	EffectSend855     = "edi.send_855"
	EffectSend856     = "edi.send_856"
	EffectSend997     = "edi.send_997" // the acknowledgment of an incoming group; no key, no saga
	EffectEmail       = "notify.email"
)

// IsSend reports whether the effect is a document sent to a partner.
func IsSend(effectType string) bool {
	return effectType == EffectSend855 || effectType == EffectSend856 || effectType == EffectSend997
}

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
	// Exists asks whether an effect of this type with this external id is already there.
	Exists(ctx context.Context, effectType, externalID string) (found bool, ref string, err error)
}

// StatusError is an answer of the other system with an unexpected HTTP status.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("unexpected HTTP status %d", e.Code) }

// Permanent reports whether repeating the request cannot help: a 4xx answer
// other than 408 (timeout) and 429 (too many requests).
func (e *StatusError) Permanent() bool {
	return e.Code >= 400 && e.Code < 500 && e.Code != 408 && e.Code != 429
}
