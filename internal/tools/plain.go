package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/SaintsFlow/edigate/internal/adapters"
	"github.com/SaintsFlow/edigate/internal/decimal"
	"github.com/SaintsFlow/edigate/internal/edi/outbound"
	"github.com/SaintsFlow/edigate/internal/rpcerr"
	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/txn"
)

// This file holds the open tools of the modes plain (C0, and C2 behind an agentgateway) and
// reqhash (C1). They have the names of the tools of the gateway, but none of its protections:
// the arguments are written by the model (partner, reference, prices, quantities), the call
// goes to the other system at once and without an external id, a document for a partner is
// built from the arguments and posted, and the document counts as sent when the partner
// answers (no MDN check, no 997). There is no taint label, no policy, no key, no outbox and
// no saga. The mode reqhash adds one thing: a key of the tool and the canonical arguments
// with the stored answer, so that a call repeated with the same arguments gets the stored
// answer and the other system is not called.

// PlainEnv holds the clients of the other systems that the open tools call directly. A
// client that is nil was not configured: its tools answer with an internal error.
type PlainEnv struct {
	Onec adapters.Adapter
	WMS  *adapters.WMS
	Mail adapters.Adapter
	AS2  *adapters.AS2
	// ReqKeys switches on the stored answers of the mode reqhash.
	ReqKeys bool
}

// PlainResult is the answer of an open tool with an effect. The effect was done in the
// other system (completed) or refused there for good (rejected).
type PlainResult struct {
	Status      string  `json:"status"`
	TxnID       string  `json:"txn_id"`
	ExternalRef *string `json:"external_ref,omitempty"`
	Reason      string  `json:"reason,omitempty"`
}

// run does one effect for an open tool: the stored answer of the same request (reqhash only),
// else the effect, then the row of the call. do returns the reference of the other system. A
// permanent refusal (HTTP 4xx other than 408 and 429) is an answer, any other failure an error.
func (e PlainEnv) run(ctx context.Context, st *store.Store, c Caller, tool string, args json.RawMessage, do func() (string, error)) (PlainResult, error) {
	var key []byte
	if e.ReqKeys {
		k, err := txn.RequestKey(args, tool)
		if err != nil {
			return PlainResult{}, rpcerr.New(rpcerr.SchemaInvalid, "")
		}
		key = k[:]
		stored, found, err := st.PlainByKey(ctx, c.TenantID, key)
		if err != nil {
			return PlainResult{}, err
		}
		if found {
			return decodePlain(stored.Result)
		}
	}
	res := PlainResult{Status: "completed", TxnID: store.NewTxnID()}
	ref, err := do()
	var refused *adapters.StatusError
	switch {
	case err == nil:
		res.ExternalRef = &ref
	case errors.As(err, &refused) && refused.Permanent():
		res.Status, res.Reason = "rejected", "adapter_rejected"
	default:
		return PlainResult{}, err
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return PlainResult{}, err
	}
	saved, stored, err := st.PlainSave(ctx, c.TenantID, store.PlainTxn{TxnID: res.TxnID, Tool: tool, Args: args, Result: raw}, key)
	if err != nil {
		return PlainResult{}, err
	}
	if !saved {
		return decodePlain(stored.Result) // a call with the same key finished first
	}
	return res, nil
}

func decodePlain(raw []byte) (PlainResult, error) {
	var r PlainResult
	return r, json.Unmarshal(raw, &r)
}

// openLine is a line as the model writes it. Quantities and prices may arrive as JSON
// numbers or strings; json.Number keeps their digits as they are.
type openLine struct {
	SKU       string      `json:"sku"`
	Qty       json.Number `json:"qty"`
	Unit      string      `json:"unit"`
	UnitPrice json.Number `json:"unit_price"`
}

// openOrder is the input of erp.create_sales_order in the open modes.
type openOrder struct {
	PartnerID           string     `json:"partner_id"`
	BusinessRef         string     `json:"business_ref"`
	ShipTo              string     `json:"ship_to"`
	Currency            string     `json:"currency"`
	DeliveryInstruction string     `json:"delivery_instruction"`
	Lines               []openLine `json:"lines"`
}

