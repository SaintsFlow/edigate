package store

import (
	"testing"

	"github.com/SaintsFlow/edigate/internal/adapters"
)

func key(b byte) []byte { return []byte{b} }

// steps of an order saga: the order and the 855.
func orderSaga(state string, order, notice SagaStep) Saga {
	order.No, order.Name = 1, adapters.EffectCreateOrder
	notice.No, notice.Name = 2, adapters.EffectSend855
	return Saga{State: state, Steps: []SagaStep{order, notice}}
}

// The decisions of a saga, as a table: what the facts say and what happens next (I5).
func TestDecide(t *testing.T) {
	done := SagaStep{Key: key(1), KeyState: "completed"}
	cases := []struct {
		name      string
		saga      Saga
		state     string
		compensae int // the step whose compensation is queued; 0 for none
		abort     []int
		rule      string
	}{
		{"the first step is not done: wait", orderSaga("running", SagaStep{Key: key(1), KeyState: "reserved"}, SagaStep{}), "", 0, nil, ""},
		{"the 855 sent but not delivered: wait",
			orderSaga("running", done, SagaStep{Key: key(2), KeyState: "completed", DocID: "d", DocState: "sent"}), "", 0, nil, ""},
		{"everything delivered: done",
			orderSaga("running", done, SagaStep{Key: key(2), KeyState: "completed", DocID: "d", DocState: "delivered"}), "done", 0, nil, ""},
		{"the 855 request was rejected: compensate the order",
			orderSaga("running", done, SagaStep{Key: key(2), KeyState: "failed", KeyReason: "approval_rejected"}), "compensating", 1, nil, ""},
		{"the order was refused: the 855 behind it is abandoned and there is nothing to compensate",
			orderSaga("running", SagaStep{Key: key(1), KeyState: "failed", KeyReason: "adapter_rejected"}, SagaStep{Key: key(2), KeyState: "reserved"}),
			"compensated", 0, []int{2}, ""},
		{"the order ran out of attempts: its effect is unknown, compensate anyway",
			orderSaga("running", SagaStep{Key: key(1), KeyState: "failed", KeyReason: "dead"}, SagaStep{}), "compensating", 1, nil, ""},
		{"the compensation is on its way: wait",
			orderSaga("compensating", SagaStep{Key: key(1), KeyState: "completed", CompKey: key(9), CompState: "reserved"}, SagaStep{Key: key(2), KeyState: "failed"}),
			"", 0, nil, ""},
		{"the compensation is done: compensated",
			orderSaga("compensating", SagaStep{Key: key(1), KeyState: "completed", CompKey: key(9), CompState: "completed"}, SagaStep{Key: key(2), KeyState: "failed"}),
			"compensated", 0, nil, ""},
		{"the compensation failed: incident",
			orderSaga("compensating", SagaStep{Key: key(1), KeyState: "completed", CompKey: key(9), CompState: "failed"}, SagaStep{Key: key(2), KeyState: "failed"}),
			"incident", 0, nil, "saga.compensation_failed"},
		{"a document that is an incident: incident",
			orderSaga("running", done, SagaStep{Key: key(2), KeyState: "completed", DocID: "d", DocState: "incident", DocReason: "ack_rejected"}),
			"incident", 0, nil, "ack.rejected"},
		{"a finished saga whose document turned into an incident: incident",
			orderSaga("done", done, SagaStep{Key: key(2), KeyState: "completed", DocID: "d", DocState: "incident", DocReason: "ack_conflict"}),
			"incident", 0, nil, "ack.conflict"},
		{"no 997 in time: incident",
			orderSaga("running", done, SagaStep{Key: key(2), KeyState: "completed", DocID: "d", DocState: "sent", Overdue: true}),
			"incident", 0, nil, "ack.missing"},
		{"a saga that is an incident stays one", orderSaga("incident", done, SagaStep{Key: key(2), KeyState: "failed"}), "", 0, nil, ""},
	}
	for _, c := range cases {
		p := decide(c.saga, AdvanceHints{})
		if p.State != c.state {
			t.Errorf("%s: state %q, want %q", c.name, p.State, c.state)
		}
		if got := 0; p.Compensate != nil {
			got = p.Compensate.No
			if got != c.compensae {
				t.Errorf("%s: compensation of step %d, want %d", c.name, got, c.compensae)
			}
		} else if c.compensae != 0 {
			t.Errorf("%s: no compensation, want one for step %d", c.name, c.compensae)
		}
		if len(p.Abort) != len(c.abort) || (len(c.abort) > 0 && p.Abort[0] != c.abort[0]) {
			t.Errorf("%s: abort %v, want %v", c.name, p.Abort, c.abort)
		}
		if got := ""; p.Incident != nil {
			got = p.Incident.Rule
			if got != c.rule {
				t.Errorf("%s: rule %q, want %q", c.name, got, c.rule)
			}
		} else if c.rule != "" {
			t.Errorf("%s: no review request, want %s", c.name, c.rule)
		}
	}
}

