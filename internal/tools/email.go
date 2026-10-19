package tools

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/SaintsFlow/edigate/internal/adapters"
	"github.com/SaintsFlow/edigate/internal/rpcerr"
	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/txn"
)

// SendEmailTool is notify.send_email (T2, a sink). It is part of the idempotency key.
const SendEmailTool = "notify.send_email"

// SendEmailInput is the validated input of notify.send_email. There is no body: the
// letter is made from a template and the order number by the one that sends it (INJ-03).
type SendEmailInput struct {
	SourceDocumentID string `json:"source_document_id"`
	To               string `json:"to"`
	TemplateID       string `json:"template_id"`
}

// emailPayload is the effect: what the mail service gets. It holds no free text of the agent.
type emailPayload struct {
	PartnerID   string `json:"partner_id"`
	To          string `json:"to"`
	TemplateID  string `json:"template_id"`
	BusinessRef string `json:"business_ref"`
}

// SendEmail queues a letter to a contact of the partner of a received 850, once per
// (partner, BEG03, template). The policy checks that the address is a contact of that
// partner (email.recipient, XP-01); the template must be one the tenant has. A second
// call with the same key and another address is a conflicting duplicate.
func SendEmail(ctx context.Context, st *store.Store, dec Decider, c Caller, in SendEmailInput) (OrderResult, error) {
	src, err := st.OrderSource(ctx, c.TenantID, in.SourceDocumentID, "850")
	if errors.Is(err, store.ErrNotFound) {
		return OrderResult{}, rpcerr.New(rpcerr.NotFound, "")
	}
	if err != nil {
		return OrderResult{}, err
	}
	if src.BKRef == "" {
		return OrderResult{}, rpcerr.Denied([]string{"missing_business_key"})
	}
	known, err := st.HasEmailTemplate(ctx, c.TenantID, in.TemplateID)
	if err != nil {
		return OrderResult{}, err
	}
	if !known {
		return OrderResult{}, rpcerr.Denied([]string{"email.template"})
	}
	payload := emailPayload{PartnerID: src.PartnerID, To: in.To, TemplateID: in.TemplateID, BusinessRef: src.BKRef}
	raw, err := json.Marshal(payload)
	if err != nil {
		return OrderResult{}, err
	}
	reqHash, err := txn.ReqHash(payload)
	if err != nil {
		return OrderResult{}, err
	}
	// The business key is (partner, BEG03, template); BEG03 has no NUL, so the join is exact.
	businessKey, err := json.Marshal([]string{src.PartnerID, src.BKRef, in.TemplateID})
	if err != nil {
		return OrderResult{}, err
	}
	view := map[string]any{"partner_id": src.PartnerID, "to": in.To, "template_id": in.TemplateID}
	return reserveEffect(ctx, st, dec, c, effectRequest{
		Tool: SendEmailTool, EffectType: adapters.EffectEmail, Src: src, ST01: "850",
		Effect: payload, ViewArgs: view,
		Key:         txn.Key(c.TenantID, SendEmailTool, src.PartnerID, src.BKRef+"\x00"+in.TemplateID),
		BusinessKey: businessKey, ReqHash: reqHash, Payload: raw,
	})
}