// orderOfArgs reads the arguments of an order and builds the effect for the ERP: quantities
// with three places, prices with two, and the total of the lines. A number that does not fit
// those places is schema_invalid. Nothing is looked up or checked against any record.
func orderOfArgs(args json.RawMessage) (txn.OrderPayload, error) {
	var in openOrder
	if err := json.Unmarshal(args, &in); err != nil {
		return txn.OrderPayload{}, rpcerr.New(rpcerr.SchemaInvalid, "")
	}
	lines, total, err := priceLines(in.Lines)
	if err != nil {
		return txn.OrderPayload{}, err
	}
	return txn.OrderPayload{
		PartnerID: in.PartnerID, BusinessRef: in.BusinessRef, ShipTo: in.ShipTo, Lines: lines, Total: total,
		Currency: in.Currency, DeliveryInstruction: in.DeliveryInstruction,
	}, nil
}

// priceLines puts the lines of the model into the form of the effect and sums them.
func priceLines(in []openLine) ([]txn.OrderLine, string, error) {
	lines := make([]txn.OrderLine, 0, len(in))
	items := make([]decimal.Item, 0, len(in))
	for _, l := range in {
		qty, qok := decimal.ParseScaled(l.Qty.String(), 3)
		price, pok := decimal.ParseScaled(l.UnitPrice.String(), 2)
		if !qok || !pok {
			return nil, "", rpcerr.New(rpcerr.SchemaInvalid, "a quantity has more than 3 places or a price more than 2")
		}
		q, p := decimal.FormatScaled(qty, 3), decimal.FormatScaled(price, 2)
		lines = append(lines, txn.OrderLine{SKU: l.SKU, Qty: q, Unit: l.Unit, UnitPrice: p})
		items = append(items, decimal.Item{Qty: q, Price: p})
	}
	cents, ok := decimal.SumCents(items)
	if !ok {
		return nil, "", rpcerr.New(rpcerr.SchemaInvalid, "")
	}
	return lines, decimal.FormatScaled(big.NewInt(cents), 2), nil
}

// money reads a total or a price with at most two places.
func money(n json.Number) (string, error) {
	v, ok := decimal.ParseScaled(n.String(), 2)
	if !ok {
		return "", rpcerr.New(rpcerr.SchemaInvalid, "an amount has more than 2 places")
	}
	return decimal.FormatScaled(v, 2), nil
}

func (e PlainEnv) onecReady() error {
	if e.Onec == nil {
		return errors.New("the ERP is not configured")
	}
	return nil
}

// PlainCreateSalesOrder posts the order of the model to the ERP.
func (e PlainEnv) PlainCreateSalesOrder(ctx context.Context, st *store.Store, c Caller, args json.RawMessage) (PlainResult, error) {
	if err := e.onecReady(); err != nil {
		return PlainResult{}, err
	}
	payload, err := orderOfArgs(args)
	if err != nil {
		return PlainResult{}, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return PlainResult{}, err
	}
	return e.run(ctx, st, c, CreateOrderTool, args, func() (string, error) {
		return e.Onec.Apply(ctx, adapters.Effect{Type: effectCreateOrder, TenantID: c.TenantID, Payload: raw})
	})
}

// openInvoice is the input of erp.post_vendor_invoice in the open modes.
type openInvoice struct {
	PartnerID     string      `json:"partner_id"`
	InvoiceNumber string      `json:"invoice_number"`
	OrderRef      string      `json:"order_ref"`
	Currency      string      `json:"currency"`
	Total         json.Number `json:"total"`
	Lines         []openLine  `json:"lines"`
}