// The shipment saga: a refused shipment releases the reservation, and the step that was never
// called is skipped.
func TestDecideShipment(t *testing.T) {
	sg := Saga{State: "running", Steps: []SagaStep{
		{No: 1, Name: adapters.EffectWMSReserve, Key: key(1), KeyState: "completed"},
		{No: 2, Name: adapters.EffectWMSShip, Key: key(2), KeyState: "failed", KeyReason: "adapter_rejected"},
		{No: 3, Name: adapters.EffectSend856},
	}}
	p := decide(sg, AdvanceHints{})
	if p.State != "compensating" || p.Compensate == nil || p.Compensate.Name != adapters.EffectWMSReserve {
		t.Fatalf("plan: %+v", p)
	}
	sg.State = "compensating"
	sg.Steps[0].CompKey, sg.Steps[0].CompState = key(9), "completed"
	p = decide(sg, AdvanceHints{})
	if p.State != "compensated" || p.Statuses[3] != "skipped" || p.Statuses[1] != "compensated" || p.Statuses[2] != "failed" {
		t.Fatalf("plan: %+v", p)
	}
}

// What a 997 does to a document (I6).
func TestAckOutcome(t *testing.T) {
	cases := []struct {
		name         string
		state, prev  string
		mdn          bool
		code         string
		set          bool
		next, reason string
		event        string
	}{
		{"A after the MDN: delivered", "sent", "", true, "A", true, "delivered", "", "ack_delivered"},
		{"E after the MDN: delivered with remarks", "sent", "", true, "E", true, "delivered", "", "ack_accepted_with_errors"},
		{"R: incident", "sent", "", true, "R", true, "incident", "ack_rejected", "ack_rejected"},
		{"A before the MDN: wait for it", "queued", "", false, "A", true, "queued", "", "ack_before_mdn"},
		{"R before the MDN: incident", "queued", "", false, "R", true, "incident", "ack_rejected", "ack_rejected"},
		{"the same verdict again: ignored", "delivered", "A", true, "A", false, "delivered", "", "ack_duplicate"},
		{"A and then E: the same side, ignored", "delivered", "A", true, "E", false, "delivered", "", "ack_duplicate"},
		{"R after A: conflict, incident", "delivered", "A", true, "R", false, "incident", "ack_conflict", "ack_conflict"},
		{"A after R: conflict, stays an incident", "incident", "R", true, "A", false, "incident", "", "ack_conflict"},
		{"a verdict after an incident without one", "incident", "", false, "A", true, "incident", "", "ack_after_incident"},
	}
	for _, c := range cases {
		set, next, reason, event := ackOutcome(c.state, c.prev, c.mdn, c.code)
		if set != c.set || next != c.next || reason != c.reason || event != c.event {
			t.Errorf("%s: got (%v %s %q %s), want (%v %s %q %s)", c.name, set, next, reason, event, c.set, c.next, c.reason, c.event)
		}
	}
}
