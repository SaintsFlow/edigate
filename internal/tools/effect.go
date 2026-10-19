package tools

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"

	"github.com/SaintsFlow/edigate/internal/approval"
	"github.com/SaintsFlow/edigate/internal/policy"
	"github.com/SaintsFlow/edigate/internal/rpcerr"
	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/txn"
)

// This file is the one path every tool with an effect takes after it has built the
// payload: ask the policy, reserve the key, and answer.

// Decider is the policy as the tools use it (policy.Engine; tests put a recorder
// in front of it).
type Decider interface {
	Decide(ctx context.Context, p policy.Principal, c policy.ToolCall) (policy.Decision, error)
}

// Caller is the identity a tool call runs under, taken from the edge, and the switches of the
// ablation modes of the benchmark (the zero value is the full gateway).
type Caller struct {
	TenantID    string
	PrincipalID string
	Kind        string
	TraceID     string

	// Args are the raw arguments of the call. ArgsKey (mode no-bizkey, C3-A3) makes the
	// idempotency key of the call from them instead of from the business key. RawValues (mode
	// no-sanitize, C3-A1) makes the effects take the raw value of an element that failed its format.
	Args      json.RawMessage
	ArgsKey   bool
	RawValues bool
}

// requestKeys returns the idempotency key of a call and the business key stored next to it. The
// key is made of the tool, the partner and the reference (keyRef), which are the business key
// (I2); bk are the parts of the business key that are stored. In the mode no-bizkey the key is
// made of the tenant, the tool and the canonical arguments, and the key itself is added to the
// stored business key as a third part, so that two orders with one reference are two sagas.
func requestKeys(c Caller, tool, partnerID, keyRef string, bk ...string) (key [32]byte, businessKey []byte, err error) {
	if !c.ArgsKey || len(c.Args) == 0 {
		businessKey, err = json.Marshal(bk)
		return txn.Key(c.TenantID, tool, partnerID, keyRef), businessKey, err
	}
	if key, err = txn.RequestKey(c.Args, c.TenantID, tool); err != nil {
		return key, nil, rpcerr.New(rpcerr.SchemaInvalid, "")
	}
	businessKey, err = json.Marshal(append(slices.Clone(bk), hex.EncodeToString(key[:])))
	return key, businessKey, err
}

// stepKeys is requestKeys for a step of a saga that already exists (confirm, reserve, ship, the
// documents of the sagas): the business key is the one of the saga, third part included.
func stepKeys(c Caller, tool string, o store.OrderSaga) ([32]byte, []byte, error) {
	if c.ArgsKey && len(c.Args) > 0 {
		key, err := txn.RequestKey(c.Args, c.TenantID, tool)
		if err != nil {
			return key, nil, rpcerr.New(rpcerr.SchemaInvalid, "")
		}
		return key, o.BusinessKey, nil
	}
	ref := o.Ref
	var parts []string
	if json.Unmarshal(o.BusinessKey, &parts) == nil && len(parts) > 2 {
		ref += "\x00" + parts[2]
	}
	return txn.Key(c.TenantID, tool, o.PartnerID, ref), o.BusinessKey, nil
}

// countsDaily is implemented by a Decider that does not use the daily counter of the
// partner (policy.AllowAll); every other decider uses it.
type countsDaily interface{ Counts() bool }

// effectRequest is a tool call that is ready to be decided and reserved.
type effectRequest struct {
	Tool       string
	EffectType string
	Src        store.OrderSource
	ST01       string

	Effect      any   // the payload of the effect; the policy sees it as input.effect
	AmountCents int64 // its total
	Counted     bool  // the amount counts against daily_auto_max (T1)
	ViewArgs    any   // typed arguments for the operator

	SupplierOrder *policy.SupplierOrder

	// PolicyTool is the name the policy knows the tool by, when the key is made under
	// another name ("" means Tool). ForecastCents replaces the forecast of the partner for
	// this call: the sagas compare a document with the order they belong to.
	PolicyTool    string
	ForecastCents *int64
	Saga          *store.SagaLink

	Key         [32]byte
	BusinessKey []byte
	ReqHash     []byte
	Payload     []byte // the Effect as JSON
}

