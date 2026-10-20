package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SaintsFlow/edigate/internal/adapters"
	"github.com/SaintsFlow/edigate/internal/edi/outbound"
)

// This file holds what happens to a document sent to a partner after its outbox row is
// written: the transmissions the relay makes (Claim methods) and the 997 that comes back
// (invariant I6). The state of a document (outbound_docs.state) is queued, sent (an MDN was
// counted), delivered (MDN and a 997 with AK5 A or E) or incident (a person decides).

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// DocIncident reports why the document of the claimed row is already an incident ("" when
// it is not). A 997 with AK5 = R can arrive while a repeat of the transmission waits.
func (c *Claim) DocIncident(ctx context.Context, docID string) (string, error) {
	var state string
	var reason *string
	err := c.tx.QueryRow(ctx, `SELECT state, incident_reason FROM outbound_docs WHERE id = $1::uuid`, docID).Scan(&state, &reason)
	if err != nil {
		return "", err
	}
	if state != "incident" {
		return "", nil
	}
	if reason == nil {
		return "unknown", nil
	}
	return *reason, nil
}

// RecordTransmission counts one transmission of the document. When its MDN counted (ok),
// the MDN is stored and the document becomes sent, or delivered if the 997 came first. An MDN
// for a document that is already an incident changes nothing but the audit log.
func (c *Claim) RecordTransmission(ctx context.Context, docID string, ok bool, mdn []byte) error {
	var state, ack, st01 string
	var key []byte
	err := c.tx.QueryRow(ctx,
		`SELECT state, COALESCE(ack_status, ''), idem_key, st01 FROM outbound_docs WHERE id = $1::uuid FOR UPDATE`, docID).
		Scan(&state, &ack, &key, &st01)
	if err != nil {
		return err
	}
	if !ok {
		_, err = c.tx.Exec(ctx, `UPDATE outbound_docs SET transmissions = transmissions + 1 WHERE id = $1::uuid`, docID)
		return err
	}
	next, event := state, "mdn_counted"
	switch {
	case state == "incident":
		event = "mdn_after_incident"
	case st01 == "997":
		next = "delivered" // nobody acknowledges a 997: the MDN is all there is
	case ack == "A" || ack == "E":
		next, event = "delivered", ackEvent(ack)
	default:
		next = "sent"
	}
	if _, err := c.tx.Exec(ctx,
		`UPDATE outbound_docs SET transmissions = transmissions + 1, mdn_at = COALESCE(mdn_at, now()), mdn_ok = true, state = $2
		 WHERE id = $1::uuid`, docID, next); err != nil {
		return err
	}
	if _, err := c.tx.Exec(ctx,
		`UPDATE edi_interchanges i SET mdn = $2::jsonb
		 FROM outbound_docs d
		 WHERE d.id = $1::uuid AND i.tenant_id = d.tenant_id AND i.direction = 'out'
		   AND i.counterparty_id = d.partner_id AND i.isa13 = d.isa13`, docID, mdn); err != nil {
		return err
	}
	return appendAudit(ctx, c.tx, AuditEvent{
		TenantID: c.Row.TenantID, EventType: event, TraceID: "relay", PrincipalID: "relay", IdemKey: key, Outcome: next,
	})
}

// FailSend ends the claimed row of a document for good: the document becomes an incident
// with the reason, the row is dead and the key failed. No new transmission follows.
func (c *Claim) FailSend(ctx context.Context, docID, reason, lastError string) error {
	if _, err := c.tx.Exec(ctx,
		`UPDATE outbound_docs SET state = 'incident', incident_reason = COALESCE(incident_reason, $2) WHERE id = $1::uuid`,
		docID, reason); err != nil {
		return err
	}
	return c.Fail(ctx, "dead", reason, lastError)
}

func ackEvent(code string) string {
	if code == "E" {
		return "ack_accepted_with_errors"
	}
	return "ack_delivered"
}

