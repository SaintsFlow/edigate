package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// TokenPrefix starts every gateway token.
const TokenPrefix = "edg_"

// Principal is the caller identity found by a token. Tenant and kind come
// only from this record, never from the request.
type Principal struct {
	ID        string
	TenantID  string
	Kind      string
	PartnerID string
}

// NewToken makes a token (edg_ + base64url of 32 random bytes) and its SHA-256.
// The token is shown once; only the hash is stored.
func NewToken() (token string, sum []byte, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("random token: %w", err)
	}
	token = TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	return token, HashToken(token), nil
}

// HashToken returns the SHA-256 of a token.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// IssuePrincipal stores a principal with the hash of its token.
func (s *Store) IssuePrincipal(ctx context.Context, p Principal, tokenSHA256 []byte) error {
	var partner *string
	if p.PartnerID != "" {
		partner = &p.PartnerID
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO principals (id, tenant_id, kind, partner_id, token_sha256) VALUES ($1, $2, $3, $4, $5)`,
		p.ID, p.TenantID, p.Kind, partner, tokenSHA256)
	return err
}

// RevokePrincipal marks a principal revoked. ErrNotFound if there is none.
func (s *Store) RevokePrincipal(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE principals SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// FindPrincipal looks a principal up by token hash. Unknown and revoked
// tokens both give ErrNotFound.
func (s *Store) FindPrincipal(ctx context.Context, tokenSHA256 []byte) (Principal, error) {
	var p Principal
	var partner *string
	err := s.pool.QueryRow(ctx,
		`SELECT id, tenant_id, kind, partner_id FROM principals
		 WHERE token_sha256 = $1 AND revoked_at IS NULL`, tokenSHA256).
		Scan(&p.ID, &p.TenantID, &p.Kind, &partner)
	if isNoRows(err) {
		return Principal{}, ErrNotFound
	}
	if err != nil {
		return Principal{}, err
	}
	if partner != nil {
		p.PartnerID = *partner
	}
	return p, nil
}
