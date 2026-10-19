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

// PostInvoiceTool is the name of erp.post_vendor_invoice (T3, money). It is part of
// the idempotency key (I2), so it must never change for a deployed database.
const PostInvoiceTool = "erp.post_vendor_invoice"

// effectPostInvoice is the outbox effect type the relay delivers to mock-1c.
const effectPostInvoice = "onec.post_vendor_invoice"

// PostVendorInvoiceInput is the validated input of erp.post_vendor_invoice: a
// reference to the received 810 and nothing else.
type PostVendorInvoiceInput struct {
	SourceDocumentID string `json:"source_document_id"`
}

// PostVendorInvoice books a supplier invoice (a received 810) in the ERP, at most
// once per business key (supplier, BIG02).
//
// The amount, the lines and the details come from the typed 810. The policy checks
// them against the order to the supplier (the number is BIG04): no such order or a
// different amount or lines give policy_denied; a matching invoice always waits for
// two different operators (T3). Otherwise the call works like erp.create_sales_order.
func PostVendorInvoice(ctx context.Context, st *store.Store, dec Decider, c Caller, in PostVendorInvoiceInput) (OrderResult, error) {
	src, err := st.OrderSource(ctx, c.TenantID, in.SourceDocumentID, "810")
	if errors.Is(err, store.ErrNotFound) {
		return OrderResult{}, rpcerr.New(rpcerr.NotFound, "")
	}
	if err != nil {
		return OrderResult{}, err
	}
	body, err := bodyOf(ctx, st, c, src.ID, src.Body)
	if err != nil {
		return OrderResult{}, err
	}
	header, lines, err := document.Decode("810", body)
	if err != nil {
		return OrderResult{}, err
	}
	h, ok := header.(document.Header810)
	if !ok {
		return OrderResult{}, fmt.Errorf("document %s has no 810 header", src.ID)
	}
	if src.BKRef == "" {
		return OrderResult{}, rpcerr.Denied([]string{"missing_business_key"})
	}
	currency, err := st.DefaultCurrency(ctx, c.TenantID, src.PartnerID)
	if err != nil {
		return OrderResult{}, err
	}
	payload, norm, err := txn.BuildInvoice(h, lines, src.PartnerID, src.BKRef, currency)
	var deny *txn.DenyError
	if errors.As(err, &deny) {
		return OrderResult{}, rpcerr.Denied(deny.Explain)
	}
	if err != nil {
		return OrderResult{}, err
	}

	var orderRef string
	if h.OrderRef != nil {
		orderRef = *h.OrderRef
	}
	order, err := supplierOrderFor(ctx, st, c.TenantID, src.PartnerID, orderRef)
	if err != nil {
		return OrderResult{}, err
	}

	key, businessKey, err := requestKeys(c, PostInvoiceTool, src.PartnerID, src.BKRef, src.PartnerID, src.BKRef)
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
	amount, _ := policy.Cents(payload.Total)
	// The payload plus the lines in the normal form, which only the policy reads.
	effect := struct {
		txn.InvoicePayload
		LinesNorm [][]string `json:"lines_norm"`
	}{payload, policy.InvoiceLines(norm)}
	// The invoice number of the payload is the raw BIG02 (partner text); the operator
	// sees the value of the typed body, a note reference if it failed its format.
	view := map[string]any{
		"supplier_id": src.PartnerID, "invoice_number": h.BusinessRef, "order_number": deref(h.OrderRef),
		"lines": payload.Lines, "total": payload.Total, "currency": payload.Currency,
	}
	return reserveEffect(ctx, st, dec, c, effectRequest{
		Tool: PostInvoiceTool, EffectType: effectPostInvoice, Src: src, ST01: "810",
		Effect: effect, AmountCents: amount, ViewArgs: view, SupplierOrder: order,
		Key: key, BusinessKey: businessKey,
		ReqHash: reqHash, Payload: rawPayload,
	})
}

// supplierOrderFor reads the order to the supplier that an invoice names, in the form the
// policy compares. An empty number or an order that is not there gives nil (the policy then
// forbids the invoice).
func supplierOrderFor(ctx context.Context, st *store.Store, tenantID, partnerID, orderRef string) (*policy.SupplierOrder, error) {
	if orderRef == "" {
		return nil, nil
	}
	so, err := st.SupplierOrder(ctx, tenantID, partnerID, orderRef)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	lines, err := txn.NormSupplierOrder(so.Lines)
	if err != nil {
		return nil, err
	}
	return &policy.SupplierOrder{TotalCents: so.TotalCents, Currency: so.Currency, LinesNorm: policy.InvoiceLines(lines)}, nil
}
