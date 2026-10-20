package txn

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"
)

// The reference value was computed outside the code, with SHA-256 over the four
// parts, each written as a 4-byte big-endian length and the bytes.
func TestKeyReferenceValue(t *testing.T) {
	k := Key("tenant-a", "erp.create_sales_order", "ZZ:ZZTESTBUYER", "PO-001")
	const want = "4db1f6cfd2eef873bd5e1aaac7305f83afde5d0694bd1ae0522bd9631ecb4f2e"
	if got := hex.EncodeToString(k[:]); got != want {
		t.Fatalf("key = %s, want %s", got, want)
	}
}

func TestKeyChangesWithEveryPart(t *testing.T) {
	base := Key("t", "tool", "ZZ:P", "PO-1")
	for name, other := range map[string][32]byte{
		"tenant":  Key("t2", "tool", "ZZ:P", "PO-1"),
		"tool":    Key("t", "tool2", "ZZ:P", "PO-1"),
		"partner": Key("t", "tool", "ZZ:P2", "PO-1"),
		"ref":     Key("t", "tool", "ZZ:P", "PO-2"),
		"case":    Key("t", "tool", "ZZ:P", "po-1"),
		"zeros":   Key("t", "tool", "ZZ:P", "PO-01"),
	} {
		if other == base {
			t.Errorf("changing the %s does not change the key", name)
		}
	}
	if Key("t", "tool", "ZZ:P", "PO-1") != base {
		t.Error("the key is not stable")
	}
}

// Without lengths, ("ab","c") and ("a","bc") would give the same bytes.
func TestKeyPartsAreLengthPrefixed(t *testing.T) {
	a, b := Key("ab", "c", "", ""), Key("a", "bc", "", "")
	if a == b {
		t.Fatal("the key does not tell ab|c from a|bc")
	}
	const wantA = "1c3f8754d58ed7595901b4b0c8ac1313dd10cbd41480646e3f3f6f748b6deccb"
	if got := hex.EncodeToString(a[:]); got != wantA {
		t.Errorf("key(ab,c) = %s, want %s", got, wantA)
	}
}

func TestBusinessKeyJSON(t *testing.T) {
	got, err := BusinessKeyJSON("ZZ:ZZTESTBUYER", "PO-001")
	if err != nil || string(got) != `["ZZ:ZZTESTBUYER","PO-001"]` {
		t.Fatalf("business key = %s (%v)", got, err)
	}
}

// The hash does not depend on the order of fields in the source and it changes
// with any value of the payload.
func TestReqHash(t *testing.T) {
	p := OrderPayload{PartnerID: "ZZ:P", BusinessRef: "PO-1", Lines: []OrderLine{{SKU: "A", Qty: "1.000", Unit: "EA", UnitPrice: "2.00"}}, Total: "2.00", Currency: "USD"}
	h1, err := ReqHash(p)
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := ReqHash(p)
	if !bytes.Equal(h1, h2) || len(h1) != 32 {
		t.Fatal("the hash is not stable")
	}
	q := p
	q.DeliveryInstruction = "none"
	h3, _ := ReqHash(q)
	if bytes.Equal(h1, h3) {
		t.Error("another delivery instruction must change the hash")
	}
	r := p
	r.Lines = []OrderLine{{SKU: "A", Qty: "2.000", Unit: "EA", UnitPrice: "2.00"}}
	if h4, _ := ReqHash(r); bytes.Equal(h1, h4) {
		t.Error("another quantity must change the hash")
	}
}

// The pause doubles from 1 s to 128 s after the eighth failure, shrinks with the
// time scale and never goes under 10 ms.
func TestRelayDelaySchedule(t *testing.T) {
	r := &Relay{}
	want := []time.Duration{1, 2, 4, 8, 16, 32, 64, 128}
	for i, w := range want {
		if got := r.delay(i); got != w*time.Second {
			t.Errorf("delay after failure %d = %v, want %v", i+1, got, w*time.Second)
		}
	}
	r.Scale = 20
	if got := r.delay(3); got != 400*time.Millisecond {
		t.Errorf("scaled delay = %v", got)
	}
	r.Scale = 1e9
	if got := r.delay(0); got != 10*time.Millisecond {
		t.Errorf("minimum delay = %v", got)
	}
}
