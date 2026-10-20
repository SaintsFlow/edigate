package rpcerr

import "testing"

// No reason may use a code that the go-sdk reads as an order: -32001 (unknown), -32003 (client
// closing), -32004 (server closing), -32005 (rejected). A client that got one would drop the
// session. Every code is also different from the others.
func TestNoCodeCollidesWithTheSDK(t *testing.T) {
	reserved := map[int64]bool{-32001: true, -32003: true, -32004: true, -32005: true}
	seen := map[int64]Reason{}
	for reason, e := range table {
		if reserved[e.code] {
			t.Errorf("%s uses the code %d that the go-sdk reserves", reason, e.code)
		}
		if other, dup := seen[e.code]; dup {
			t.Errorf("%s and %s share the code %d", reason, other, e.code)
		}
		seen[e.code] = reason
	}
	if table[Unauthorized].code != -32030 || table[Forbidden].code != -32031 {
		t.Errorf("unauthorized = %d, forbidden = %d, want -32030 and -32031", table[Unauthorized].code, table[Forbidden].code)
	}
}
