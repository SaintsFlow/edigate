package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SaintsFlow/edigate/internal/adapters"
	"github.com/SaintsFlow/edigate/internal/edi/outbound"
)

// This file holds the parts of Reserve and of the operator's decisions that belong to
// the sagas (wave 2-7): linking the key of a step to its saga, writing the outbox row of
// an effect (a document for a partner gets its control numbers and its bytes here), and
// the lookups the tools of the sagas need.

// SagaLink ties the key of a call to a step of a saga. Steps is set by the call that starts the
// saga (the names of all its steps; Payload and SourceDocumentID describe the order); a call
// that continues a saga leaves it nil and the saga must exist.
type SagaLink struct {
	Kind             string
	StepNo           int
	Steps            []string
	Payload          []byte
	SourceDocumentID string
}

// linkSaga creates the saga with its steps when the call starts it, then writes the key into
// its step. It can be repeated: a repeat changes nothing.
func linkSaga(ctx context.Context, tx pgx.Tx, in ReserveInput) error {
	l := in.Saga
	if l == nil {
		return nil
	}
	if l.Steps != nil {
		if _, err := tx.Exec(ctx,
			`INSERT INTO sagas (tenant_id, kind, business_key, payload, source_document_id)
			 VALUES ($1, $2, $3, $4, $5::uuid) ON CONFLICT (tenant_id, kind, business_key) DO NOTHING`,
			in.TenantID, l.Kind, in.BusinessKey, l.Payload, l.SourceDocumentID); err != nil {
			return fmt.Errorf("create the saga: %w", err)
		}
		for i, name := range l.Steps {
			if _, err := tx.Exec(ctx,
				`INSERT INTO saga_steps (saga_id, step_no, name)
				 SELECT id, $4, $5 FROM sagas WHERE tenant_id = $1 AND kind = $2 AND business_key = $3
				 ON CONFLICT DO NOTHING`, in.TenantID, l.Kind, in.BusinessKey, i+1, name); err != nil {
				return fmt.Errorf("create a step: %w", err)
			}
		}
	}
	tag, err := tx.Exec(ctx,
		`UPDATE saga_steps SET idem_key = $5
		 WHERE saga_id = (SELECT id FROM sagas WHERE tenant_id = $1 AND kind = $2 AND business_key = $3)
		   AND step_no = $4 AND idem_key IS NULL`, in.TenantID, l.Kind, in.BusinessKey, l.StepNo, in.Key)
	if err != nil {
		return fmt.Errorf("link the key to the step: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var linked bool
		err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM saga_steps WHERE idem_key = $1)`, in.Key).Scan(&linked)
		if err != nil || !linked {
			return errors.New("the saga step of the call does not exist")
		}
	}
	return nil
}

// enqueue writes the outbox row of an effect and, for a document to a partner, everything
// that must be fixed together with it (invariant I7): the three control numbers, the bytes of
// the interchange, its Message-Id, the row of edi_interchanges (out) and the row of
// outbound_docs. payload is the effect for an ERP or a warehouse; for a document it is the
// outbound.Spec, and the row gets the transmission built from it.
func (s *Store) enqueue(ctx context.Context, tx pgx.Tx, tenantID string, key []byte, effectType string, payload []byte) error {
	if !adapters.IsSend(effectType) {
		_, err := insertOutboxReturning(ctx, tx, tenantID, key, effectType, payload)
		return err
	}
	var spec outbound.Spec
	if err := json.Unmarshal(payload, &spec); err != nil {
		return fmt.Errorf("read the document spec: %w", err)
	}
	var gateway *string
	if err := tx.QueryRow(ctx, `SELECT interchange_id FROM tenants WHERE id = $1`, tenantID).Scan(&gateway); err != nil {
		return err
	}
	if gateway == nil {
		return errors.New("the tenant has no interchange id: it cannot send documents")
	}
	var docID, messageUUID string
	if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text, gen_random_uuid()::text`).Scan(&docID, &messageUUID); err != nil {
		return err
	}
	isa, err := nextNumber(ctx, tx, tenantID, spec.PartnerID, "isa")
	if err != nil {
		return err
	}
	gs, err := nextNumber(ctx, tx, tenantID, spec.PartnerID, "gs")
	if err != nil {
		return err
	}
	st, err := nextNumber(ctx, tx, tenantID, spec.PartnerID, "st")
	if err != nil {
		return err
	}
	env := outbound.Envelope{
		Sender: *gateway, Receiver: spec.PartnerID, Now: time.Now(),
		ISA13: fmt.Sprintf("%09d", isa), GS06: strconv.FormatInt(gs, 10), ST02: fmt.Sprintf("%04d", st),
	}
	raw, err := outbound.Build(spec, env)
	if err != nil {
		return fmt.Errorf("build the document: %w", err)
	}
	sum := sha256Sum(raw)
	t := adapters.Transmission{
		DocID: docID, MessageID: "<" + messageUUID + "@edigate>", From: *gateway, To: spec.PartnerID, ISA13: env.ISA13, Body: string(raw),
	}
	body, err := json.Marshal(t)
	if err != nil {
		return err
	}
	outboxID, err := insertOutboxReturning(ctx, tx, tenantID, key, effectType, body)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO edi_interchanges (tenant_id, direction, counterparty_id, isa13, message_id, raw, raw_sha256, mic_sha256)
		 VALUES ($1, 'out', $2, $3, $4, $5, $6, $6)`,
		tenantID, spec.PartnerID, env.ISA13, t.MessageID, raw, sum); err != nil {
		return fmt.Errorf("store the outgoing interchange: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO outbound_docs (id, tenant_id, partner_id, saga_id, step_no, outbox_id, idem_key, st01, isa13, gs06, st02, message_id, mic_sha256)
		 SELECT $1::uuid, $2, $3, (SELECT saga_id FROM saga_steps WHERE idem_key = $5),
		        (SELECT step_no FROM saga_steps WHERE idem_key = $5), $4, $5, $6, $7, $8, $9, $10, $11`,
		docID, tenantID, spec.PartnerID, outboxID, key, spec.ST01, env.ISA13, env.GS06, env.ST02, t.MessageID, sum); err != nil {
		return fmt.Errorf("record the outgoing document: %w", err)
	}
	numbers, err := json.Marshal(map[string]string{"isa13": env.ISA13, "gs06": env.GS06, "st02": env.ST02})
	if err != nil {
		return err
	}
	return appendAudit(ctx, tx, AuditEvent{
		TenantID: tenantID, EventType: "outbound_queued", TraceID: "outbox", PrincipalID: "gateway", IdemKey: key,
		PayloadSHA256: sum, Outcome: spec.ST01, ControlNumbers: numbers,
	})
}

