package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/SaintsFlow/edigate/internal/adapters"
	"github.com/SaintsFlow/edigate/internal/decimal"
	"github.com/SaintsFlow/edigate/internal/edi/outbound"
	"github.com/SaintsFlow/edigate/internal/policy"
	"github.com/SaintsFlow/edigate/internal/rpcerr"
	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/txn"
)

// This file holds the tools with an effect of the mode no-contracts (C3-A4). They have the
// open arguments of the modes plain and reqhash (the values come from the model, there is no
// reference to a received document), but everything after the arguments is the gateway: the
// policy decides, the key is made of the business key that the arguments carry (partner and
// number), and the effect goes through the outbox, the sagas and the receipts. The answer is
// the one of the full gateway.

// noDocument stands for the received document that these calls do not have: the key and the
// saga need a document id, and no row of edi_documents has this one.
const noDocument = "00000000-0000-0000-0000-000000000000"

// OpenCreateSalesOrder reserves the order of the model: the prices and quantities are its own.
// The policy still checks the partner, the ship-to against the partner's codes, and the limits.
func OpenCreateSalesOrder(ctx context.Context, st *store.Store, dec Decider, c Caller, args json.RawMessage) (OrderResult, error) {
	payload, err := orderOfArgs(args)
	if err != nil {
		return OrderResult{}, err
	}
	key, businessKey, err := requestKeys(c, CreateOrderTool, payload.PartnerID, payload.BusinessRef, payload.PartnerID, payload.BusinessRef)
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
	amount, _ := policy.Cents(payload.Total) // priceLines wrote the total with two places
	kept, err := json.Marshal(sagaOrder{OrderPayload: payload, OrderDate: today()})
	if err != nil {
		return OrderResult{}, err
	}
	view := map[string]any{
		"partner_id": payload.PartnerID, "document_number": payload.BusinessRef, "ship_to": payload.ShipTo,
		"lines": payload.Lines, "total": payload.Total, "currency": payload.Currency,
	}
	return reserveEffect(ctx, st, dec, c, effectRequest{
		Tool: CreateOrderTool, EffectType: effectCreateOrder, Src: store.OrderSource{ID: noDocument, PartnerID: payload.PartnerID}, ST01: "850",
		Effect: payload, AmountCents: amount, Counted: true, ViewArgs: view,
		Key: key, BusinessKey: businessKey, ReqHash: reqHash, Payload: rawPayload,
		Saga: &store.SagaLink{
			Kind: "order", StepNo: 1, Steps: []string{adapters.EffectCreateOrder, adapters.EffectSend855},
			Payload: kept, SourceDocumentID: noDocument,
		},
	})
}

