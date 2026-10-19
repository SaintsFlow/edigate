package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/SaintsFlow/edigate/internal/adapters"
	"github.com/SaintsFlow/edigate/internal/edi/outbound"
	"github.com/SaintsFlow/edigate/internal/policy"
	"github.com/SaintsFlow/edigate/internal/rpcerr"
	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/txn"
)

// This file holds the tools of the sagas (wave 2-7): erp.confirm_order (the 855 of the order
// saga), wms.reserve and edi.send_asn (the shipment saga), and SendShipNotice, which the
// saga runner calls when the shipment is done and the 856 must be made. All of them
// point at an order by sales_order_txn_id, the txn_id that erp.create_sales_order gave.
// The content of every document comes from the order the gateway built and, for the 856,
// from the facts of the warehouse; the agent passes no price, quantity or address.

// Names under which the keys are made. They are part of the idempotency key (I2).
const (
	ConfirmOrderTool = "erp.confirm_order"
	WMSReserveTool   = "wms.reserve"
	SendASNTool      = "edi.send_asn"
	sendShipNotice   = "edi.send_856"
)

// ConfirmOrderInput is the validated input of erp.confirm_order.
type ConfirmOrderInput struct {
	SalesOrderTxnID string `json:"sales_order_txn_id"`
	AckType         string `json:"ack_type"` // AC accepted, AD accepted with changes, RJ rejected
}

// OrderRefInput is the input of wms.reserve and edi.send_asn.
type OrderRefInput struct {
	SalesOrderTxnID string `json:"sales_order_txn_id"`
}

// order finds the order saga of a txn_id of the tenant and the order it keeps. A txn_id of
// another tenant, of another tool or unknown is not_found. A saga that is no longer running
// (compensating, compensated, incident, done) is policy_denied with saga.order.
func order(ctx context.Context, st *store.Store, tenantID, txnID string) (store.OrderSaga, sagaOrder, error) {
	o, err := st.OrderSagaByTxn(ctx, tenantID, txnID, CreateOrderTool)
	if errors.Is(err, store.ErrNotFound) {
		return store.OrderSaga{}, sagaOrder{}, rpcerr.New(rpcerr.NotFound, "")
	}
	if err != nil {
		return store.OrderSaga{}, sagaOrder{}, err
	}
	var kept sagaOrder
	if err := json.Unmarshal(o.Payload, &kept); err != nil {
		return store.OrderSaga{}, sagaOrder{}, err
	}
	if o.State != "running" && o.State != "done" {
		return store.OrderSaga{}, sagaOrder{}, rpcerr.Denied([]string{"saga.order"})
	}
	return o, kept, nil
}

// orderLines turns the lines of the order into the lines of a document: status IA, or IR
// for all of them when the order is rejected.
func orderLines(kept sagaOrder, rejected bool) []outbound.Line {
	lines := make([]outbound.Line, 0, len(kept.Lines))
	for _, l := range kept.Lines {
		status := "IA"
		if rejected {
			status = "IR"
		}
		lines = append(lines, outbound.Line{SKU: l.SKU, Qty: l.Qty, Unit: l.Unit, UnitPrice: l.UnitPrice, Status: status})
	}
	return lines
}

// checkSpec refuses a document that cannot be written (a value with a separator, a partner
// id the interchange cannot carry) before anything is reserved.
func checkSpec(spec outbound.Spec) error {
	env := outbound.Envelope{Sender: "ZZ:ZZGATEWAY", Receiver: spec.PartnerID, ISA13: "000000001", GS06: "1", ST02: "0001", Now: time.Now()}
	if _, err := outbound.Build(spec, env); err != nil {
		return rpcerr.Denied([]string{"invalid_document"})
	}
	return nil
}

// viewOfSpec is what the operator sees of a document: typed values only. The business
// reference of the partner is not among them.
func viewOfSpec(spec outbound.Spec, amount, forecast int64) map[string]any {
	return map[string]any{
		"partner_id": spec.PartnerID, "set": spec.ST01, "ack_type": spec.AckType, "lines": spec.Lines,
		"amount_cents": amount, "order_amount_cents": forecast, "currency": spec.Currency,
	}
}

