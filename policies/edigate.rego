# Business policies of the gateway (control M6, flows P4 and P7).
#
# Input (built by the server, never by the client; see internal/policy):
#   principal       {id, kind, tenant}
#   tool            name of the tool
#   effect          what the effect will carry; amount_cents is its total in cents,
#                   lines_norm the lines of an invoice in a fixed order
#   partner         null, or {role, active, auto_max_cents, daily_max_cents,
#                   forecast_cents, ship_to_codes, contacts}; a limit that is
#                   null is not set
#   daily_auto_cents  cents the partner already got automatically today (UTC)
#   document        {st01, sender_role}
#   supplier_order  null, or {total_cents, currency, lines_norm}
# Data (data.json): the tier of every tool, the role and the document it needs,
# and the allowed deviation from the forecast in percent. upstream_allow lists, by
# tenant, the up.* tools the tenant may call (empty by default: all are forbidden).
# A tool may carry no_amount (nothing to compare with the forecast) and check_recipient
# (input.effect.to must be a contact of the partner).
#
# Output: data.edigate.decision = {allow, tier, approval, deny, fired}.
# deny names the rules that forbid the action; fired names the rules that turn an
# allowed action into an approval request. The names are stable: they go to
# data.explain of the error, to the audit log and to the operator.
package edigate

tool := data.tools[input.tool]

partner_known if is_object(input.partner)

# A tool of an upstream MCP server (wave 2-8). It has no partner and no document: the
# only question is whether the tenant may call it (data.upstream_allow).
is_upstream if startswith(input.tool, "up.")

# ---- deny ----

deny contains "tool.unknown" if {
	not tool
	not is_upstream
}

deny contains "partner.unknown" if {
	not partner_known
	not is_upstream
}

# An upstream tool is forbidden unless the data of the policy names it for the tenant (TL-07).
deny contains "upstream.not_allowed" if {
	is_upstream
	not input.tool in object.get(data.upstream_allow, input.principal.tenant, [])
}

# A letter goes only to a contact of the partner of the source document (XP-01).
deny contains "email.recipient" if {
	tool.check_recipient
	partner_known
	not input.effect.to in input.partner.contacts
}

deny contains "partner.inactive" if {
	partner_known
	input.partner.active != true
}

deny contains "partner.role" if {
	tool
	partner_known
	wrong_role
}

wrong_role if input.partner.role != tool.partner_role

wrong_role if input.document.sender_role != tool.partner_role

wrong_role if input.document.st01 != tool.document

# A ship-to that is not one of the partner's own codes (VAL-02, INJ-02).
deny contains "ship_to.unknown" if {
	partner_known
	input.effect.ship_to
	not input.effect.ship_to in input.partner.ship_to_codes
}

# A supplier invoice must match an order to that supplier (INJ-07).
deny contains "invoice.order_unknown" if {
	tool.check_supplier_order
	not is_object(input.supplier_order)
}

deny contains "invoice.order_mismatch" if {
	tool.check_supplier_order
	is_object(input.supplier_order)
	not invoice_matches
}

invoice_matches if {
	input.supplier_order.total_cents == input.effect.amount_cents
	input.supplier_order.currency == input.effect.currency
	input.supplier_order.lines_norm == input.effect.lines_norm
}

# ---- fired: the action waits for an operator ----

# Limits of the partner for a reversible write (T1). A limit that is not set
# means nothing goes through automatically.
fired contains "limit.document" if {
	tier == 1
	not within_document
}

within_document if {
	is_number(input.partner.auto_max_cents)
	input.effect.amount_cents <= input.partner.auto_max_cents
}

fired contains "limit.daily" if {
	tier == 1
	not within_daily
}

within_daily if {
	is_number(input.partner.daily_max_cents)
	input.daily_auto_cents + input.effect.amount_cents <= input.partner.daily_max_cents
}

# An irreversible action (T2) goes through automatically only close to the forecast.
# A tool without an amount (a letter) has nothing to compare, and an upstream tool has no partner.
fired contains "forecast.deviation" if {
	tier == 2
	tool
	not tool.no_amount
	not within_forecast
}

within_forecast if {
	is_number(input.partner.forecast_cents)
	input.partner.forecast_cents > 0
	abs(input.effect.amount_cents - input.partner.forecast_cents) * 100 <= data.forecast_pct * input.partner.forecast_cents
}

# Money (T3) always needs two operators.
fired contains "tier.three" if tier == 3

# ---- decision ----

tier := tool.tier

# An upstream tool has an effect we cannot see, so it is counted as irreversible (T2).
tier := 2 if is_upstream

default tier := -1

default allow := false

allow if count(deny) == 0

default approval := "none"

approval := "two" if {
	allow
	tier == 3
}

approval := "one" if {
	allow
	tier != 3
	count(fired) > 0
}

decision := {
	"allow": allow,
	"tier": tier,
	"approval": approval,
	"deny": sort(deny),
	"fired": sort(fired),
}
