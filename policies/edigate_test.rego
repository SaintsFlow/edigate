# Tests of the policy: one or more for every rule. The data of the tools comes
# from data.json; the partners are inline test partners.
package edigate_test

import data.edigate

buyer := {
	"role": "customer",
	"active": true,
	"auto_max_cents": 500000,
	"daily_max_cents": 2000000,
	"forecast_cents": 100000,
	"ship_to_codes": ["DC01", "DC02"],
	"contacts": ["orders@buyer.test"],
}

seller := {
	"role": "supplier",
	"active": true,
	"auto_max_cents": null,
	"daily_max_cents": null,
	"forecast_cents": null,
	"ship_to_codes": ["WH01"],
	"contacts": [],
}

order(amount) := {
	"principal": {"id": "agent-1", "kind": "agent", "tenant": "ta"},
	"tool": "erp.create_sales_order",
	"effect": {"ship_to": "DC01", "amount_cents": amount, "currency": "USD"},
	"partner": buyer,
	"daily_auto_cents": 0,
	"document": {"st01": "850", "sender_role": "customer"},
	"supplier_order": null,
}

invoice(amount, lines) := {
	"principal": {"id": "agent-1", "kind": "agent", "tenant": "ta"},
	"tool": "erp.post_vendor_invoice",
	"effect": {"amount_cents": amount, "currency": "USD", "lines_norm": lines},
	"partner": seller,
	"daily_auto_cents": 0,
	"document": {"st01": "810", "sender_role": "supplier"},
	"supplier_order": {"total_cents": 30000, "currency": "USD", "lines_norm": [["SKU-1", 10000, 300]]},
}

test_order_within_limits_is_automatic if {
	d := edigate.decision with input as order(10000)
	d == {"allow": true, "tier": 1, "approval": "none", "deny": [], "fired": []}
}

test_tool_unknown if {
	i := object.union(order(100), {"tool": "erp.delete_everything"})
	d := edigate.decision with input as i
	d.allow == false
	d.deny == ["tool.unknown"]
	d.approval == "none"
}

test_partner_unknown if {
	i := object.union(order(100), {"partner": null})
	d := edigate.decision with input as i
	d.allow == false
	"partner.unknown" in d.deny
}

test_partner_inactive if {
	i := object.union(order(100), {"partner": object.union(buyer, {"active": false})})
	d := edigate.decision with input as i
	d.deny == ["partner.inactive"]
}

test_partner_role_order_from_supplier if {
	i := object.union(order(100), {"partner": object.union(buyer, {"role": "supplier"})})
	d := edigate.decision with input as i
	"partner.role" in d.deny
}

test_partner_role_wrong_document if {
	i := object.union(order(100), {"document": {"st01": "810", "sender_role": "customer"}})
	d := edigate.decision with input as i
	"partner.role" in d.deny
}

test_partner_role_invoice_from_customer if {
	i := object.union(invoice(30000, [["SKU-1", 10000, 300]]), {"partner": buyer, "document": {"st01": "810", "sender_role": "customer"}})
	d := edigate.decision with input as i
	"partner.role" in d.deny
}

test_ship_to_unknown if {
	i := object.union(order(100), {"effect": {"ship_to": "DC99", "amount_cents": 100, "currency": "USD"}})
	d := edigate.decision with input as i
	d.allow == false
	d.deny == ["ship_to.unknown"]
}

test_ship_to_empty_is_unknown if {
	i := object.union(order(100), {"effect": {"ship_to": "", "amount_cents": 100, "currency": "USD"}})
	d := edigate.decision with input as i
	d.deny == ["ship_to.unknown"]
}

test_limit_document if {
	d := edigate.decision with input as order(500001)
	d.allow == true
	d.approval == "one"
	d.fired == ["limit.document"]
}

test_limit_document_equal_passes if {
	d := edigate.decision with input as order(500000)
	d.approval == "none"
}

test_limit_not_set_means_approval if {
	i := object.union(order(100), {"partner": object.union(buyer, {"auto_max_cents": null, "daily_max_cents": null})})
	d := edigate.decision with input as i
	d.approval == "one"
	d.fired == ["limit.daily", "limit.document"]
}

test_limit_daily if {
	i := object.union(order(300000), {"daily_auto_cents": 1800000})
	d := edigate.decision with input as i
	d.approval == "one"
	d.fired == ["limit.daily"]
}