// ConfirmOrder sends the 855 of an order (T2, irreversible): step 2 of the order saga. The
// 855 is built from the order; ack_type AC and AD accept every line, RJ rejects every line.
// The policy compares the amount of the document with the amount of the order (forecast.deviation):
// a rejection moves it by 100 percent and waits for an operator. The row waits behind step 1,
// so the 855 never leaves before the order exists in the ERP.
func ConfirmOrder(ctx context.Context, st *store.Store, dec Decider, c Caller, in ConfirmOrderInput) (OrderResult, error) {
	o, kept, err := order(ctx, st, c.TenantID, in.SalesOrderTxnID)
	if err != nil {
		return OrderResult{}, err
	}
	spec := outbound.Spec{
		ST01: "855", PartnerID: o.PartnerID, Ref: o.Ref, OrderDate: kept.OrderDate, Currency: kept.Currency,
		AckType: in.AckType, ShipTo: kept.ShipTo, Lines: orderLines(kept, in.AckType == "RJ"),
	}
	return sendDocument(ctx, st, dec, c, o, kept, spec, effectKeys{
		tool: ConfirmOrderTool, policyTool: ConfirmOrderTool, effect: adapters.EffectSend855, kind: "order", step: 2,
	})
}

// WMSReserve reserves the lines of the order in the warehouse (T1): step 1 of the shipment
// saga, which this call starts.
func WMSReserve(ctx context.Context, st *store.Store, dec Decider, c Caller, in OrderRefInput) (OrderResult, error) {
	o, kept, err := order(ctx, st, c.TenantID, in.SalesOrderTxnID)
	if err != nil {
		return OrderResult{}, err
	}
	payload, err := json.Marshal(warehouseRequest(o, kept))
	if err != nil {
		return OrderResult{}, err
	}
	bk, reqHash, err := keysOf(o, payload)
	if err != nil {
		return OrderResult{}, err
	}
	amount, _ := policy.Cents(kept.Total)
	view := map[string]any{"partner_id": o.PartnerID, "lines": kept.Lines, "total": kept.Total, "currency": kept.Currency}
	return reserveEffect(ctx, st, dec, c, effectRequest{
		Tool: WMSReserveTool, EffectType: adapters.EffectWMSReserve, Src: sourceOf(o), ST01: "850",
		Effect: json.RawMessage(payload), AmountCents: amount, ViewArgs: view,
		Key: txn.Key(c.TenantID, WMSReserveTool, o.PartnerID, o.Ref), BusinessKey: bk, ReqHash: reqHash, Payload: payload,
		Saga: &store.SagaLink{
			Kind: "shipment", StepNo: 1, Steps: []string{adapters.EffectWMSReserve, adapters.EffectWMSShip, adapters.EffectSend856},
			Payload: o.Payload, SourceDocumentID: o.SourceDocumentID,
		},
	})
}

// SendASN ships the order and notifies the partner (T2, irreversible): steps 2 and 3 of the
// shipment saga. This call queues the shipment (wms.ship); the 856 is made from the facts of
// the warehouse when the shipment is done (SendShipNotice) and goes through the policy then.
// Without a done reservation the call is policy_denied with saga.order.
func SendASN(ctx context.Context, st *store.Store, dec Decider, c Caller, in OrderRefInput) (OrderResult, error) {
	o, kept, err := order(ctx, st, c.TenantID, in.SalesOrderTxnID)
	if err != nil {
		return OrderResult{}, err
	}
	payload, err := json.Marshal(warehouseRequest(o, kept))
	if err != nil {
		return OrderResult{}, err
	}
	bk, reqHash, err := keysOf(o, payload)
	if err != nil {
		return OrderResult{}, err
	}
	progress, err := st.ShipmentSagaOf(ctx, c.TenantID, bk)
	if err != nil {
		return OrderResult{}, err
	}
	if !progress.Exists || progress.ReserveState != "completed" || progress.State != "running" {
		return OrderResult{}, rpcerr.Denied([]string{"saga.order"})
	}
	amount, _ := policy.Cents(kept.Total)
	forecast := amount
	view := map[string]any{"partner_id": o.PartnerID, "lines": kept.Lines, "total": kept.Total, "currency": kept.Currency}
	return reserveEffect(ctx, st, dec, c, effectRequest{
		Tool: SendASNTool, EffectType: adapters.EffectWMSShip, Src: sourceOf(o), ST01: "850",
		Effect: json.RawMessage(payload), AmountCents: amount, ForecastCents: &forecast, ViewArgs: view,
		Key: txn.Key(c.TenantID, SendASNTool, o.PartnerID, o.Ref), BusinessKey: bk, ReqHash: reqHash, Payload: payload,
		Saga: &store.SagaLink{Kind: "shipment", StepNo: 2},
	})
}