// invoiceOfArgs builds the effect of a supplier invoice from the arguments of the model. The
// total is the one the model wrote; it is not compared with the lines.
func invoiceOfArgs(args json.RawMessage) (txn.InvoicePayload, error) {
	var in openInvoice
	if err := json.Unmarshal(args, &in); err != nil {
		return txn.InvoicePayload{}, rpcerr.New(rpcerr.SchemaInvalid, "")
	}
	lines, _, err := priceLines(in.Lines)
	if err != nil {
		return txn.InvoicePayload{}, err
	}
	total, err := money(in.Total)
	if err != nil {
		return txn.InvoicePayload{}, err
	}
	out := txn.InvoicePayload{
		PartnerID: in.PartnerID, InvoiceNumber: in.InvoiceNumber, OrderRef: in.OrderRef, Total: total, Currency: in.Currency,
		Lines: make([]txn.InvoiceLine, 0, len(lines)),
	}
	for _, l := range lines {
		out.Lines = append(out.Lines, txn.InvoiceLine{SKU: l.SKU, Qty: l.Qty, UnitPrice: l.UnitPrice})
	}
	return out, nil
}

// PlainPostVendorInvoice posts the invoice of the model to the ERP.
func (e PlainEnv) PlainPostVendorInvoice(ctx context.Context, st *store.Store, c Caller, args json.RawMessage) (PlainResult, error) {
	if err := e.onecReady(); err != nil {
		return PlainResult{}, err
	}
	payload, err := invoiceOfArgs(args)
	if err != nil {
		return PlainResult{}, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return PlainResult{}, err
	}
	return e.run(ctx, st, c, PostInvoiceTool, args, func() (string, error) {
		return e.Onec.Apply(ctx, adapters.Effect{Type: effectPostInvoice, TenantID: c.TenantID, Payload: raw})
	})
}