// OpenConfirmOrder sends the 855 of the order the partner and the reference name. Lines in the
// arguments replace the lines of the order; their prices are those of the order, 0.00 for a SKU
// that is not in it.
func OpenConfirmOrder(ctx context.Context, st *store.Store, dec Decider, c Caller, args json.RawMessage) (OrderResult, error) {
	var in struct {
		PartnerID   string `json:"partner_id"`
		BusinessRef string `json:"business_ref"`
		AckType     string `json:"ack_type"`
		Lines       []struct {
			SKU  string      `json:"sku"`
			Qty  json.Number `json:"qty"`
			Unit string      `json:"unit"`
		} `json:"lines"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return OrderResult{}, rpcerr.New(rpcerr.SchemaInvalid, "")
	}
	o, kept, err := orderByRef(ctx, st, c.TenantID, in.PartnerID, in.BusinessRef)
	if err != nil {
		return OrderResult{}, err
	}
	spec := outbound.Spec{
		ST01: "855", PartnerID: o.PartnerID, Ref: o.Ref, OrderDate: kept.OrderDate, Currency: kept.Currency,
		AckType: in.AckType, ShipTo: kept.ShipTo, Lines: orderLines(kept, in.AckType == "RJ"),
	}
	if len(in.Lines) > 0 {
		prices := map[string]string{}
		for _, l := range kept.Lines {
			prices[l.SKU] = l.UnitPrice
		}
		spec.Lines = spec.Lines[:0]
		for _, l := range in.Lines {
			qty, ok := decimal.ParseScaled(l.Qty.String(), 3)
			if !ok {
				return OrderResult{}, rpcerr.New(rpcerr.SchemaInvalid, "a quantity has more than 3 places")
			}
			status := "IA"
			if in.AckType == "RJ" {
				status = "IR"
			}
			spec.Lines = append(spec.Lines, outbound.Line{SKU: l.SKU, Qty: decimal.FormatScaled(qty, 3), Unit: l.Unit, UnitPrice: priceOr(prices, l.SKU), Status: status})
		}
	}
	return sendDocument(ctx, st, dec, c, o, kept, spec, effectKeys{
		tool: ConfirmOrderTool, policyTool: ConfirmOrderTool, effect: adapters.EffectSend855, kind: "order", step: 2,
	})
}

// openWarehouseRequest turns the lines of the model into the request to the warehouse; the unit
// of a line is the one of the order, EA for a SKU that is not in it.
func openWarehouseRequest(ref string, kept sagaOrder, in openWarehouse) (map[string]any, error) {
	units := map[string]string{}
	for _, l := range kept.Lines {
		units[l.SKU] = l.Unit
	}
	lines := make([]map[string]string, 0, len(in.Lines))
	for _, l := range in.Lines {
		qty, ok := decimal.ParseScaled(l.Qty.String(), 3)
		if !ok {
			return nil, rpcerr.New(rpcerr.SchemaInvalid, "a quantity has more than 3 places")
		}
		unit := units[l.SKU]
		if unit == "" {
			unit = "EA"
		}
		lines = append(lines, map[string]string{"sku": l.SKU, "qty": decimal.FormatScaled(qty, 3), "unit": unit})
	}
	return map[string]any{"order_ref": ref, "lines": lines}, nil
}

// OpenWMSReserve reserves the lines of the model for the order that the reference names.
func OpenWMSReserve(ctx context.Context, st *store.Store, dec Decider, c Caller, args json.RawMessage) (OrderResult, error) {
	var in openWarehouse
	if err := json.Unmarshal(args, &in); err != nil {
		return OrderResult{}, rpcerr.New(rpcerr.SchemaInvalid, "")
	}
	o, kept, err := orderByRef(ctx, st, c.TenantID, "", in.BusinessRef)
	if err != nil {
		return OrderResult{}, err
	}
	req, err := openWarehouseRequest(o.Ref, kept, in)
	if err != nil {
		return OrderResult{}, err
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return OrderResult{}, err
	}
	return reserveOrder(ctx, st, dec, c, o, kept, payload)
}

// OpenSendASN ships the lines of the model. The policy sees the ship-to of the call, so a code
// that is not one of the partner's own is refused; the 856 is made later from what the warehouse
// shipped and from the order, as in the full gateway.
func OpenSendASN(ctx context.Context, st *store.Store, dec Decider, c Caller, args json.RawMessage) (OrderResult, error) {
	var in openWarehouse
	if err := json.Unmarshal(args, &in); err != nil {
		return OrderResult{}, rpcerr.New(rpcerr.SchemaInvalid, "")
	}
	o, kept, err := orderByRef(ctx, st, c.TenantID, in.PartnerID, in.BusinessRef)
	if err != nil {
		return OrderResult{}, err
	}
	req, err := openWarehouseRequest(o.Ref, kept, in)
	if err != nil {
		return OrderResult{}, err
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return OrderResult{}, err
	}
	seen := map[string]any{"ship_to": in.ShipTo}
	for k, v := range req {
		seen[k] = v
	}
	return shipOrder(ctx, st, dec, c, o, kept, payload, seen)
}

// OpenPostVendorInvoice books the invoice of the model. The policy compares its total, currency
// and lines with the order to the supplier, as it does for a received invoice, and the invoice
// always waits for two operators.
func OpenPostVendorInvoice(ctx context.Context, st *store.Store, dec Decider, c Caller, args json.RawMessage) (OrderResult, error) {
	payload, err := invoiceOfArgs(args)
	if err != nil {
		return OrderResult{}, err
	}
	norm := make([]policy.NormLine, 0, len(payload.Lines))
	for _, l := range payload.Lines {
		qty, qok := decimal.ParseScaled(l.Qty, 3)
		price, pok := decimal.ParseScaled(l.UnitPrice, 2)
		if !qok || !pok || !qty.IsInt64() || !price.IsInt64() {
			return OrderResult{}, rpcerr.New(rpcerr.SchemaInvalid, "")
		}
		norm = append(norm, policy.NormLine{SKU: l.SKU, QtyMilli: qty.Int64(), PriceCents: price.Int64()})
	}
	order, err := supplierOrderFor(ctx, st, c.TenantID, payload.PartnerID, payload.OrderRef)
	if err != nil {
		return OrderResult{}, err
	}
	key, businessKey, err := requestKeys(c, PostInvoiceTool, payload.PartnerID, payload.InvoiceNumber, payload.PartnerID, payload.InvoiceNumber)
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
	effect := struct {
		txn.InvoicePayload
		LinesNorm [][]string `json:"lines_norm"`
	}{payload, policy.InvoiceLines(norm)}
	view := map[string]any{
		"supplier_id": payload.PartnerID, "invoice_number": payload.InvoiceNumber, "order_number": payload.OrderRef,
		"lines": payload.Lines, "total": payload.Total, "currency": payload.Currency,
	}
	return reserveEffect(ctx, st, dec, c, effectRequest{
		Tool: PostInvoiceTool, EffectType: effectPostInvoice, Src: store.OrderSource{ID: noDocument, PartnerID: payload.PartnerID}, ST01: "810",
		Effect: effect, AmountCents: amount, ViewArgs: view, SupplierOrder: order,
		Key: key, BusinessKey: businessKey, ReqHash: reqHash, Payload: rawPayload,
	})
}

// OpenSendEmail queues the letter of the model. It names no partner, so the partner is the one
// that has the address among its contacts; for an address nobody has the policy finds no partner
// and refuses. The business key is the partner and the hash of the arguments.
func OpenSendEmail(ctx context.Context, st *store.Store, dec Decider, c Caller, args json.RawMessage) (OrderResult, error) {
	var in struct {
		To      string `json:"to"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return OrderResult{}, rpcerr.New(rpcerr.SchemaInvalid, "")
	}
	partner, err := st.PartnerOfContact(ctx, c.TenantID, in.To)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return OrderResult{}, err
	}
	canon, err := txn.RequestKey(args, "letter")
	if err != nil {
		return OrderResult{}, rpcerr.New(rpcerr.SchemaInvalid, "")
	}
	ref := hex.EncodeToString(canon[:])
	key, businessKey, err := requestKeys(c, SendEmailTool, partner, ref, partner, ref)
	if err != nil {
		return OrderResult{}, err
	}
	effect := map[string]string{"partner_id": partner, "to": in.To, "subject": in.Subject, "body": in.Body}
	raw, err := json.Marshal(effect)
	if err != nil {
		return OrderResult{}, err
	}
	sum := sha256.Sum256(raw)
	view := map[string]any{"partner_id": partner, "to": in.To, "payload_sha256": hex.EncodeToString(sum[:])}
	return reserveEffect(ctx, st, dec, c, effectRequest{
		Tool: SendEmailTool, EffectType: adapters.EffectEmail, Src: store.OrderSource{ID: noDocument, PartnerID: partner}, ST01: "850",
		Effect: effect, ViewArgs: view,
		Key: key, BusinessKey: businessKey, ReqHash: sum[:], Payload: raw,
	})
}