// reserveEffect asks the policy (inside the transaction of the reservation, so that
// the daily counter of the partner is locked while it decides), then reserves the key.
//
// The answer is the status of the key: accepted (the effect is queued) or
// pending_approval (an operator must decide first). The policy forbidding the call
// gives policy_denied with the rule names, after an audit row with the decision; the
// same business key with another request gives conflicting_duplicate (and a review
// request for the operator). A failure of the policy gives an internal error and no
// effect.
func reserveEffect(ctx context.Context, st *store.Store, dec Decider, c Caller, r effectRequest) (OrderResult, error) {
	call := policy.ToolCall{
		Tool: cmp.Or(r.PolicyTool, r.Tool), Effect: r.Effect, AmountCents: r.AmountCents, SupplierOrder: r.SupplierOrder,
		Document: policy.Document{ST01: r.ST01},
	}
	partner, err := st.PartnerPolicy(ctx, c.TenantID, r.Src.PartnerID)
	known := err == nil
	switch {
	case known:
		call.Partner = &policy.Partner{
			Role: partner.Role, Active: partner.Active, AutoMaxCents: partner.AutoMaxCents,
			DailyMaxCents: partner.DailyMaxCents, ForecastCents: partner.ForecastCents,
			ShipToCodes: partner.ShipToCodes, Contacts: partner.Contacts,
		}
		call.Document.SenderRole = partner.Role
		if r.ForecastCents != nil {
			call.Partner.ForecastCents = r.ForecastCents
		}
	case !errors.Is(err, store.ErrNotFound):
		return OrderResult{}, err
	}
	principal := policy.Principal{ID: c.PrincipalID, Kind: c.Kind, TenantID: c.TenantID}

	reviewView, err := approval.View(r.Tool, 0, []string{"conflicting_duplicate"}, r.ViewArgs)
	if err != nil {
		return OrderResult{}, err
	}
	in := store.ReserveInput{
		TenantID: c.TenantID, Tool: r.Tool, PrincipalID: c.PrincipalID, TraceID: c.TraceID,
		DocumentID: r.Src.ID, Key: r.Key[:], BusinessKey: r.BusinessKey, ReqHash: r.ReqHash,
		EffectType: r.EffectType, Payload: r.Payload, ReviewView: reviewView, Saga: r.Saga,
	}
	counts := true
	if d, ok := dec.(countsDaily); ok {
		counts = d.Counts()
	}
	if r.Counted && known && counts {
		in.PartnerID, in.AmountCents = r.Src.PartnerID, r.AmountCents
	}
	in.Decide = func(dailyAutoCents int64) (store.Verdict, error) {
		call.DailyAutoCents = dailyAutoCents
		d, err := dec.Decide(ctx, principal, call)
		if err != nil {
			return store.Verdict{}, err
		}
		if !d.Allow && len(d.Deny) == 0 {
			return store.Verdict{}, errors.New("the policy refused without naming a rule")
		}
		explain, err := json.Marshal(d)
		if err != nil {
			return store.Verdict{}, err
		}
		v := store.Verdict{Deny: d.Deny, Approval: d.Approval, Tier: d.Tier, Rules: d.Fired, Explain: explain}
		if d.Approval != policy.ApprovalNone {
			v.View, err = approval.View(r.Tool, d.Tier, d.Fired, r.ViewArgs)
		}
		return v, err
	}

	res, err := st.Reserve(ctx, in)
	if err != nil {
		return OrderResult{}, err
	}
	switch res.Outcome {
	case store.ReservedConflict:
		return OrderResult{}, rpcerr.New(rpcerr.ConflictingDuplicate, "")
	case store.ReservedDenied:
		err := st.AppendAudit(ctx, store.AuditEvent{
			TenantID: c.TenantID, EventType: "policy_denied", TraceID: c.TraceID, PrincipalID: c.PrincipalID,
			Tool: r.Tool, IdemKey: r.Key[:], PayloadSHA256: r.ReqHash, Outcome: string(rpcerr.PolicyDenied),
			PolicyExplain: res.Verdict.Explain,
		})
		if err != nil {
			return OrderResult{}, err
		}
		return OrderResult{}, rpcerr.Denied(res.Verdict.Deny)
	}
	return OrderResult{Status: res.Status, TxnID: res.TxnID, Reason: res.Reason, ApprovalID: res.ApprovalID}, nil
}
