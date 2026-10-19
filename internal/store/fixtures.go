package store

import (
	"context"
	"encoding/json"
)

// Fixtures is the starting data of a stand: one tenant, its partners with limits,
// contacts and ship-to codes, and the price list. Amounts are decimal strings.
type Fixtures struct {
	TenantID      string
	InterchangeID string
	Currency      string
	Partners      []FixturePartner
	Prices        []FixturePrice
	Templates     []FixtureTemplate
}

// FixtureTemplate is a mail template the tenant may use with notify.send_email.
type FixtureTemplate struct {
	TemplateID string
	Category   string
}

// FixturePartner is a partner with its limits, contacts and ship-to codes.
type FixturePartner struct {
	PartnerID     string
	Role          string
	Name          string
	AutoMaxAmount string
	DailyAutoMax  string
	// ForecastAmount is the expected order amount; empty means the same as AutoMaxAmount.
	ForecastAmount string
	Contacts       []string
	ShipToCodes    []string
	SupplierOrders []FixtureSupplierOrder
}

// FixtureSupplierOrder is an order to a supplier that an invoice is checked against.
type FixtureSupplierOrder struct {
	OrderNumber string
	Total       string
	Lines       []FixtureSupplierLine
}

// FixtureSupplierLine is a line of such an order; Qty and UnitPrice are decimal strings.
type FixtureSupplierLine struct {
	SKU       string `json:"sku"`
	Qty       string `json:"qty"`
	UnitPrice string `json:"unit_price"`
}

// FixturePrice is a row of the price list.
type FixturePrice struct {
	SKU       string
	Unit      string
	UnitPrice string
}

// IssueMissingPartnerTokens gives a token to each of the partners that has no active one and
// returns the tokens by partner id. The principal is named stand-<tenant>-<partner>. The
// token is shown here once; only its hash is stored.
func (s *Store) IssueMissingPartnerTokens(ctx context.Context, tenantID string, partnerIDs []string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT p.partner_id FROM partners p
		 WHERE p.tenant_id = $1 AND p.partner_id = ANY($2)
		   AND NOT EXISTS (SELECT 1 FROM principals pr WHERE pr.tenant_id = p.tenant_id AND pr.partner_id = p.partner_id
		                   AND pr.kind = 'partner' AND pr.revoked_at IS NULL)
		 ORDER BY p.partner_id`, tenantID, partnerIDs)
	if err != nil {
		return nil, err
	}
	var missing []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		missing = append(missing, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	tokens := map[string]string{}
	for _, id := range missing {
		token, sum, err := NewToken()
		if err != nil {
			return nil, err
		}
		p := Principal{ID: "stand-" + tenantID + "-" + id, TenantID: tenantID, Kind: "partner", PartnerID: id}
		if err := s.IssuePrincipal(ctx, p, sum); err != nil {
			return nil, err
		}
		tokens[id] = token
	}
	return tokens, nil
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
		`INSERT INTO tenants (id, name, interchange_id, currency) VALUES ($1, $1, NULLIF($2, ''), COALESCE(NULLIF($3, ''), 'USD'))
		 ON CONFLICT (id) DO UPDATE SET interchange_id = COALESCE(EXCLUDED.interchange_id, tenants.interchange_id),
		                                currency = COALESCE(NULLIF($3, ''), tenants.currency)`,
		f.TenantID, f.InterchangeID, f.Currency); err != nil {
		return err
	}
	for _, p := range f.Partners {
		if _, err := tx.Exec(ctx,
			`INSERT INTO partners (tenant_id, partner_id, role, name, auto_max_amount, daily_auto_max, currency, forecast_amount)
			 VALUES ($1, $2, $3, $4, NULLIF($5, '')::numeric, NULLIF($6, '')::numeric, $7,
			         COALESCE(NULLIF($8, '')::numeric, NULLIF($5, '')::numeric))
			 ON CONFLICT (tenant_id, partner_id) DO UPDATE
			   SET role = EXCLUDED.role, name = EXCLUDED.name, auto_max_amount = EXCLUDED.auto_max_amount,
			       daily_auto_max = EXCLUDED.daily_auto_max, currency = EXCLUDED.currency,
			       forecast_amount = EXCLUDED.forecast_amount`,
			f.TenantID, p.PartnerID, p.Role, p.Name, p.AutoMaxAmount, p.DailyAutoMax, f.Currency, p.ForecastAmount); err != nil {
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
		for _, o := range p.SupplierOrders {
			lines, err := json.Marshal(o.Lines)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO supplier_orders (tenant_id, partner_id, order_number, lines, total, currency)
				 VALUES ($1, $2, $3, $4, $5::numeric, $6)
				 ON CONFLICT (tenant_id, partner_id, order_number) DO UPDATE
				   SET lines = EXCLUDED.lines, total = EXCLUDED.total, currency = EXCLUDED.currency`,
				f.TenantID, p.PartnerID, o.OrderNumber, lines, o.Total, f.Currency); err != nil {
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
	for _, t := range f.Templates {
		if _, err := tx.Exec(ctx,
			`INSERT INTO email_templates (tenant_id, template_id, category) VALUES ($1, $2, $3)
			 ON CONFLICT (tenant_id, template_id) DO UPDATE SET category = EXCLUDED.category`,
			f.TenantID, t.TemplateID, t.Category); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