// queueReplies puts the 997 for each group of the interchange into the outbox, in the
// transaction that stores the interchange (invariant I7: the numbers, the bytes and the
// interchange are fixed together). A 997 whose content cannot be written is not sent; the
// audit log says so and the interchange is stored all the same.
func queueReplies(ctx context.Context, tx pgx.Tx, in InboundInterchange) error {
	for _, a := range in.Replies {
		probe := outbound.Envelope{Sender: in.GatewayID, Receiver: in.PartnerID, ISA13: "000000001", GS06: "1", ST02: "0001", Now: time.Now()}
		if _, err := outbound.Build997(a, probe); err != nil {
			detail, jerr := json.Marshal(map[string]string{"gs06": a.GroupControl})
			if jerr != nil {
				return jerr
			}
			if err := appendAudit(ctx, tx, AuditEvent{TenantID: in.TenantID, EventType: "ack_not_built", TraceID: "inbound",
				PrincipalID: "gateway", Outcome: "skipped", Detail: detail}); err != nil {
				return err
			}
			continue
		}
		err := queueDocument(ctx, tx, in.TenantID, in.GatewayID, in.PartnerID, nil, adapters.EffectSend997, "997",
			func(env outbound.Envelope) ([]byte, error) { return outbound.Build997(a, env) })
		if err != nil {
			return err
		}
	}
	return nil
}

// InboundAck is a 997 to match: the group it answers (AK102) and the verdict on each of its sets.
type InboundAck struct {
	GS06 string
	Sets []InboundAckSet
}

// InboundAckSet is the verdict on one set: ST02 (AK202) and AK501.
type InboundAckSet struct {
	ST02 string
	Code string
}

// ackOutcome decides what a 997 for one set does to the document (I6): setAck tells whether
// the verdict is recorded as the first one, next is the new state, reason the incident reason
// to record, event the audit event. The first verdict decides; the same verdict again is a
// duplicate; a rejection after an acceptance is a conflict and makes an incident.
func ackOutcome(state, prev string, mdnOK bool, code string) (setAck bool, next, reason, event string) {
	accepted := func(c string) bool { return c == "A" || c == "E" }
	next = state
	if prev == "" {
		setAck = true
		switch {
		case state == "incident":
			event = "ack_after_incident"
		case !accepted(code):
			next, reason, event = "incident", "ack_rejected", "ack_rejected"
		case mdnOK:
			next, event = "delivered", ackEvent(code)
		default:
			event = "ack_before_mdn"
		}
		return
	}
	if accepted(prev) == accepted(code) {
		return false, state, "", "ack_duplicate"
	}
	if accepted(prev) {
		next, reason = "incident", "ack_conflict"
	}
	return false, next, reason, "ack_conflict"
}

// matchAcks applies the 997 of a partner to the documents it answers. The document is found
// by (tenant, the partner that sent the 997, GS06, ST02): a 997 of another partner with the
// same numbers finds nothing of ours. It runs in the transaction that stores the 997.
func matchAcks(ctx context.Context, tx pgx.Tx, tenantID, partnerID string, acks []InboundAck) error {
	for _, a := range acks {
		for _, set := range a.Sets {
			detail, err := json.Marshal(map[string]string{"gs06": a.GS06, "st02": set.ST02, "ak5": set.Code})
			if err != nil {
				return err
			}
			var id, state, prev string
			var mdnOK *bool
			var key []byte
			err = tx.QueryRow(ctx,
				`SELECT id::text, state, COALESCE(ack_status, ''), mdn_ok, idem_key FROM outbound_docs
				 WHERE tenant_id = $1 AND partner_id = $2 AND gs06 = $3 AND st02 = $4 FOR UPDATE`,
				tenantID, partnerID, a.GS06, set.ST02).Scan(&id, &state, &prev, &mdnOK, &key)
			ev := AuditEvent{TenantID: tenantID, TraceID: "ack", PrincipalID: partnerID, Outcome: set.Code, Detail: detail}
			if isNoRows(err) {
				ev.EventType = "ack_unmatched"
				if err := appendAudit(ctx, tx, ev); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return fmt.Errorf("find the document of a 997: %w", err)
			}
			setAck, next, reason, event := ackOutcome(state, prev, mdnOK != nil && *mdnOK, set.Code)
			if _, err := tx.Exec(ctx,
				`UPDATE outbound_docs
				    SET ack_status = CASE WHEN $2 THEN $3::char(1) ELSE ack_status END,
				        ack_at = CASE WHEN $2 THEN now() ELSE ack_at END,
				        state = $4, incident_reason = COALESCE(incident_reason, NULLIF($5, ''))
				  WHERE id = $1::uuid`, id, setAck, set.Code, next, reason); err != nil {
				return err
			}
			ev.EventType, ev.IdemKey = event, key
			if err := appendAudit(ctx, tx, ev); err != nil {
				return err
			}
		}
	}
	return nil
}
