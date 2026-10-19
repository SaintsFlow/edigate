package store_test

import (
	"context"
	"testing"

	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/store/storetest"
)

func TestLoadFixturesTwiceDuplicatesNothing(t *testing.T) {
	s, pool := storetest.New(t)
	ctx := context.Background()
	f := store.Fixtures{
		TenantID: "bench", InterchangeID: "ZZ:ZZTESTGATEWAY", Currency: "USD",
		Partners: []store.FixturePartner{{
			PartnerID: "ZZ:ZZTESTBUY01", Role: "customer", Name: "Buyer 01", AutoMaxAmount: "5000.00", DailyAutoMax: "50000.00",
			Contacts: []string{"a@buyer.test", "b@buyer.test"}, ShipToCodes: []string{"B01-DC1"},
		}},
		Prices: []store.FixturePrice{{SKU: "SKU-1", Unit: "EA", UnitPrice: "1.50"}},
	}
	for range 2 {
		if err := s.LoadFixtures(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	for query, want := range map[string]int{
		`SELECT count(*) FROM tenants WHERE id = 'bench'`: 1,
		`SELECT count(*) FROM partners`:                   1,
		`SELECT count(*) FROM partner_contacts`:           2,
		`SELECT count(*) FROM partner_ship_to`:            1,
		`SELECT count(*) FROM price_list`:                 1,
		`SELECT count(*) FROM partners WHERE auto_max_amount = 5000 AND daily_auto_max = 50000 AND currency = 'USD'`: 1,
	} {
		var n int
		if err := pool.QueryRow(ctx, query).Scan(&n); err != nil || n != want {
			t.Errorf("%s = %d (%v), want %d", query, n, err, want)
		}
	}

	// The forecast defaults to auto_max_amount; a value in the file wins. A supplier's
	// orders are stored once, and the policy reads the partner with cents.
	f.Partners = append(f.Partners, store.FixturePartner{
		PartnerID: "ZZ:ZZTESTSUP01", Role: "supplier", Name: "Supplier 01", AutoMaxAmount: "5000.00", ForecastAmount: "1200.50",
		SupplierOrders: []store.FixtureSupplierOrder{{
			OrderNumber: "SPO01-001", Total: "30.00", Lines: []store.FixtureSupplierLine{{SKU: "SKU-1", Qty: "10", UnitPrice: "3.00"}},
		}},
	})
	for range 2 {
		if err := s.LoadFixtures(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	buyer, err := s.PartnerPolicy(ctx, "bench", "ZZ:ZZTESTBUY01")
	if err != nil || buyer.ForecastCents == nil || *buyer.ForecastCents != 500000 || *buyer.AutoMaxCents != 500000 ||
		*buyer.DailyMaxCents != 5000000 || len(buyer.ShipToCodes) != 1 || len(buyer.Contacts) != 2 || !buyer.Active {
		t.Errorf("buyer policy data = %+v %v", buyer, err)
	}
	sup, err := s.PartnerPolicy(ctx, "bench", "ZZ:ZZTESTSUP01")
	if err != nil || *sup.ForecastCents != 120050 || sup.DailyMaxCents != nil {
		t.Errorf("supplier policy data = %+v %v", sup, err)
	}
	if _, err := s.PartnerPolicy(ctx, "bench", "ZZ:NOBODY"); err != store.ErrNotFound {
		t.Errorf("unknown partner: %v", err)
	}
	if _, err := s.PartnerPolicy(ctx, "other", "ZZ:ZZTESTBUY01"); err != store.ErrNotFound {
		t.Errorf("partner of another tenant: %v", err)
	}
	so, err := s.SupplierOrder(ctx, "bench", "ZZ:ZZTESTSUP01", "SPO01-001")
	if err != nil || so.TotalCents != 3000 || so.Currency != "USD" {
		t.Errorf("supplier order = %+v %v", so, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM supplier_orders`).Scan(&n); err != nil || n != 1 {
		t.Errorf("supplier orders = %d %v, want 1", n, err)
	}
	if _, err := s.SupplierOrder(ctx, "other", "ZZ:ZZTESTSUP01", "SPO01-001"); err != store.ErrNotFound {
		t.Errorf("order of another tenant: %v", err)
	}
	f.Partners = f.Partners[:1]

	// A new price replaces the old one.
	f.Prices[0].UnitPrice = "1.75"
	if err := s.LoadFixtures(ctx, f); err != nil {
		t.Fatal(err)
	}
	p, err := s.Prices(ctx, "bench", []string{"SKU-1", "NOPE"})
	if err != nil || len(p) != 1 || p["SKU-1"].UnitPrice != "1.75" || p["SKU-1"].Unit != "EA" {
		t.Errorf("prices = %+v %v", p, err)
	}
}
