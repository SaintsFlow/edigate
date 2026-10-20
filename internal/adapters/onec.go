package adapters

import (
	"context"
	"time"
)

// Timeout of one request to the ERP.
const onecTimeout = 15 * time.Second

// onecRoutes maps the effect type to the path of the 1C HTTP service. An effect type
// that is not here is refused for good. The cancel of an order has its own call.
var onecRoutes = map[string]string{
	EffectCreateOrder:          "/hs/orders",
	"onec.post_vendor_invoice": "/hs/vendor-invoices",
}

// Onec is the client of the 1C HTTP service (here: mock-1c). It sends an effect to
// POST <path> and looks it up with GET <path>?external_id=, where the path depends
// on the effect type (orders and supplier invoices); an order is cancelled with
// POST /hs/orders/{external_id}/cancel.
type Onec struct{ service }

// NewOnec builds the client for the service at baseURL (no trailing slash needed).
func NewOnec(baseURL string) *Onec { return &Onec{newService(baseURL, onecTimeout, onecRoutes)} }

// Apply performs the effect. The cancel names the order by its own external id (the
// payload holds it), not by the key of the compensation.
func (o *Onec) Apply(ctx context.Context, e Effect) (string, error) {
	if e.Type == EffectCancelOrder {
		return o.compensate(ctx, e, "order_external_id", "/hs/orders/", "cancel", "cancelled")
	}
	return o.apply(ctx, e)
}

// Exists asks mock-1c whether the effect is there. A cancel is idempotent and is
// simply sent again, so for it the answer is always no.
func (o *Onec) Exists(ctx context.Context, effectType, externalID string) (bool, string, error) {
	if effectType == EffectCancelOrder {
		return false, "", nil
	}
	return o.exists(ctx, effectType, externalID)
}