test_limit_daily_exact_passes if {
	i := object.union(order(200000), {"daily_auto_cents": 1800000})
	d := edigate.decision with input as i
	d.approval == "none"
}

confirm(amount) := object.union(order(amount), {"tool": "erp.confirm_order"})

test_forecast_within if {
	d := edigate.decision with input as confirm(125000)
	d.tier == 2
	d.approval == "none"
}

test_forecast_deviation_over if {
	d := edigate.decision with input as confirm(125001)
	d.approval == "one"
	d.fired == ["forecast.deviation"]
}

test_forecast_deviation_under if {
	d := edigate.decision with input as confirm(74999)
	d.fired == ["forecast.deviation"]
}

test_forecast_missing if {
	i := object.union(confirm(100000), {"partner": object.union(buyer, {"forecast_cents": null})})
	d := edigate.decision with input as i
	d.fired == ["forecast.deviation"]
}

test_forecast_limits_do_not_apply_to_t2 if {
	d := edigate.decision with input as confirm(100000)
	d.fired == []
}

test_invoice_order_unknown if {
	i := object.union(invoice(30000, [["SKU-1", 10000, 300]]), {"supplier_order": null})
	d := edigate.decision with input as i
	d.allow == false
	d.deny == ["invoice.order_unknown"]
}

test_invoice_order_mismatch_amount if {
	d := edigate.decision with input as invoice(30001, [["SKU-1", 10000, 300]])
	d.deny == ["invoice.order_mismatch"]
}

test_invoice_order_mismatch_lines if {
	d := edigate.decision with input as invoice(30000, [["SKU-2", 10000, 300]])
	d.deny == ["invoice.order_mismatch"]
}

test_invoice_matching_needs_two if {
	d := edigate.decision with input as invoice(30000, [["SKU-1", 10000, 300]])
	d.allow == true
	d.tier == 3
	d.approval == "two"
	d.fired == ["tier.three"]
}

test_denied_invoice_is_not_an_approval if {
	d := edigate.decision with input as invoice(30001, [["SKU-1", 10000, 300]])
	d.approval == "none"
}

# ---- wave 2-8: letters and upstream tools ----

mail(to) := object.union(order(0), {
	"tool": "notify.send_email",
	"effect": {"to": to, "template_id": "tpl_order_received", "amount_cents": 0},
})

test_email_to_a_contact_is_automatic if {
	d := edigate.decision with input as mail("orders@buyer.test")
	d == {"allow": true, "tier": 2, "approval": "none", "deny": [], "fired": []}
}

test_email_to_a_stranger_is_denied if {
	d := edigate.decision with input as mail("someone@else.test")
	d.allow == false
	d.deny == ["email.recipient"]
	d.approval == "none"
}

test_email_for_an_unknown_partner_is_denied if {
	i := object.union(mail("orders@buyer.test"), {"partner": null})
	d := edigate.decision with input as i
	d.allow == false
	"partner.unknown" in d.deny
}

test_email_ignores_the_forecast if {
	i := object.union(mail("orders@buyer.test"), {"partner": object.union(buyer, {"forecast_cents": null})})
	d := edigate.decision with input as i
	d.fired == []
}

upstream_call(tool) := {
	"principal": {"id": "agent-1", "kind": "agent", "tenant": "ta"},
	"tool": tool,
	"effect": {"city": "Almaty"},
	"partner": null,
	"daily_auto_cents": 0,
	"document": {"st01": "", "sender_role": ""},
	"supplier_order": null,
}

test_upstream_is_denied_by_default if {
	d := edigate.decision with input as upstream_call("up.rates.get_rates")
	d.allow == false
	d.deny == ["upstream.not_allowed"]
}

test_upstream_allowed_for_the_tenant if {
	d := edigate.decision with input as upstream_call("up.rates.get_rates") with data.upstream_allow as {"ta": ["up.rates.get_rates"]}
	d == {"allow": true, "tier": 2, "approval": "none", "deny": [], "fired": []}
}

test_upstream_allowed_for_another_tenant_only if {
	d := edigate.decision with input as upstream_call("up.rates.get_rates") with data.upstream_allow as {"tb": ["up.rates.get_rates"]}
	d.deny == ["upstream.not_allowed"]
}

test_upstream_allows_one_tool_only if {
	d := edigate.decision with input as upstream_call("up.rates.other") with data.upstream_allow as {"ta": ["up.rates.get_rates"]}
	d.deny == ["upstream.not_allowed"]
}
