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

// service is an HTTP service of the stand (mock-1c, mock-wms) that takes an effect
// as POST <path> with a JSON body and answers GET <path>?external_id= for a lookup.
// routes maps an effect type to its path; a type that is not there is refused for good.
type service struct {
	base   string
	http   *http.Client
	routes map[string]string
}

func newService(baseURL string, timeout time.Duration, routes map[string]string) service {
	return service{base: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: timeout}, routes: routes}
}

func (s *service) route(effectType string) (string, error) {
	p, ok := s.routes[effectType]
	if !ok {
		return "", &StatusError{Code: http.StatusBadRequest}
	}
	return p, nil
}

// answer is the part of a reply the gateway reads: the id the service gave the effect.
type answer struct {
	ID json.Number `json:"id"`
}

// apply posts the payload of the outbox row as it is, with the tenant and the external
// id added. 201 (new) and 200 (already there) both succeed; the result is the id.
func (s *service) apply(ctx context.Context, e Effect) (string, error) {
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
	path, err := s.route(e.Type)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+path, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	status, reply, err := s.do(req)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return "", &StatusError{Code: status}
	}
	return refOf(reply)
}

// exists asks the service for the effect of this type with this external id: 200 is a
// yes, 404 a no.
func (s *service) exists(ctx context.Context, effectType, externalID string) (bool, string, error) {
	path, err := s.route(effectType)
	if err != nil {
		return false, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+path+"?external_id="+url.QueryEscape(externalID), nil)
	if err != nil {
		return false, "", err
	}
	status, reply, err := s.do(req)
	if err != nil {
		return false, "", err
	}
	switch status {
	case http.StatusOK:
		ref, err := refOf(reply)
		return err == nil, ref, err
	case http.StatusNotFound:
		return false, "", nil
	}
	return false, "", &StatusError{Code: status}
}

// compensate sends a compensation: POST <prefix><target>/<action>, where target is the
// external id of the effect to undo, named in the payload field. The service undoes what
// is there and does nothing for what is not, so the call can be repeated.
func (s *service) compensate(ctx context.Context, e Effect, field, prefix, action, result string) (string, error) {
	var p map[string]string
	if err := json.Unmarshal(e.Payload, &p); err != nil || p[field] == "" {
		return "", &StatusError{Code: http.StatusBadRequest}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+prefix+url.PathEscape(p[field])+"/"+action, nil)
	if err != nil {
		return "", err
	}
	status, _, err := s.do(req)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", &StatusError{Code: status}
	}
	return result, nil
}

func (s *service) do(req *http.Request) (int, []byte, error) {
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, reply, err
}

func refOf(reply []byte) (string, error) {
	var a answer
	if err := json.Unmarshal(reply, &a); err != nil || a.ID == "" {
		return "", fmt.Errorf("the service answer has no id")
	}
	return a.ID.String(), nil
}
