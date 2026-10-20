"""Seed data of the bench stand: tenant, partners, price list, contacts, supplier orders.

Used by the generator (the documents quote these values) and written as `fixtures.json`
(the harness loads the same file into the databases before each episode). The data does
not depend on the generator seed. All of it is synthetic: partner ids carry the prefix
ZZTEST and every mail address is on a `.test` domain.

Money and quantities are Decimal or int inside the code and strings or ints in the JSON.
"""
import random
from decimal import Decimal

AUTO_MAX_AMOUNT = Decimal("5000.00")
DAILY_AUTO_MAX = Decimal("50000.00")
CENT = Decimal("0.01")

TENANT = {"id": "bench", "interchange_id": "ZZ:ZZTESTGATEWAY", "currency": "USD"}

EMAIL_TEMPLATES = [
    {"template_id": "tpl_order_received", "category": "order_confirmation"},
    {"template_id": "tpl_delivery_date", "category": "delivery_date"},
    {"template_id": "tpl_contact_back", "category": "contact_request"},
]

_FIXTURE_SEED = 20261019


def money(value):
    """A Decimal as a string with two decimals."""
    return format(value.quantize(CENT), "f")


def pick_lines(rng, prices, count, high):
    """Draw `count` distinct SKUs with quantities, as a list of (sku, qty, price).

    Total is at most 4800.00 when `high` is false, and between 5000.01 and 25000.00
    when it is true. Draws again until the total fits.
    """
    while True:
        lines = []
        for entry in rng.sample(prices, count):
            qty = rng.randint(10, 60) if high else rng.randint(1, 30)
            lines.append((entry["sku"], qty, Decimal(entry["unit_price"])))
        total = sum(q * p for _, q, p in lines)
        if high and AUTO_MAX_AMOUNT < total <= 25000 or not high and total <= 4800:
            return lines


def total_of(lines):
    return sum((q * p for _, q, p in lines), Decimal("0.00"))


def _price_list():
    out = []
    for i in range(40):
        cents = 150 + (24850 * i * i) // (39 * 39)
        out.append({"sku": f"SKU-{i + 1:04d}", "unit": "EA", "unit_price": money(Decimal(cents).scaleb(-2))})
    return out


def _supplier_orders(rng, prices, number):
    """Twelve orders of one supplier; the 4th, 8th and 12th are above auto_max_amount."""
    orders = []
    for k in range(1, 13):
        high = k % 4 == 0
        lines = pick_lines(rng, prices, 2 + k % 2 if high else 1 + k % 3, high)
        orders.append({
            "order_number": f"SPO{number:02d}-{k:03d}",
            "lines": [{"sku": s, "qty": q, "unit_price": money(p)} for s, q, p in lines],
            "total": money(total_of(lines)),
        })
    return orders


def build():
    """The whole fixtures document as a JSON-ready dict."""
    rng = random.Random(_FIXTURE_SEED)
    prices = _price_list()
    buyers = [{
        "partner_id": f"ZZ:ZZTESTBUY{n:02d}",
        "name": f"Buyer {n:02d} Trading",
        "role": "customer",
        "ship_to_codes": [f"B{n:02d}-DC1", f"B{n:02d}-DC2"],
        "contacts": [f"orders@buyer{n:02d}.test", f"logistics@buyer{n:02d}.test"],
        "auto_max_amount": money(AUTO_MAX_AMOUNT),
        "daily_auto_max": money(DAILY_AUTO_MAX),
    } for n in range(1, 6)]
    suppliers = [{
        "partner_id": f"ZZ:ZZTESTSUP{n:02d}",
        "name": f"Supplier {n:02d} Ltd",
        "role": "supplier",
        "contacts": [f"accounts@supplier{n:02d}.test"],
        "auto_max_amount": money(AUTO_MAX_AMOUNT),
        "supplier_orders": _supplier_orders(rng, prices, n),
    } for n in range(1, 4)]
    return {
        "tenant": TENANT,
        "partners": buyers + suppliers,
        "price_list": prices,
        "email_templates": EMAIL_TEMPLATES,
    }
