package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"
)

const wmsTimeout = 15 * time.Second

var wmsRoutes = map[string]string{
	EffectWMSReserve: "/reservations",
	EffectWMSShip:    "/shipments",
}

// WMS is the client of the warehouse service (here: mock-wms): a reservation, its release,
// and the shipment with the facts (the SSCC of every pack and the quantity that left).
type WMS struct{ service }

// NewWMS builds the client for the service at baseURL.
func NewWMS(baseURL string) *WMS { return &WMS{newService(baseURL, wmsTimeout, wmsRoutes)} }

// Apply performs the effect. A release names the reservation by its own external id.
func (w *WMS) Apply(ctx context.Context, e Effect) (string, error) {
	if e.Type == EffectWMSRelease {
		return w.compensate(ctx, e, "reservation_external_id", "/reservations/", "release", "released")
	}
	return w.apply(ctx, e)
}

// Exists asks the warehouse whether the reservation or the shipment is there.
func (w *WMS) Exists(ctx context.Context, effectType, externalID string) (bool, string, error) {
	if effectType == EffectWMSRelease {
		return false, "", nil
	}
	return w.exists(ctx, effectType, externalID)
}

// ShipLine is one line of a shipment as the warehouse recorded it.
type ShipLine struct {
	SKU  string `json:"sku"`
	Qty  string `json:"qty"`
	Unit string `json:"unit"`
}

// Shipment is the fact of the warehouse: what left and the SSCC of each line (the same order).
type Shipment struct {
	ExternalID string     `json:"external_id"`
	OrderRef   string     `json:"order_ref"`
	Lines      []ShipLine `json:"lines"`
	SSCC       []string   `json:"sscc"`
}

// ShipNow ships without an external id and returns what the warehouse recorded, with the SSCC
// of every line (the answer of POST /shipments carries them). The open tools use it: they
// have no key, so there is nothing to look the shipment up by afterwards.
func (w *WMS) ShipNow(ctx context.Context, tenantID string, payload json.RawMessage) (Shipment, error) {
	reply, err := w.post(ctx, Effect{Type: EffectWMSShip, TenantID: tenantID, Payload: payload})
	if err != nil {
		return Shipment{}, err
	}
	var s Shipment
	if err := json.Unmarshal(reply, &s); err != nil || len(s.Lines) == 0 || len(s.Lines) != len(s.SSCC) {
		return Shipment{}, errors.New("the warehouse answer holds no shipped lines")
	}
	return s, nil
}

// Shipment reads the shipment with this external id (the key of the step wms.ship in hex).
func (w *WMS) Shipment(ctx context.Context, externalID string) (Shipment, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.base+wmsRoutes[EffectWMSShip]+"?external_id="+url.QueryEscape(externalID), nil)
	if err != nil {
		return Shipment{}, err
	}
	status, reply, err := w.do(req)
	if err != nil {
		return Shipment{}, err
	}
	if status != http.StatusOK {
		return Shipment{}, &StatusError{Code: status}
	}
	var s Shipment
	if err := json.Unmarshal(reply, &s); err != nil {
		return Shipment{}, err
	}
	return s, nil
}