// insertOutboxReturning queues an effect and ties the row to its saga step (if the key has one).
func insertOutboxReturning(ctx context.Context, tx pgx.Tx, tenantID string, key []byte, effectType string, payload []byte) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx,
		`INSERT INTO outbox (tenant_id, idem_key, effect_type, payload, status) VALUES ($1, $2, $3, $4, 'pending') RETURNING id`,
		tenantID, key, effectType, payload).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("enqueue the effect: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE saga_steps SET outbox_id = $2 WHERE idem_key = $1`, key, id); err != nil {
		return 0, err
	}
	return id, nil
}

// nextNumber gives out the next control number of a kind for a partner. The row of the
// counter is locked until the transaction ends, so two documents never get the same
// number, and a rollback gives the number back (invariant I7).
func nextNumber(ctx context.Context, tx pgx.Tx, tenantID, partnerID, kind string) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx,
		`INSERT INTO control_numbers (tenant_id, partner_id, kind, last_value) VALUES ($1, $2, $3, 1)
		 ON CONFLICT (tenant_id, partner_id, kind) DO UPDATE SET last_value = control_numbers.last_value + 1
		 RETURNING last_value`, tenantID, partnerID, kind).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("allocate a control number: %w", err)
	}
	return n, nil
}

// OrderSaga is the order saga a sales_order_txn_id leads to.
type OrderSaga struct {
	SagaID           string
	State            string
	PartnerID        string
	Ref              string // the raw business reference (BEG03)
	Payload          []byte // JSON: the order as the gateway built it
	SourceDocumentID string
	OrderKeyState    string // the state of the key of erp.create_sales_order
	BusinessKey      []byte // JSON: [partner, reference], and in the mode no-bizkey a third part that tells orders apart
}

