package adapters

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Mux gives every effect to the adapter that owns its type: onec.* to the ERP, wms.* to
// the warehouse and notify.* to the mail service. An adapter that is nil was not
// configured: the effect fails with a temporary error and is tried again, so that
// configuring it later is enough.
type Mux struct {
	Onec Adapter
	WMS  Adapter
	Mail Adapter
}

func (m Mux) pick(effectType string) (Adapter, error) {
	var a Adapter
	switch {
	case strings.HasPrefix(effectType, "onec."):
		a = m.Onec
	case strings.HasPrefix(effectType, "wms."):
		a = m.WMS
	case strings.HasPrefix(effectType, "notify."):
		a = m.Mail
	default:
		return nil, &StatusError{Code: http.StatusBadRequest}
	}
	if a == nil {
		return nil, fmt.Errorf("no adapter is configured for %s", effectType)
	}
	return a, nil
}

// Apply performs the effect in the system that owns its type.
func (m Mux) Apply(ctx context.Context, e Effect) (string, error) {
	a, err := m.pick(e.Type)
	if err != nil {
		return "", err
	}
	return a.Apply(ctx, e)
}

// Exists asks the system that owns the type.
func (m Mux) Exists(ctx context.Context, effectType, externalID string) (bool, string, error) {
	a, err := m.pick(effectType)
	if err != nil {
		return false, "", err
	}
	return a.Exists(ctx, effectType, externalID)
}
