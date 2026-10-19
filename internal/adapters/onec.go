package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Timeout of one request to the ERP.
const onecTimeout = 15 * time.Second

// routes maps the effect type to the path of the 1C HTTP service. An effect type
// that is not here is refused for good.
var routes = map[string]string{
	"onec.create_order":        "/hs/orders",
	"onec.post_vendor_invoice": "/hs/vendor-invoices",
}

func route(effectType string) (string, error) {
	p, ok := routes[effectType]
	if !ok {
		return "", &StatusError{Code: http.StatusBadRequest}
	}
	return p, nil
}

// Onec is the client of the 1C HTTP service (here: mock-1c). It sends an effect to
// POST <path> and looks it up with GET <path>?external_id=, where the path depends
// on the effect type (orders and supplier invoices).
type Onec struct {
	base string
	http *http.Client
}

// NewOnec builds the client for the service at baseURL (no trailing slash needed).
func NewOnec(baseURL string) *Onec {
	return &Onec{base: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: onecTimeout}}
}

// order is the part of the answer the gateway reads.
type order struct {
	ID json.Number `json:"id"`
}

// Apply posts the order. The payload of the outbox row is sent as it is, with the
// tenant and the external id added. 201 (new) and 200 (already there) both succeed.
func (o *Onec) Apply(ctx context.Context, e Effect) (string, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(e.Payload, &body); err != nil {
		return "", fmt.Errorf("payload is not an object: %w", err)
	}
	var err error
	if body["tenant_id"], err = json.Marshal(e.TenantID); err != nil {
		return "", err
	}
	if body["external_id"], err = json.Marshal(e.ExternalID); err != nil {
		return "", err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	path, err := route(e.Type)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.base+path, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	status, answer, err := o.do(req)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return "", &StatusError{Code: status}
	}
	return refOf(answer)
}

// Exists asks mock-1c for the effect of this type with this external id: 200 is a
// yes, 404 a no.
func (o *Onec) Exists(ctx context.Context, effectType, externalID string) (bool, string, error) {
	path, err := route(effectType)
	if err != nil {
		return false, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.base+path+"?external_id="+url.QueryEscape(externalID), nil)
	if err != nil {
		return false, "", err
	}
	status, answer, err := o.do(req)
	if err != nil {
		return false, "", err
	}
	switch status {
	case http.StatusOK:
		ref, err := refOf(answer)
		return err == nil, ref, err
	case http.StatusNotFound:
		return false, "", nil
	}
	return false, "", &StatusError{Code: status}
}

func (o *Onec) do(req *http.Request) (int, []byte, error) {
	resp, err := o.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, answer, err
}

func refOf(answer []byte) (string, error) {
	var o order
	if err := json.Unmarshal(answer, &o); err != nil || o.ID == "" {
		return "", fmt.Errorf("the ERP answer has no order id")
	}
	return o.ID.String(), nil
}
