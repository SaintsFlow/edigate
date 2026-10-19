package adapters

import (
	"context"
	"time"
)

const mailTimeout = 15 * time.Second

var mailRoutes = map[string]string{
	EffectEmail: "/email",
}

// Mail is the client of the mail service of the stand (the partner emulator takes the
// letter at POST /email and answers GET /email?external_id= for a lookup). The gateway
// sends the template, the address and the order number; it never sends a text.
type Mail struct{ service }

// NewMail builds the client for the service at baseURL.
func NewMail(baseURL string) *Mail { return &Mail{newService(baseURL, mailTimeout, mailRoutes)} }

// WithReplyTo makes the client send the stand header Edigate-Reply-To (see ReplyToHeader).
func (m *Mail) WithReplyTo(publicURL string) *Mail {
	m.replyTo = publicURL
	return m
}

// Apply posts the letter. The service takes a repeat with the same external id once.
func (m *Mail) Apply(ctx context.Context, e Effect) (string, error) { return m.apply(ctx, e) }

// Exists asks the service whether the letter with this external id is already there.
func (m *Mail) Exists(ctx context.Context, effectType, externalID string) (bool, string, error) {
	return m.exists(ctx, effectType, externalID)
}
