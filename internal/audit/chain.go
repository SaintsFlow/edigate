// Package audit makes the audit log tamper-evident: a hash chain over the rows of a
// tenant, signed anchors every 100 rows (in the database and in a file outside it),
// and a check that finds an edited row or a cut tail.
//
// Input: a row of audit_log (Row) to hash; a pool or transaction for the anchor
// writer (Anchorer.Run) and for Verify.
// Output: the row hash (Hash), signed anchors in audit_anchors and in
// <dir>/<tenant>.jsonl, and the verdict of Verify.
//
// The chain of a tenant: every row holds prev_hash, the hash of the previous row of the
// tenant (32 zero bytes for the first one), and hash, the SHA-256 of the canonical JSON
// (RFC 8785) of all fields of the row except hash. Writing the rows is done by
// internal/store (it takes a lock per tenant, so the chain never forks).
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/gowebpki/jcs"
)

// HashSize is the length of a row hash and of prev_hash in bytes.
const HashSize = sha256.Size

// ZeroHash is prev_hash of the first row of a tenant.
func ZeroHash() []byte { return make([]byte, HashSize) }

// Row holds the fields of an audit_log row that the hash covers. Empty Tool and
// Outcome, and nil byte fields, stand for NULL.
type Row struct {
	Seq            int64
	TenantID       string
	At             time.Time // stored with microsecond precision
	EventType      string
	TraceID        string
	PrincipalID    string
	Tool           string
	IdemKey        []byte
	ControlNumbers []byte // JSON
	PolicyExplain  []byte // JSON
	ManifestSHA256 []byte
	PayloadSHA256  []byte
	Outcome        string
	Detail         []byte // JSON
	PrevHash       []byte
}

// TimeText is the one way a timestamp enters the hash: UTC, six fractional digits.
func TimeText(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000Z") }

// Hash returns SHA-256 of the canonical record of the row. The database may store a JSON
// field in another form than it was written (jsonb reorders keys and changes spaces), so
// the JSON fields are canonicalized first and the answer does not depend on that.
func Hash(r Row) ([]byte, error) {
	record := map[string]any{
		"seq":             r.Seq,
		"tenant_id":       r.TenantID,
		"at":              TimeText(r.At),
		"event_type":      r.EventType,
		"trace_id":        r.TraceID,
		"principal_id":    r.PrincipalID,
		"tool":            nullable(r.Tool),
		"idem_key":        hexOrNull(r.IdemKey),
		"control_numbers": rawOrNull(r.ControlNumbers),
		"policy_explain":  rawOrNull(r.PolicyExplain),
		"manifest_sha256": hexOrNull(r.ManifestSHA256),
		"payload_sha256":  hexOrNull(r.PayloadSHA256),
		"outcome":         nullable(r.Outcome),
		"detail":          rawOrNull(r.Detail),
		"prev_hash":       hex.EncodeToString(r.PrevHash),
	}
	raw, err := json.Marshal(record)
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

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func hexOrNull(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return hex.EncodeToString(b)
}

func rawOrNull(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}
