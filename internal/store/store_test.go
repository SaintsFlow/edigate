package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/store/storetest"
)

func TestMigrationsApply(t *testing.T) {
	s, pool := storetest.New(t)
	ctx := context.Background()
	if err := s.CheckMigrated(ctx); err != nil {
		t.Fatalf("CheckMigrated: %v", err)
	}
	for _, table := range []string{"tenants", "principals", "audit_log", "partners", "edi_interchanges", "edi_documents", "edi_rejected"} {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}
}

func TestAuditLogRejectsUpdateAndDelete(t *testing.T) {
	s, pool := storetest.New(t)
	ctx := context.Background()
	err := s.AppendAudit(ctx, store.AuditEvent{
		TenantID: "t1", EventType: "tool_call", TraceID: "abc", PrincipalID: "p1", Outcome: "ok",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_log SET outcome = 'x'`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Errorf("UPDATE must fail with the trigger error, got %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM audit_log`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Errorf("DELETE must fail with the trigger error, got %v", err)
	}
}

func TestFindPrincipalByTokenHash(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := context.Background()
	if err := s.AddTenant(ctx, "t1", "Tenant One", ""); err != nil {
		t.Fatal(err)
	}
	token, sum, err := store.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, store.TokenPrefix) {
		t.Fatalf("token %q lacks the prefix", token)
	}
	if err := s.IssuePrincipal(ctx, store.Principal{ID: "a1", TenantID: "t1", Kind: "agent"}, sum); err != nil {
		t.Fatal(err)
	}
	p, err := s.FindPrincipal(ctx, store.HashToken(token))
	if err != nil || p.ID != "a1" || p.TenantID != "t1" || p.Kind != "agent" {
		t.Fatalf("issued token not found: %+v %v", p, err)
	}
	if _, err := s.FindPrincipal(ctx, store.HashToken("edg_unknown")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown token: want ErrNotFound, got %v", err)
	}
	if err := s.RevokePrincipal(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FindPrincipal(ctx, store.HashToken(token)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoked token: want ErrNotFound, got %v", err)
	}
}

func TestPartnerIDOnlyForPartnerKind(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := context.Background()
	if err := s.AddTenant(ctx, "t1", "Tenant One", ""); err != nil {
		t.Fatal(err)
	}
	_, sum, _ := store.NewToken()
	if err := s.IssuePrincipal(ctx, store.Principal{ID: "p1", TenantID: "t1", Kind: "partner"}, sum); err == nil {
		t.Error("partner without partner_id must be rejected")
	}
	_, sum, _ = store.NewToken()
	if err := s.IssuePrincipal(ctx, store.Principal{ID: "p2", TenantID: "t1", Kind: "agent", PartnerID: "X:Y"}, sum); err == nil {
		t.Error("agent with partner_id must be rejected")
	}
}

func TestPartnerTokenNeedsARegisteredPartner(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := context.Background()
	if err := s.AddTenant(ctx, "t1", "Tenant One", "ZZ:ZZTESTSUPPL"); err != nil {
		t.Fatal(err)
	}
	_, sum, _ := store.NewToken()
	if err := s.IssuePrincipal(ctx, store.Principal{ID: "p1", TenantID: "t1", Kind: "partner", PartnerID: "ZZ:ZZTESTBUYER"}, sum); err == nil {
		t.Error("a partner token for an unknown partner must be rejected")
	}
	if err := s.AddPartner(ctx, store.Partner{TenantID: "t1", PartnerID: "ZZ:ZZTESTBUYER", Role: "customer", Name: "Buyer"}); err != nil {
		t.Fatal(err)
	}
	if err := s.IssuePrincipal(ctx, store.Principal{ID: "p1", TenantID: "t1", Kind: "partner", PartnerID: "ZZ:ZZTESTBUYER"}, sum); err != nil {
		t.Errorf("a partner token for a registered partner: %v", err)
	}
	access, err := s.PartnerAccess(ctx, "t1", "ZZ:ZZTESTBUYER")
	if err != nil || access.Role != "customer" || !access.Active || access.GatewayID != "ZZ:ZZTESTSUPPL" {
		t.Fatalf("access: %+v %v", access, err)
	}
	if _, err := s.PartnerAccess(ctx, "t1", "ZZ:NOBODY"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown partner: %v", err)
	}
	if err := s.AddPartner(ctx, store.Partner{TenantID: "t1", PartnerID: "ZZ:ZZTESTBUYER", Role: "bogus", Name: "x"}); err == nil {
		t.Error("an unknown role must be rejected")
	}
}

func TestInterchangeIDIsUniqueAndKeptOnReAdd(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := context.Background()
	if err := s.AddTenant(ctx, "t1", "One", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTenant(ctx, "t1", "One", "ZZ:ZZTESTSUPPL"); err != nil { // sets it on the existing tenant
		t.Fatal(err)
	}
	if err := s.AddTenant(ctx, "t1", "One", ""); err != nil { // does not clear it
		t.Fatal(err)
	}
	if err := s.AddPartner(ctx, store.Partner{TenantID: "t1", PartnerID: "ZZ:P", Role: "customer", Name: "P"}); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.PartnerAccess(ctx, "t1", "ZZ:P"); a.GatewayID != "ZZ:ZZTESTSUPPL" {
		t.Fatalf("interchange id: %q", a.GatewayID)
	}
	if err := s.AddTenant(ctx, "t2", "Two", "ZZ:ZZTESTSUPPL"); err == nil {
		t.Error("two tenants with one interchange id must be rejected")
	}
}
