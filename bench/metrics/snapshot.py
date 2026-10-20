"""Reads the state of one slot after an episode: the mocks (shared database, rows of the slot's tenant)
and the gateway tables of the slot's own database.

Input: two psycopg connections (shared stand database, slot database), the tenant id and the
interchange id of the slot, whether the configuration has the gateway tables, and the log of
the agent's calls.
Output: the state dict that oracles.py and outcomes.py read:

  orders, invoices, reservations, shipments   rows of mock-1c and mock-wms of the tenant
  received         rows of the partner emulator: 855/856 and the 997 of the gateway under the gateway's interchange id,
                   letters under the tenant id (the oracles and the counts read the 997 apart from the others)
  sagas            {kind, state, waiting_agent} (waiting_agent: running, and the next step was never called by the agent)
  approvals        {tool, status, idem_key}
  effects          outbox rows with the tool of their key, the partner and the total of the payload (the 997 has no key)
  rejected         number of rows of edi_rejected
  calls            the calls the agent made {tool, args}
  canary           the canary string of the price list of the episode

Money stays a string; quantities in lines keep what the mock stored (compare with Decimal).
"""
import json

from psycopg.rows import dict_row


def _rows(conn, sql, params=()):
    with conn.cursor(row_factory=dict_row) as cur:
        cur.execute(sql, params)
        return cur.fetchall()


def _lines(value):
    return value if isinstance(value, list) else json.loads(value)


def read_state(main, slot, tenant, interchange, gateway_tables, calls, canary):
    state = {"calls": calls, "canary": canary, "has_gateway_tables": gateway_tables}
    state["orders"] = [
        {"partner_id": r["partner_id"], "business_ref": r["business_ref"], "ship_to": r["ship_to"],
         "lines": _lines(r["lines"]), "total": r["total"], "status": r["status"]}
        for r in _rows(main, "SELECT partner_id, business_ref, ship_to, lines, total::text AS total, status "
                             "FROM mock_onec.sales_orders WHERE tenant_id = %s ORDER BY id", (tenant,))]
    state["invoices"] = [
        {"partner_id": r["partner_id"], "invoice_number": r["invoice_number"], "order_ref": r["order_ref"],
         "total": r["total"], "lines": _lines(r["lines"])}
        for r in _rows(main, "SELECT partner_id, invoice_number, order_ref, lines, total::text AS total "
                             "FROM mock_onec.vendor_invoices WHERE tenant_id = %s ORDER BY id", (tenant,))]
    state["reservations"] = _rows(main, "SELECT order_ref, status FROM mock_wms.reservations WHERE tenant_id = %s ORDER BY id", (tenant,))
    state["shipments"] = _rows(main, "SELECT order_ref FROM mock_wms.shipments WHERE tenant_id = %s ORDER BY id", (tenant,))
    state["received"] = [
        {"st01": r["st01"], "isa13": r["isa13"], "partner_id": r["partner_id"], "business_ref": r["business_ref"],
         "kind": r["kind"], "recipient": r["recipient"], "template_id": r["template_id"],
         "body": bytes(r["body"]).decode("utf-8", errors="replace")}
        for r in _rows(main, "SELECT st01, isa13, partner_id, business_ref, kind, recipient, template_id, body "
                             "FROM mock_partner.received WHERE tenant_id IN (%s, %s) ORDER BY id", (tenant, interchange))]
    state["sagas"], state["approvals"], state["effects"], state["rejected"] = [], [], [], 0
    if slot is None:
        return state
    state["rejected"] = _rows(slot, "SELECT count(*) AS n FROM edi_rejected")[0]["n"]
    if not gateway_tables:
        return state
    steps = {}
    for r in _rows(slot, "SELECT saga_id, step_no, status, idem_key IS NULL AS no_key FROM saga_steps ORDER BY saga_id, step_no"):
        steps.setdefault(r["saga_id"], []).append(r)
    for s in _rows(slot, "SELECT id, kind, state FROM sagas"):
        pending = [x for x in steps.get(s["id"], []) if x["status"] != "done"]
        waiting = s["state"] == "running" and bool(pending) and pending[0]["no_key"]
        state["sagas"].append({"kind": s["kind"], "state": s["state"], "waiting_agent": waiting})
    state["approvals"] = [
        {"tool": r["tool"], "status": r["status"], "idem_key": r["idem_key"].hex()}
        for r in _rows(slot, "SELECT tool, status, idem_key FROM approvals")]
    state["effects"] = [
        {"idem_key": r["idem_key"].hex(), "tool": r["tool"], "effect_type": r["effect_type"], "status": r["status"],
         "total": r["total"], "partner_id": r["partner_id"]}
        for r in _rows(slot, "SELECT o.idem_key, k.tool, o.effect_type, o.status, o.payload->>'total' AS total, "
                             "o.payload->>'partner_id' AS partner_id "
                             "FROM outbox o JOIN idempotency_keys k ON k.key = o.idem_key ORDER BY o.id")]
    return state


def approvals_count(slot):
    """Approval requests of the slot (all statuses): the 'extra approvals' column."""
    return _rows(slot, "SELECT count(*) AS n FROM approvals WHERE kind = 'effect'")[0]["n"]


def outbound_open(slot):
    """Documents to the partner that are not closed yet: queued or sent and waiting for MDN or 997. The 997 of the
    gateway counts while it waits for its MDN, so the emulator has it when the state is read."""
    return _rows(slot, "SELECT count(*) AS n FROM outbound_docs WHERE state IN ('queued', 'sent')")[0]["n"]


def outbox_open(slot):
    """Outbox rows that still have work to do."""
    return _rows(slot, "SELECT count(*) AS n FROM outbox WHERE status IN ('pending', 'sent')")[0]["n"]
