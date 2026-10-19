package audit

// Verify checks the audit log of one tenant: every row hashes to its hash and points at
// the hash of the row before it; the signatures of the anchors are valid; the anchors of
// the database and of the anchor file name rows that exist with the hash they name; the
// file and the database agree. It answers with the first thing that is wrong.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// Options say what Verify checks. From and To limit the rows and the anchors checked by seq
// (0 means no limit). PubKey nil skips the signatures. AnchorDir empty skips the file.
type Options struct {
	Tenant    string
	From, To  int64
	AnchorDir string
	PubKey    ed25519.PublicKey
}

// Result is the verdict. Failure is empty when everything holds; Rows is the number of
// rows whose chain was checked.
type Result struct {
	Rows    int
	Failure string
}

// Verify runs the checks. The error is for a failed query or file; a log that does not
// verify is a Result with a Failure.
func Verify(ctx context.Context, db DB, o Options) (Result, error) {
	rows, failure, err := verifyChain(ctx, db, o)
	if err != nil || failure != "" {
		return Result{Rows: rows, Failure: failure}, err
	}
	failure, err = verifyAnchors(ctx, db, o)
	return Result{Rows: rows, Failure: failure}, err
}

// verifyChain walks the rows of the tenant in order of seq.
func verifyChain(ctx context.Context, db DB, o Options) (int, string, error) {
	expected := ZeroHash()
	var started bool
	var prior []byte
	err := db.QueryRow(ctx,
		`SELECT hash FROM audit_log WHERE tenant_id = $1 AND seq < $2 AND hash IS NOT NULL ORDER BY seq DESC LIMIT 1`,
		o.Tenant, o.From).Scan(&prior)
	switch {
	case err == nil:
		expected, started = prior, true
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, "", err
	}

	q, err := db.Query(ctx,
		`SELECT seq, tenant_id, at, event_type, trace_id, principal_id, tool, idem_key, control_numbers, policy_explain,
		        manifest_sha256, payload_sha256, outcome, detail, prev_hash, hash
		   FROM audit_log WHERE tenant_id = $1 AND seq >= $2 AND ($3::bigint = 0 OR seq <= $3) ORDER BY seq`,
		o.Tenant, o.From, o.To)
	if err != nil {
		return 0, "", err
	}
	defer q.Close()
	n := 0
	for q.Next() {
		var r Row
		var tool, outcome *string
		var stored []byte
		var at time.Time
		if err := q.Scan(&r.Seq, &r.TenantID, &at, &r.EventType, &r.TraceID, &r.PrincipalID, &tool, &r.IdemKey,
			&r.ControlNumbers, &r.PolicyExplain, &r.ManifestSHA256, &r.PayloadSHA256, &outcome, &r.Detail, &r.PrevHash, &stored); err != nil {
			return n, "", err
		}
		r.At = at
		if tool != nil {
			r.Tool = *tool
		}
		if outcome != nil {
			r.Outcome = *outcome
		}
		if stored == nil && !started {
			continue // a row written before the chain existed
		}
		started = true
		if !bytes.Equal(r.PrevHash, expected) {
			return n, fmt.Sprintf("broken at seq %d (it does not follow the row before it)", r.Seq), nil
		}
		got, err := Hash(r)
		if err != nil {
			return n, "", err
		}
		if !bytes.Equal(got, stored) {
			return n, fmt.Sprintf("broken at seq %d (the row does not match its hash)", r.Seq), nil
		}
		expected = stored
		n++
	}
	return n, "", q.Err()
}

// anchorInRange reports whether the anchor belongs to the checked range.
func anchorInRange(seqUpto int64, o Options) bool {
	return seqUpto >= o.From && (o.To == 0 || seqUpto <= o.To)
}

// verifyAnchors checks the anchors of the database and then those of the file.
func verifyAnchors(ctx context.Context, db DB, o Options) (string, error) {
	q, err := db.Query(ctx,
		`SELECT seq_upto, head_hash, signature, key_id FROM audit_anchors WHERE tenant_id = $1 ORDER BY seq_upto`, o.Tenant)
	if err != nil {
		return "", err
	}
	inDB := map[int64]Anchor{}
	for q.Next() {
		var an Anchor
		var head, sig []byte
		if err := q.Scan(&an.SeqUpto, &head, &sig, &an.KeyID); err != nil {
			q.Close()
			return "", err
		}
		an.TenantID, an.HeadHash, an.Signature = o.Tenant, hex.EncodeToString(head), b64(sig)
		inDB[an.SeqUpto] = an
	}
	q.Close()
	if err := q.Err(); err != nil {
		return "", err
	}
	for _, an := range sortedAnchors(inDB) {
		if !anchorInRange(an.SeqUpto, o) {
			continue
		}
		if msg, err := checkAnchor(ctx, db, o, an, "anchor of the database"); err != nil || msg != "" {
			return msg, err
		}
	}
	if o.AnchorDir == "" {
		return "", nil
	}

	path := FilePath(o.AnchorDir, o.Tenant)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		if len(inDB) > 0 {
			return fmt.Sprintf("the anchor file %s is missing, but the database has %d anchors", path, len(inDB)), nil
		}
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	fileAnchors, err := ReadAnchorFile(f)
	if err != nil {
		return "", err
	}
	for _, an := range fileAnchors {
		if an.TenantID != o.Tenant {
			return fmt.Sprintf("the anchor file holds an anchor of another tenant (%s)", an.TenantID), nil
		}
		if !anchorInRange(an.SeqUpto, o) {
			continue
		}
		if msg, err := checkAnchor(ctx, db, o, an, "anchor of the file"); err != nil || msg != "" {
			return msg, err
		}
		dbAn, ok := inDB[an.SeqUpto]
		switch {
		case !ok:
			return fmt.Sprintf("anchor of the file at seq_upto %d is not in the database", an.SeqUpto), nil
		case dbAn.HeadHash != an.HeadHash || dbAn.Signature != an.Signature:
			return fmt.Sprintf("anchor at seq_upto %d differs between the file and the database", an.SeqUpto), nil
		}
	}
	return "", nil
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func sortedAnchors(m map[int64]Anchor) []Anchor {
	out := make([]Anchor, 0, len(m))
	for _, an := range m {
		out = append(out, an)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SeqUpto < out[j].SeqUpto })
	return out
}

// checkAnchor verifies the signature (when there is a key) and that the log has the row the
// anchor names, with the hash it names. what says whose anchor it is, for the message.
func checkAnchor(ctx context.Context, db DB, o Options, an Anchor, what string) (string, error) {
	if o.PubKey != nil && !VerifyAnchor(o.PubKey, an) {
		return fmt.Sprintf("%s at seq_upto %d has a bad signature", what, an.SeqUpto), nil
	}
	var hash []byte
	err := db.QueryRow(ctx, `SELECT hash FROM audit_log WHERE tenant_id = $1 AND seq = $2`, o.Tenant, an.SeqUpto).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Sprintf("the database tail is shorter than the %s: no row at seq_upto %d", what, an.SeqUpto), nil
	}
	if err != nil {
		return "", err
	}
	if hex.EncodeToString(hash) != an.HeadHash {
		return fmt.Sprintf("%s at seq_upto %d does not match the row", what, an.SeqUpto), nil
	}
	return "", nil
}