// SendShipNotice makes the 856 of a shipment that is done and queues it: step 3 of the
// shipment saga. facts is what the warehouse recorded (what left and the SSCC of every
// line). The quantity of the document is the shipped one, so a shipment that differs from
// the order by more than the allowed share waits for an operator. A refusal of the policy is
// returned as policy_denied; the saga runner then compensates.
func SendShipNotice(ctx context.Context, st *store.Store, dec Decider, sg store.Saga, facts adapters.Shipment) (OrderResult, error) {
	var kept sagaOrder
	if err := json.Unmarshal(sg.Payload, &kept); err != nil {
		return OrderResult{}, err
	}
	var partner, ref string
	var bk []string
	if err := json.Unmarshal(sg.BusinessKey, &bk); err != nil || len(bk) != 2 {
		return OrderResult{}, errors.New("the business key of the saga is not [partner, reference]")
	}
	partner, ref = bk[0], bk[1]
	prices := map[string]txn.OrderLine{}
	for _, l := range kept.Lines {
		prices[l.SKU] = l
	}
	if len(facts.Lines) != len(facts.SSCC) {
		return OrderResult{}, errors.New("the warehouse gave a different number of SSCC and lines")
	}
	spec := outbound.Spec{
		ST01: "856", PartnerID: partner, Ref: ref, OrderDate: kept.OrderDate, Currency: kept.Currency,
		ShipTo: kept.ShipTo, ShipmentID: "SHP" + strings.ToUpper(facts.ExternalID[:min(12, len(facts.ExternalID))]),
	}
	for i, l := range facts.Lines {
		priced, ok := prices[l.SKU]
		if !ok {
			return OrderResult{}, errors.New("the warehouse shipped a product that is not in the order")
		}
		spec.Lines = append(spec.Lines, outbound.Line{SKU: l.SKU, Qty: l.Qty, Unit: l.Unit, UnitPrice: priced.UnitPrice, SSCC: facts.SSCC[i]})
	}
	c := Caller{TenantID: sg.TenantID, PrincipalID: "saga", Kind: "saga", TraceID: "saga"}
	o := store.OrderSaga{PartnerID: partner, Ref: ref, SourceDocumentID: sg.SourceDocumentID, Payload: sg.Payload}
	return sendDocument(ctx, st, dec, c, o, kept, spec, effectKeys{
		tool: sendShipNotice, policyTool: SendASNTool, effect: adapters.EffectSend856, kind: "shipment", step: 3,
	})
}

// effectKeys names the pieces of a document that is sent as a saga step.
type effectKeys struct {
	tool, policyTool, effect, kind string
	step                           int
}

// sendDocument checks, prices and reserves a document for the partner as a step of a saga.
// The forecast of the policy is the amount of the order; the amount of the effect is the
// amount of the document.
func sendDocument(ctx context.Context, st *store.Store, dec Decider, c Caller, o store.OrderSaga, kept sagaOrder, spec outbound.Spec, k effectKeys) (OrderResult, error) {
	if err := checkSpec(spec); err != nil {
		return OrderResult{}, err
	}
	amount, err := spec.AmountCents()
	if err != nil {
		return OrderResult{}, err
	}
	forecast, _ := policy.Cents(kept.Total)
	payload, err := json.Marshal(spec)
	if err != nil {
		return OrderResult{}, err
	}
	bk, err := txn.BusinessKeyJSON(o.PartnerID, o.Ref)
	if err != nil {
		return OrderResult{}, err
	}
	reqHash, err := txn.ReqHash(spec)
	if err != nil {
		return OrderResult{}, err
	}
	return reserveEffect(ctx, st, dec, c, effectRequest{
		Tool: k.tool, PolicyTool: k.policyTool, EffectType: k.effect, Src: sourceOf(o), ST01: "850",
		Effect: spec, AmountCents: amount, ForecastCents: &forecast, ViewArgs: viewOfSpec(spec, amount, forecast),
		Key: txn.Key(c.TenantID, k.tool, o.PartnerID, o.Ref), BusinessKey: bk, ReqHash: reqHash, Payload: payload,
		Saga: &store.SagaLink{Kind: k.kind, StepNo: k.step},
	})
}

// warehouseRequest is the effect of wms.reserve and wms.ship: the order and its lines.
func warehouseRequest(o store.OrderSaga, kept sagaOrder) map[string]any {
	lines := make([]map[string]string, 0, len(kept.Lines))
	for _, l := range kept.Lines {
		lines = append(lines, map[string]string{"sku": l.SKU, "qty": l.Qty, "unit": l.Unit})
	}
	return map[string]any{"order_ref": o.Ref, "lines": lines}
}

func keysOf(o store.OrderSaga, payload []byte) (businessKey, reqHash []byte, err error) {
	if businessKey, err = txn.BusinessKeyJSON(o.PartnerID, o.Ref); err != nil {
		return nil, nil, err
	}
	reqHash, err = txn.ReqHash(json.RawMessage(payload))
	return businessKey, reqHash, err
}

// sourceOf is the 850 an order came from, as the reservation of a key wants it.
func sourceOf(o store.OrderSaga) store.OrderSource {
	return store.OrderSource{ID: o.SourceDocumentID, PartnerID: o.PartnerID}
}
