package audit

// Signed anchors. Every 100 rows of a tenant the head of its chain (the hash of the
// 100th row) is signed with an ed25519 key and stored in audit_anchors; the same anchor
// is appended as one JSON line to <dir>/<tenant>.jsonl, a file outside the database.
// A tail cut out of the log together with its anchors in the database stays visible in
// that file (control AUD-03). The anchor writer runs in the gateway process (Run) on
// committed rows only, so an anchor never names a row of a rolled-back transaction.
// Several processes may run it at once: the table key makes a second insert a no-op and
// the file is locked while it is written.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gowebpki/jcs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// AnchorEvery is the number of rows of a tenant between two anchors.
const AnchorEvery = 100

// DB is the part of a pool that the anchor writer and Verify use.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Anchor is one signed head of a chain. In the file it is one JSON line.
type Anchor struct {
	TenantID  string `json:"tenant_id"`
	SeqUpto   int64  `json:"seq_upto"`
	HeadHash  string `json:"head_hash"` // hex
	Signature string `json:"signature"` // base64
	KeyID     string `json:"key_id"`
	SignedAt  string `json:"signed_at"` // RFC 3339, UTC
}

// message is what the signature covers: the canonical JSON of (tenant, seq_upto, head_hash).
func message(tenant string, seqUpto int64, headHash string) ([]byte, error) {
	raw, err := json.Marshal(map[string]any{"tenant": tenant, "seq_upto": seqUpto, "head_hash": headHash})
	if err != nil {
		return nil, err
	}
	return jcs.Transform(raw)
}

// KeyID names a public key in anchors: the first 8 bytes of its SHA-256, in hex.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// ParsePublicKey reads the base64 form of an ed25519 public key.
func ParsePublicKey(b64 string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("the public key must be 32 bytes in base64")
	}
	return ed25519.PublicKey(raw), nil
}

// ParsePrivateKey reads the base64 form of an ed25519 private key: 64 bytes, or a 32 byte seed.
func ParsePrivateKey(b64 string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, errors.New("the private key is not base64")
	}
	switch len(raw) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	}
	return nil, errors.New("the private key must be 64 bytes (or a 32 byte seed) in base64")
}

// GenerateKey makes a key pair and returns both halves in base64.
func GenerateKey() (pub, priv string, err error) {
	p, k, err := ed25519.GenerateKey(nil)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(p), base64.StdEncoding.EncodeToString(k), nil
}

// VerifyAnchor reports whether the signature of the anchor is valid for the key.
func VerifyAnchor(pub ed25519.PublicKey, a Anchor) bool {
	msg, err := message(a.TenantID, a.SeqUpto, a.HeadHash)
	if err != nil {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(a.Signature)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, msg, sig)
}

// Anchorer signs and writes the anchors. Dir is empty when no file is kept (the anchors
// then live in the database only).
type Anchorer struct {
	priv  ed25519.PrivateKey
	keyID string
	dir   string
}

// NewAnchorer makes an anchor writer from the base64 private key. dir may be empty.
func NewAnchorer(privateKeyB64, dir string) (*Anchorer, error) {
	priv, err := ParsePrivateKey(privateKeyB64)
	if err != nil {
		return nil, err
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("not an ed25519 key")
	}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("anchor directory: %w", err)
		}
	}
	return &Anchorer{priv: priv, keyID: KeyID(pub), dir: dir}, nil
}

// Run writes the anchors every interval until ctx ends, and once more at the end.
// A failed pass is logged and tried again at the next tick.
func (a *Anchorer) Run(ctx context.Context, db DB, interval time.Duration, log *slog.Logger) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			last, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := a.Once(last, db); err != nil {
				log.Warn("audit anchors: last pass failed", "error", err.Error())
			}
			cancel()
			return
		case <-tick.C:
			if err := a.Once(ctx, db); err != nil && ctx.Err() == nil {
				log.Warn("audit anchors: pass failed", "error", err.Error())
			}
		}
	}
}

// Once makes the anchors that are due (one for every full 100 rows of a tenant since its
// last anchor) and brings the anchor files up to date. Safe to call from several processes.
func (a *Anchorer) Once(ctx context.Context, db DB) error {
	tenants, err := auditTenants(ctx, db)
	if err != nil {
		return err
	}
	for _, tenant := range tenants {
		if err := a.anchorTenant(ctx, db, tenant); err != nil {
			return fmt.Errorf("tenant %s: %w", tenant, err)
		}
		if a.dir != "" {
			if err := a.syncFile(ctx, db, tenant); err != nil {
				return fmt.Errorf("tenant %s: anchor file: %w", tenant, err)
			}
		}
	}
	return nil
}

