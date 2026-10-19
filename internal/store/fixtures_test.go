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
