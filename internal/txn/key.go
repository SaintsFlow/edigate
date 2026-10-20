// Package txn holds the pure parts of the guarantee "at most one effect per
// business key": the idempotency key (invariant I2), the hash of the request and
// the order payload (this file and order.go), and the relay that delivers the
// outbox (relay.go). The SQL lives in internal/store.
package txn

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"

	"github.com/gowebpki/jcs"
)

// Key is k = SHA-256(part(tenant) || part(tool) || part(partner_id) || part(ref)),
// where part(x) is the length of x in bytes (uint32, big endian) followed by the
// bytes. The lengths keep ("ab","c") and ("a","bc") apart. ref is BEG03 of an 850
// without spaces at the edges; case and leading zeros stay.
func Key(tenant, tool, partnerID, ref string) [32]byte {
	h := sha256.New()
	for _, part := range []string{tenant, tool, partnerID, ref} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(part)))
		h.Write(n[:])
		h.Write([]byte(part))
	}
	var k [32]byte
	copy(k[:], h.Sum(nil))
	return k
}

// RequestKey is the key of the weak configurations: SHA-256 of the length-prefixed parts
// (as in Key) where the last part is the arguments of the call in the canonical form of
// RFC 8785. The mode reqhash (C1) passes (tool, args), the mode no-bizkey (C3-A3) passes
// (tenant, tool, args). Arguments that are not valid JSON are an error.
func RequestKey(args []byte, parts ...string) ([32]byte, error) {
	canonical, err := jcs.Transform(args)
	if err != nil {
		return [32]byte{}, err
	}
	h := sha256.New()
	for _, part := range append(parts, string(canonical)) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(part)))
		h.Write(n[:])
		h.Write([]byte(part))
	}
	var k [32]byte
	copy(k[:], h.Sum(nil))
	return k, nil
}

// BusinessKeyJSON is the form of the business key stored next to the idempotency
// key: ["<partner_id>","<ref>"].
func BusinessKeyJSON(partnerID, ref string) ([]byte, error) {
	return json.Marshal([]string{partnerID, ref})
}

// ReqHash is SHA-256 of the canonical JSON (RFC 8785) of the payload of the effect.
// It is taken over what the effect will carry, not over the raw arguments, so the
// same order that arrives again in a new exchange gives the same hash.
func ReqHash(payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	canonical, err := jcs.Transform(raw)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	return sum[:], nil
}