// auditTenants lists the tenants that have audit rows, by a loose index scan (the cost is
// the number of tenants, not the number of rows).
func auditTenants(ctx context.Context, db DB) ([]string, error) {
	rows, err := db.Query(ctx, `
		WITH RECURSIVE t AS (
			(SELECT min(tenant_id) AS id FROM audit_log)
			UNION ALL
			SELECT (SELECT min(tenant_id) FROM audit_log WHERE tenant_id > t.id) FROM t WHERE t.id IS NOT NULL
		) SELECT id FROM t WHERE id IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// anchorTenant signs the 100th row after the last anchor, again and again while there is one.
func (a *Anchorer) anchorTenant(ctx context.Context, db DB, tenant string) error {
	for {
		var last int64
		if err := db.QueryRow(ctx, `SELECT coalesce(max(seq_upto), 0) FROM audit_anchors WHERE tenant_id = $1`, tenant).Scan(&last); err != nil {
			return err
		}
		var seq int64
		var hash []byte
		err := db.QueryRow(ctx,
			`SELECT seq, hash FROM audit_log WHERE tenant_id = $1 AND seq > $2 AND hash IS NOT NULL
			 ORDER BY seq OFFSET $3 LIMIT 1`, tenant, last, AnchorEvery-1).Scan(&seq, &hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		head := hex.EncodeToString(hash)
		msg, err := message(tenant, seq, head)
		if err != nil {
			return err
		}
		sig := ed25519.Sign(a.priv, msg)
		if _, err := db.Exec(ctx,
			`INSERT INTO audit_anchors (tenant_id, seq_upto, head_hash, signature, key_id) VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT DO NOTHING`, tenant, seq, hash, sig, a.keyID); err != nil {
			return err
		}
	}
}

// FilePath is the anchor file of a tenant inside dir.
func FilePath(dir, tenant string) string {
	return filepath.Join(dir, url.PathEscape(tenant)+".jsonl")
}

// syncFile appends the anchors of the database that the file does not have yet. The file is
// locked while it is read and written, and every line is flushed to disk.
func (a *Anchorer) syncFile(ctx context.Context, db DB, tenant string) error {
	var has bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_anchors WHERE tenant_id = $1)`, tenant).Scan(&has); err != nil || !has {
		return err // a tenant without anchors gets no file
	}
	f, err := os.OpenFile(FilePath(a.dir, tenant), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck

	existing, err := ReadAnchorFile(f)
	if err != nil {
		return err
	}
	var haveUpto int64
	if n := len(existing); n > 0 {
		haveUpto = existing[n-1].SeqUpto
	}
	rows, err := db.Query(ctx,
		`SELECT seq_upto, head_hash, signature, key_id, signed_at FROM audit_anchors
		 WHERE tenant_id = $1 AND seq_upto > $2 ORDER BY seq_upto`, tenant, haveUpto)
	if err != nil {
		return err
	}
	var lines bytes.Buffer
	for rows.Next() {
		var an Anchor
		var head, sig []byte
		var at time.Time
		if err := rows.Scan(&an.SeqUpto, &head, &sig, &an.KeyID, &at); err != nil {
			rows.Close()
			return err
		}
		an.TenantID = tenant
		an.HeadHash = hex.EncodeToString(head)
		an.Signature = base64.StdEncoding.EncodeToString(sig)
		an.SignedAt = at.UTC().Format(time.RFC3339)
		line, err := json.Marshal(an)
		if err != nil {
			rows.Close()
			return err
		}
		lines.Write(line)
		lines.WriteByte('\n')
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if lines.Len() == 0 {
		return nil
	}
	if _, err := f.Write(lines.Bytes()); err != nil {
		return err
	}
	return f.Sync()
}

// ReadAnchorFile reads every line of an anchor file (from its start, whatever the
// position of the handle is). A line that is not an anchor is an error.
func ReadAnchorFile(f *os.File) ([]Anchor, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	var out []Anchor
	for i, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var an Anchor
		if err := json.Unmarshal(line, &an); err != nil {
			return nil, fmt.Errorf("line %d is not an anchor: %w", i+1, err)
		}
		out = append(out, an)
	}
	return out, nil
}