// PlainSendEmail sends the letter of the model, with its body, as a free letter. The body is
// the path of the leak of configuration C0: whatever the model writes goes out.
func (e PlainEnv) PlainSendEmail(ctx context.Context, st *store.Store, c Caller, args json.RawMessage) (PlainResult, error) {
	if e.Mail == nil {
		return PlainResult{}, errors.New("the mail service is not configured")
	}
	var in struct {
		To      string `json:"to"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return PlainResult{}, rpcerr.New(rpcerr.SchemaInvalid, "")
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return PlainResult{}, err
	}
	// The mail service takes a letter once per external id; this one is new every time.
	return e.run(ctx, st, c, SendEmailTool, args, func() (string, error) {
		return e.Mail.Apply(ctx, adapters.Effect{Type: adapters.EffectEmail, TenantID: c.TenantID, ExternalID: store.NewTxnID(), Payload: raw})
	})
}

// openWarehouse is the input of wms.reserve and, with a partner and a ship-to, of edi.send_asn.
type openWarehouse struct {
	PartnerID   string `json:"partner_id"`
	BusinessRef string `json:"business_ref"`
	ShipTo      string `json:"ship_to"`
	Lines       []struct {
		SKU string      `json:"sku"`
		Qty json.Number `json:"qty"`
	} `json:"lines"`
}

// warehouseOfArgs builds the request to the warehouse. The unit of a line comes from the price
// list when the SKU is there, else it is EA: the model does not write one.
func warehouseOfArgs(ctx context.Context, st *store.Store, tenantID string, in openWarehouse) (map[string]any, error) {
	skus := make([]string, 0, len(in.Lines))
	for _, l := range in.Lines {
		skus = append(skus, l.SKU)
	}
	prices, err := st.Prices(ctx, tenantID, skus)
	if err != nil {
		return nil, err
	}
	lines := make([]map[string]string, 0, len(in.Lines))
	for _, l := range in.Lines {
		qty, ok := decimal.ParseScaled(l.Qty.String(), 3)
		if !ok {
			return nil, rpcerr.New(rpcerr.SchemaInvalid, "a quantity has more than 3 places")
		}
		unit := "EA"
		if p, found := prices[l.SKU]; found {
			unit = p.Unit
		}
		lines = append(lines, map[string]string{"sku": l.SKU, "qty": decimal.FormatScaled(qty, 3), "unit": unit})
	}
	return map[string]any{"order_ref": in.BusinessRef, "lines": lines}, nil
}

// PlainWMSReserve reserves the lines of the model in the warehouse.
func (e PlainEnv) PlainWMSReserve(ctx context.Context, st *store.Store, c Caller, args json.RawMessage) (PlainResult, error) {
	if e.WMS == nil {
		return PlainResult{}, errors.New("the warehouse is not configured")
	}
	var in openWarehouse
	if err := json.Unmarshal(args, &in); err != nil {
		return PlainResult{}, rpcerr.New(rpcerr.SchemaInvalid, "")
	}
	req, err := warehouseOfArgs(ctx, st, c.TenantID, in)
	if err != nil {
		return PlainResult{}, err
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return PlainResult{}, err
	}
	return e.run(ctx, st, c, WMSReserveTool, args, func() (string, error) {
		return e.WMS.Apply(ctx, adapters.Effect{Type: adapters.EffectWMSReserve, TenantID: c.TenantID, Payload: raw})
	})
}

// openOrderSpec finds what a document needs from the order that an earlier erp.create_sales_order
// of the open tools made for the partner and the reference: the currency and the price of each
// SKU. Without such an order the currency is USD and the prices are 0.00.
func openOrderSpec(ctx context.Context, st *store.Store, tenantID, partnerID, ref string) (currency string, prices map[string]string, order *openOrder, err error) {
	currency, prices = "USD", map[string]string{}
	raw, err := st.PlainLast(ctx, tenantID, CreateOrderTool, partnerID, ref)
	if errors.Is(err, store.ErrNotFound) {
		return currency, prices, nil, nil
	}
	if err != nil {
		return "", nil, nil, err
	}
	var o openOrder
	if err := json.Unmarshal(raw, &o); err != nil {
		return "", nil, nil, err
	}
	for _, l := range o.Lines {
		if p, err := money(l.UnitPrice); err == nil {
			prices[l.SKU] = p
		}
	}
	return o.Currency, prices, &o, nil
}

// PlainConfirmOrder builds the 855 from the arguments and posts it to the partner. Without
// lines in the arguments the lines of the order the open tools made are used; without such an
// order the call is not_found. ack_type RJ rejects every line, the other kinds accept them.
func (e PlainEnv) PlainConfirmOrder(ctx context.Context, st *store.Store, c Caller, args json.RawMessage) (PlainResult, error) {
	if e.AS2 == nil {
		return PlainResult{}, errors.New("the partner is not configured")
	}
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
		return PlainResult{}, rpcerr.New(rpcerr.SchemaInvalid, "")
	}
	currency, prices, order, err := openOrderSpec(ctx, st, c.TenantID, in.PartnerID, in.BusinessRef)
	if err != nil {
		return PlainResult{}, err
	}
	spec := outbound.Spec{
		ST01: "855", PartnerID: in.PartnerID, Ref: in.BusinessRef, OrderDate: today(), Currency: currency, AckType: in.AckType,
	}
	status := "IA"
	if in.AckType == "RJ" {
		status = "IR"
	}
	if order != nil {
		spec.ShipTo = order.ShipTo
	}
	switch {
	case len(in.Lines) > 0:
		for _, l := range in.Lines {
			qty, ok := decimal.ParseScaled(l.Qty.String(), 3)
			if !ok {
				return PlainResult{}, rpcerr.New(rpcerr.SchemaInvalid, "a quantity has more than 3 places")
			}
			spec.Lines = append(spec.Lines, outbound.Line{SKU: l.SKU, Qty: decimal.FormatScaled(qty, 3), Unit: l.Unit, UnitPrice: priceOr(prices, l.SKU), Status: status})
		}
	case order != nil:
		for _, l := range order.Lines {
			lines, _, err := priceLines([]openLine{l})
			if err != nil {
				return PlainResult{}, err
			}
			spec.Lines = append(spec.Lines, outbound.Line{SKU: l.SKU, Qty: lines[0].Qty, Unit: l.Unit, UnitPrice: lines[0].UnitPrice, Status: status})
		}
	default:
		return PlainResult{}, rpcerr.New(rpcerr.NotFound, "")
	}
	return e.run(ctx, st, c, ConfirmOrderTool, args, func() (string, error) {
		return e.transmit(ctx, st, c.TenantID, spec)
	})
}

// PlainSendASN ships the lines of the model in the warehouse and posts the 856 with the SSCC
// the warehouse gave. The shipped quantity is the one the warehouse recorded.
func (e PlainEnv) PlainSendASN(ctx context.Context, st *store.Store, c Caller, args json.RawMessage) (PlainResult, error) {
	if e.WMS == nil || e.AS2 == nil {
		return PlainResult{}, errors.New("the warehouse or the partner is not configured")
	}
	var in openWarehouse
	if err := json.Unmarshal(args, &in); err != nil {
		return PlainResult{}, rpcerr.New(rpcerr.SchemaInvalid, "")
	}
	req, err := warehouseOfArgs(ctx, st, c.TenantID, in)
	if err != nil {
		return PlainResult{}, err
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return PlainResult{}, err
	}
	currency, prices, _, err := openOrderSpec(ctx, st, c.TenantID, in.PartnerID, in.BusinessRef)
	if err != nil {
		return PlainResult{}, err
	}
	return e.run(ctx, st, c, SendASNTool, args, func() (string, error) {
		shipped, err := e.WMS.ShipNow(ctx, c.TenantID, raw)
		if err != nil {
			return "", err
		}
		spec := outbound.Spec{
			ST01: "856", PartnerID: in.PartnerID, Ref: in.BusinessRef, OrderDate: today(), Currency: currency, ShipTo: in.ShipTo,
			ShipmentID: "SHP" + strings.ToUpper(shipped.SSCC[0][:min(12, len(shipped.SSCC[0]))]),
		}
		for i, l := range shipped.Lines {
			spec.Lines = append(spec.Lines, outbound.Line{SKU: l.SKU, Qty: l.Qty, Unit: l.Unit, UnitPrice: priceOr(prices, l.SKU), SSCC: shipped.SSCC[i]})
		}
		return e.transmit(ctx, st, c.TenantID, spec)
	})
}

func today() string { return time.Now().UTC().Format("20060102") }

func priceOr(prices map[string]string, sku string) string {
	if p, ok := prices[sku]; ok {
		return p
	}
	return "0.00"
}

// transmit builds the interchange of a document with the next control numbers of the
// partner (the counters the sagas use) and posts it. The document counts as sent when the
// partner answers; the receipt is not compared with what was sent. The reference is the ISA13.
func (e PlainEnv) transmit(ctx context.Context, st *store.Store, tenantID string, spec outbound.Spec) (string, error) {
	n, err := st.NextControlNumbers(ctx, tenantID, spec.PartnerID)
	if err != nil {
		return "", err
	}
	env := outbound.Envelope{
		Sender: n.Gateway, Receiver: spec.PartnerID, Now: time.Now(),
		ISA13: fmt.Sprintf("%09d", n.ISA), GS06: strconv.FormatInt(n.GS, 10), ST02: fmt.Sprintf("%04d", n.ST),
	}
	raw, err := outbound.Build(spec, env)
	if err != nil {
		return "", rpcerr.New(rpcerr.SchemaInvalid, "the document cannot be written")
	}
	t := adapters.Transmission{
		DocID: store.NewTxnID(), MessageID: "<" + store.NewTxnID() + "@edigate>", From: n.Gateway, To: spec.PartnerID,
		ISA13: env.ISA13, Body: string(raw),
	}
	if _, err := e.AS2.Transmit(ctx, t); err != nil {
		return "", err
	}
	return env.ISA13, nil
}

// PlainTxnStatus answers txn.status from the row of the open call: completed or rejected, with
// the reference of the other system. There is no saga, so saga_state is null.
func PlainTxnStatus(ctx context.Context, st *store.Store, tenantID string, in TxnStatusInput) (TxnStatusOutput, error) {
	row, err := st.PlainState(ctx, tenantID, in.TxnID)
	if errors.Is(err, store.ErrNotFound) {
		return TxnStatusOutput{}, rpcerr.New(rpcerr.NotFound, "")
	}
	if err != nil {
		return TxnStatusOutput{}, err
	}
	res, err := decodePlain(row.Result)
	if err != nil {
		return TxnStatusOutput{}, err
	}
	return TxnStatusOutput{TxnID: row.TxnID, Status: res.Status, ExternalRef: res.ExternalRef}, nil
}
