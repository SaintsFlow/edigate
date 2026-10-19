package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/SaintsFlow/edigate/internal/edi/document"
	"github.com/SaintsFlow/edigate/internal/policy"
	"github.com/SaintsFlow/edigate/internal/rpcerr"
	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/txn"
)

// This file holds the tools with an effect on the ERP and the status of such an
// effect: erp.create_sales_order (T1) and txn.status (T0).

// CreateOrderTool is the name under which the order is reserved. It is part of the
// idempotency key (I2), so it must never change for a deployed database.
const CreateOrderTool = "erp.create_sales_order"

// effectCreateOrder is the outbox effect type the relay delivers to mock-1c.
const effectCreateOrder = "onec.create_order"

// CreateSalesOrderInput is the validated input of erp.create_sales_order. The
// agent passes a reference to the document and a closed value: no price, partner,
// address or quantity.
type CreateSalesOrderInput struct {
	SourceDocumentID    string `json:"source_document_id"`
	DeliveryInstruction string `json:"delivery_instruction,omitempty"`
}

// OrderResult is the answer of the tools with an effect (erp.create_sales_order,
// erp.post_vendor_invoice). Status is accepted (queued), pending_approval (with
// ApprovalID), or for a failed key rejected or expired. Reason is set for a
// rejected one only (the recorded outcome of a failed key).
type OrderResult struct {
	Status     string `json:"status"`
	TxnID      string `json:"txn_id"`
	Reason     string `json:"reason,omitempty"`
	ApprovalID string `json:"approval_id,omitempty"`
}

// CreateSalesOrder turns a received 850 into an order in the ERP, at most once per
// business key (partner, BEG03).
//
// The document must be an 850 of the tenant in the status received or processed
// (otherwise not_found). Every line is priced from the price list; a SKU that is
// not in the list or is a note reference gives policy_denied with
// explain ["unknown_sku"]. Then the policy decides (internal/policy): refused
// (policy_denied with the rule names), queued, or waiting for one operator. The
// key, the request hash, the outbox row or the approval request, and the audit row
// are written in one transaction (store.Reserve). A call that repeats an earlier
// one gets the same txn_id; the same business key with a different request gives
// conflicting_duplicate.
func CreateSalesOrder(ctx context.Context, st *store.Store, dec Decider, c Caller, in CreateSalesOrderInput) (OrderResult, error) {
	src, err := st.OrderSource(ctx, c.TenantID, in.SourceDocumentID, "850")
	if errors.Is(err, store.ErrNotFound) {
		return OrderResult{}, rpcerr.New(rpcerr.NotFound, "")
	}
	if err != nil {
		return OrderResult{}, err
	}
	header, lines, err := document.Decode("850", src.Body)
	if err != nil {
		return OrderResult{}, err
	}
	h, ok := header.(document.Header850)
	if !ok {
		return OrderResult{}, fmt.Errorf("document %s has no 850 header", src.ID)
	}
	if src.BKRef == "" {
		return OrderResult{}, rpcerr.Denied([]string{"missing_business_key"})
	}
	prices, err := st.Prices(ctx, c.TenantID, txn.SKUs(lines))
	if err != nil {
		return OrderResult{}, err
	}
	payload, err := txn.BuildOrder(h, lines, src.PartnerID, src.BKRef, in.DeliveryInstruction, prices)
	var deny *txn.DenyError
	if errors.As(err, &deny) {
		return OrderResult{}, rpcerr.Denied(deny.Explain)
	}
	if err != nil {
		return OrderResult{}, err
	}

	businessKey, err := txn.BusinessKeyJSON(src.PartnerID, src.BKRef)
	if err != nil {
		return OrderResult{}, err
	}
	reqHash, err := txn.ReqHash(payload)
	if err != nil {
		return OrderResult{}, err
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return OrderResult{}, err
	}
	amount, _ := policy.Cents(payload.Total) // the total is built by BuildOrder: digits with two places
	// What the operator may see: values of the typed body (a value that failed its
	// format is a note reference there) and of the price list. BusinessRef of the
	// payload is the raw BEG03, which is partner text, so the body's value is used.
	view := map[string]any{
		"partner_id": src.PartnerID, "document_number": h.BusinessRef, "ship_to": deref(h.ShipTo),
		"lines": payload.Lines, "total": payload.Total, "currency": payload.Currency,
		"delivery_instruction": payload.DeliveryInstruction,
	}
	return reserveEffect(ctx, st, dec, c, effectRequest{
		Tool: CreateOrderTool, EffectType: effectCreateOrder, Src: src, ST01: "850",
		Effect: payload, AmountCents: amount, Counted: true, ViewArgs: view,
		Key: txn.Key(c.TenantID, CreateOrderTool, src.PartnerID, src.BKRef), BusinessKey: businessKey,
		ReqHash: reqHash, Payload: rawPayload,
	})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// TxnStatusInput is the validated input of txn.status.
type TxnStatusInput struct {
	TxnID string `json:"txn_id"`
}

// TxnStatusOutput is the answer of txn.status. ExternalRef is null until the
// effect is delivered. saga_state arrives with the sagas of wave 2-7.
type TxnStatusOutput struct {
	TxnID       string  `json:"txn_id"`
	Status      string  `json:"status"`
	ExternalRef *string `json:"external_ref"`
}

// TxnStatus reports where a transaction of the tenant stands: accepted (queued),
// completed (the effect is confirmed) or rejected (failed or dead). A txn_id of
// another tenant and an unknown one both give not_found.
func TxnStatus(ctx context.Context, st *store.Store, tenantID string, in TxnStatusInput) (TxnStatusOutput, error) {
	t, err := st.TxnState(ctx, tenantID, in.TxnID)
	if errors.Is(err, store.ErrNotFound) {
		return TxnStatusOutput{}, rpcerr.New(rpcerr.NotFound, "")
	}
	if err != nil {
		return TxnStatusOutput{}, err
	}
	out := TxnStatusOutput{TxnID: in.TxnID}
	switch t.State {
	case "completed":
		out.Status = "completed"
	case "failed":
		out.Status = "rejected"
		if t.ResultStatus == "expired" {
			out.Status = "expired"
		}
	case "reopened":
		out.Status = "rejected"
	default:
		out.Status = "accepted"
		if t.ResultStatus == "pending_approval" {
			out.Status = "pending_approval"
		}
	}
	if t.ExternalRef != "" {
		out.ExternalRef = &t.ExternalRef
	}
	return out, nil
}