// OrderSagaByRef finds the newest order saga of a business reference of the tenant, for the
// tools that take their values from the model and name the order by its number. An empty
// partnerID matches any partner. Unknown gives ErrNotFound.
func (s *Store) OrderSagaByRef(ctx context.Context, tenantID, partnerID, ref string) (OrderSaga, error) {
	var o OrderSaga
	err := s.pool.QueryRow(ctx,
		`SELECT sg.id::text, sg.state, sg.business_key->>0, sg.business_key->>1, sg.payload, sg.source_document_id::text,
		        COALESCE(k.state, ''), sg.business_key
		 FROM sagas sg
		 LEFT JOIN saga_steps st ON st.saga_id = sg.id AND st.step_no = 1
		 LEFT JOIN idempotency_keys k ON k.key = st.idem_key
		 WHERE sg.tenant_id = $1 AND sg.kind = 'order' AND sg.business_key->>1 = $3 AND ($2 = '' OR sg.business_key->>0 = $2)
		 ORDER BY sg.created_at DESC LIMIT 1`, tenantID, partnerID, ref).
		Scan(&o.SagaID, &o.State, &o.PartnerID, &o.Ref, &o.Payload, &o.SourceDocumentID, &o.OrderKeyState, &o.BusinessKey)
	if isNoRows(err) {
		return OrderSaga{}, ErrNotFound
	}
	return o, err
}

// OrderSagaByTxn finds the order saga of the txn_id that erp.create_sales_order gave. A txn_id of
// another tenant, of another tool and an unknown one all give ErrNotFound.
func (s *Store) OrderSagaByTxn(ctx context.Context, tenantID, txnID, createTool string) (OrderSaga, error) {
	var o OrderSaga
	err := s.pool.QueryRow(ctx,
		`SELECT sg.id::text, sg.state, sg.business_key->>0, sg.business_key->>1, sg.payload, sg.source_document_id::text, k.state,
		        sg.business_key
		 FROM idempotency_keys k
		 JOIN sagas sg ON sg.tenant_id = k.tenant_id AND sg.kind = 'order' AND sg.business_key = k.business_key
		 WHERE k.tenant_id = $1 AND k.txn_id = $2::uuid AND k.tool = $3`, tenantID, txnID, createTool).
		Scan(&o.SagaID, &o.State, &o.PartnerID, &o.Ref, &o.Payload, &o.SourceDocumentID, &o.OrderKeyState, &o.BusinessKey)
	if isNoRows(err) {
		return OrderSaga{}, ErrNotFound
	}
	return o, err
}

// ShipmentProgress tells how far the shipment saga of an order is: whether it exists and
// whether its first step (the reservation) is done.
type ShipmentProgress struct {
	Exists       bool
	State        string
	ReserveState string // the state of the key of wms.reserve; "" when the step was not called
}

// ShipmentSagaOf reads the shipment saga of a business key.
func (s *Store) ShipmentSagaOf(ctx context.Context, tenantID string, businessKey []byte) (ShipmentProgress, error) {
	var p ShipmentProgress
	err := s.pool.QueryRow(ctx,
		`SELECT sg.state, COALESCE(k.state, '')
		 FROM sagas sg
		 LEFT JOIN saga_steps st ON st.saga_id = sg.id AND st.step_no = 1
		 LEFT JOIN idempotency_keys k ON k.key = st.idem_key
		 WHERE sg.tenant_id = $1 AND sg.kind = 'shipment' AND sg.business_key = $2`, tenantID, businessKey).
		Scan(&p.State, &p.ReserveState)
	if isNoRows(err) {
		return ShipmentProgress{}, nil
	}
	p.Exists = err == nil
	return p, err
}
