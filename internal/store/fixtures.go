package store

import "context"

// Fixtures is the starting data of a stand: one tenant, its partners with limits,
// contacts and ship-to codes, and the price list. Amounts are decimal strings.
type Fixtures struct {
	TenantID      string
	InterchangeID string
	Currency      string
	Partners      []FixturePartner
	Prices        []FixturePrice
}

// FixturePartner is a partner with its limits, contacts and ship-to codes.
type FixturePartner struct {
	PartnerID     string
	Role          string
	Name          string
	AutoMaxAmount string
	DailyAutoMax  string
	Contacts      []string
	ShipToCodes   []string
}

// FixturePrice is a row of the price list.
type FixturePrice struct {
	SKU       string
	Unit      string
	UnitPrice string
}

// LoadFixtures writes the data in one transaction. Running it again changes
// nothing that is already there: tenants, partners and prices are updated in place,
// contacts and ship-to codes are added only when new.
func (s *Store) LoadFixtures(ctx context.Context, f Fixtures) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`INSERT INTO tenants (id, name, interchange_id) VALUES ($1, $1, NULLIF($2, ''))
		 ON CONFLICT (id) DO UPDATE SET interchange_id = COALESCE(EXCLUDED.interchange_id, tenants.interchange_id)`,
		f.TenantID, f.InterchangeID); err != nil {
		return err
	}
	for _, p := range f.Partners {
		if _, err := tx.Exec(ctx,
			`INSERT INTO partners (tenant_id, partner_id, role, name, auto_max_amount, daily_auto_max, currency)
			 VALUES ($1, $2, $3, $4, NULLIF($5, '')::numeric, NULLIF($6, '')::numeric, $7)
			 ON CONFLICT (tenant_id, partner_id) DO UPDATE
			   SET role = EXCLUDED.role, name = EXCLUDED.name, auto_max_amount = EXCLUDED.auto_max_amount,
			       daily_auto_max = EXCLUDED.daily_auto_max, currency = EXCLUDED.currency`,
			f.TenantID, p.PartnerID, p.Role, p.Name, p.AutoMaxAmount, p.DailyAutoMax, f.Currency); err != nil {
			return err
		}
		for _, email := range p.Contacts {
			if _, err := tx.Exec(ctx,
				`INSERT INTO partner_contacts (tenant_id, partner_id, email) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
				f.TenantID, p.PartnerID, email); err != nil {
				return err
			}
		}
		for _, code := range p.ShipToCodes {
			if _, err := tx.Exec(ctx,
				`INSERT INTO partner_ship_to (tenant_id, partner_id, code) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
				f.TenantID, p.PartnerID, code); err != nil {
				return err
			}
		}
	}
	for _, p := range f.Prices {
		if _, err := tx.Exec(ctx,
			`INSERT INTO price_list (tenant_id, sku, unit_price, currency, unit) VALUES ($1, $2, $3::numeric, $4, $5)
			 ON CONFLICT (tenant_id, sku) DO UPDATE
			   SET unit_price = EXCLUDED.unit_price, currency = EXCLUDED.currency, unit = EXCLUDED.unit`,
			f.TenantID, p.SKU, p.UnitPrice, f.Currency, p.Unit); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
