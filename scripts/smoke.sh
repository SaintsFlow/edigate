#!/bin/sh
# Smoke test of the running stand (started by `make smoke` / `make up`).
# Creates a tenant with an interchange id, an agent, an operator and a partner,
# then checks: an agent gets an empty inbox; bad requests get the right errors;
# the audit log has the calls; a partner can send an 850 and gets an MDN; the
# agent reads that document without the partner's free text (it is listed as
# notes); the agent extracts a closed value from a note through the quarantined
# reader (a local model, skipped when Ollama is not reachable); the same
# interchange again gives the same MDN and no new rows; the same ISA13 with
# another body gives 409; a foreign AS2-From gives 403; the gateway log holds
# none of the partner's text. Wave 2-5 adds: fixtures load; the agent orders from the
# 850 (accepted, a txn_id), the same call gives the same answer, txn.status turns
# completed and mock-1c holds one order priced from the price list; a note reference
# in the arguments is refused (by the schema); the answers hold no business key,
# idempotency key or price of the 850. Wave 2-6 adds: an order above the limit waits as
# pending_approval, one operator approves it with edigatectl and it completes (y); an unknown
# ship-to gives policy_denied with ship_to.unknown (f2); a supplier invoice needs two different
# operators and reaches mock-1c once (x). Wave 2-7 adds scenario S2 (s2): order -> 855 ->
# reservation -> shipment -> 856 through partner-emulator, both documents delivered (MDN and
# a positive 997) and both sagas done, one 855 and one 856 at the partner with different ISA13
# (c); then a negative 997 (PARTNER_ACK_STATUS=R through /control) for a new order: the saga is an
# incident and no second 855 with a new ISA13 goes out (ch). Wave 2-8 adds: the key pair for the pins
# of upstream tools; a tool without a pin is off (ш0); evil-validator cannot be pinned, also with
# --accept-lint (ш1); rugpull-rates is pinned (ш2), three calls pass with the taint label (ш3), the
# fourth is manifest_drift and the audit row holds both hashes (ш4); no client token at the hostile
# servers (ш5); the price list; a letter to a contact of the partner arrives once and a letter to a
# contact of another partner is policy_denied with email.recipient (щ).
# Exits 0 only if every check matches. Safe to run again: principal ids carry a
# random suffix, ISA13 comes from the clock, and the EDI rows of the tenant
# "smoke" (stand data only) are cleared at the start so that step a still sees
# an empty inbox.
set -eu

BIN=bin
URL=http://127.0.0.1:18080/mcp
INBOUND=http://127.0.0.1:18080/edi/inbound
export EDIGATE_DB_URL='postgres://edigate:edigate@127.0.0.1:55432/edigate?sslmode=disable'
COMPOSE="docker compose -f deploy/compose/docker-compose.yml"
SUFFIX=$(date +%s)$$
AGENT_ID="smoke-agent-$SUFFIX"
OPERATOR_ID="smoke-operator-$SUFFIX"
OPERATOR2_ID="smoke-operator2-$SUFFIX"
PARTNER_ID="smoke-partner-$SUFFIX"
SUPPLIER_ID="smoke-supplier-$SUFFIX"
GATEWAY=ZZ:ZZTESTSUPPL
BUYER=ZZ:ZZTESTBUYER
VENDOR=ZZ:ZZTESTVENDOR
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fail() { echo "FAIL: $1" >&2; exit 1; }

# expect <label> <text> <fragment>: text must contain the fragment.
expect() {
	case "$2" in
	*"$3"*) echo "ok: $1" ;;
	*) fail "$1: expected '$3' in: $2" ;;
	esac
}

# sql <statement>...: runs statements in the stand's database, prints the plain result.
sql() {
	for statement in "$@"; do
		$COMPOSE exec -T postgres psql -U edigate -d edigate -v ON_ERROR_STOP=1 -Atq -c "$statement"
	done
}

# post <token or empty> <outfile> [extra curl args...]: prints the HTTP status.
post() {
	token=$1
	out=$2
	shift 2
	if [ -n "$token" ]; then
		set -- -H "Authorization: Bearer $token" "$@"
	fi
	curl -sS -o "$out" -w '%{http_code}' -X POST "$URL" \
		-H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
		"$@" --data "$CALL_BODY"
}

# edi850 <file> <ISA13> <order number> [quantity] [ship-to code]: writes an 850 of the smoke
# partner (quantity 10 and ship-to DC01 unless given). The PID05, MSG01 and N1-02 texts are
# what the agent must never see.
PID_TEXT="SMOKE SECRET WIDGET TEXT"
MSG_TEXT="Please call before delivery."
edi850() {
	{
		printf 'ISA*00*          *00*          *ZZ*ZZTESTBUYER    *ZZ*ZZTESTSUPPL    *261018*1200*U*00401*%s*0*T*>~' "$2"
		printf 'GS*PO*ZZTESTBUYER*ZZTESTSUPPL*20261018*1200*1*X*004010~ST*850*0001~BEG*00*SA*%s**20261018~' "$3"
		printf 'N1*BY*TEST BUYER*92*B001~N1*ST*TEST SHIP-TO*92*%s~PO1*1*%s*EA*5.25**UP*012345678905~' "${5:-DC01}" "${4:-10}"
		printf 'PID*F****%s~MSG*%s~CTT*1~SE*9*0001~GE*1*1~IEA*1*%s~' "$PID_TEXT" "$MSG_TEXT" "$2"
	} >"$1"
}

# edi810 <file> <ISA13> <invoice number> <order number>: writes an 810 of the smoke supplier
# for 10 pieces at 3.00 (total 30.00), the order SPO-SMK-1 of the fixtures.
edi810() {
	{
		printf 'ISA*00*          *00*          *ZZ*ZZTESTVENDOR   *ZZ*ZZTESTSUPPL    *261018*1200*U*00401*%s*0*T*>~' "$2"
		printf 'GS*IN*ZZTESTVENDOR*ZZTESTSUPPL*20261018*1200*1*X*004010~ST*810*0001~BIG*20261018*%s*20261018*%s~' "$3" "$4"
		printf 'CUR*SE*USD~N1*BY*TEST BUYER*92*B001~IT1*1*10*EA*3.00**UP*012345678905~TDS*3000~CTT*1~SE*8*0001~GE*1*1~IEA*1*%s~' "$2"
	} >"$1"
}

# send <file> [from]: posts the file as the smoke partner; prints "HTTP <code>" and the body.
send() {
	"$BIN/edigatectl" send --url "$INBOUND" --token "$PARTNER_TOKEN" --from "${2:-$BUYER}" --to "$GATEWAY" --file "$1"
}

CALL_BODY='{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"edi.list_inbox","arguments":{}}}'

# Wave 2-8: the pair of keys that signs the pins of upstream tools. The private half stays in
# deploy/compose/keys (not mounted into the gateway); the public half goes to the gateway through
# deploy/compose/.env, and the gateway is recreated only when it changed.
KEYS=deploy/compose/keys
mkdir -p "$KEYS"
[ -f "$KEYS/manifest.key" ] || "$BIN/edigatectl" manifests keygen --out "$KEYS/manifest.key" >"$KEYS/manifest.pub"
chmod 644 "$KEYS/manifest.key" # the stand's own key; the one-off container of pinctl runs as another user
printf 'EDIGATE_MANIFEST_PUBKEY=%s\n' "$(cat "$KEYS/manifest.pub")" >deploy/compose/.env
$COMPOSE up -d --wait edigate >/dev/null 2>&1 || fail "the gateway did not restart with the manifest key"

sql "DELETE FROM manifests" "DELETE FROM upstream_notes WHERE tenant_id = 'smoke'" \
	"DELETE FROM mock_partner.received WHERE kind = 'email' AND tenant_id = 'smoke'" >/dev/null
sql "DELETE FROM outbound_docs WHERE tenant_id = 'smoke'" \
	"DELETE FROM saga_steps WHERE saga_id IN (SELECT id FROM sagas WHERE tenant_id = 'smoke')" "DELETE FROM sagas WHERE tenant_id = 'smoke'" \
	"DELETE FROM control_numbers WHERE tenant_id = 'smoke'" \
	"DELETE FROM mock_wms.reservations WHERE tenant_id = 'smoke'" "DELETE FROM mock_wms.shipments WHERE tenant_id = 'smoke'" \
	"DELETE FROM mock_partner.received WHERE tenant_id = '$GATEWAY'" \
	"DELETE FROM approvals WHERE tenant_id = 'smoke'" "DELETE FROM daily_auto_totals WHERE tenant_id = 'smoke'" \
	"DELETE FROM outbox WHERE tenant_id = 'smoke'" "DELETE FROM idempotency_keys WHERE tenant_id = 'smoke'" \
	"DELETE FROM mock_onec.sales_orders WHERE tenant_id = 'smoke'" "DELETE FROM mock_onec.vendor_invoices WHERE tenant_id = 'smoke'" \
	"DELETE FROM quarantine_calls WHERE tenant_id = 'smoke'" "DELETE FROM edi_notes WHERE tenant_id = 'smoke'" \
	"DELETE FROM edi_documents WHERE tenant_id = 'smoke'" "DELETE FROM edi_interchanges WHERE tenant_id = 'smoke'" \
	"DELETE FROM edi_rejected WHERE tenant_id = 'smoke'" >/dev/null

"$BIN/edigatectl" tenant add smoke "Smoke tenant" --interchange-id "$GATEWAY" >/dev/null
"$BIN/edigatectl" partner add --tenant smoke --partner "$BUYER" --role customer --name "Smoke buyer" >/dev/null
# The price list of the stand: 7.50 for the SKU of the smoke 850, whose own price (PO104) is 5.25.
"$BIN/edigatectl" fixtures load scripts/testdata/smoke_fixtures.json >/dev/null
AGENT_TOKEN=$("$BIN/edigatectl" token issue --tenant smoke --kind agent --id "$AGENT_ID")
OPERATOR_TOKEN=$("$BIN/edigatectl" token issue --tenant smoke --kind operator --id "$OPERATOR_ID")
PARTNER_TOKEN=$("$BIN/edigatectl" token issue --tenant smoke --kind partner --id "$PARTNER_ID" --partner "$BUYER")
OPERATOR2_TOKEN=$("$BIN/edigatectl" token issue --tenant smoke --kind operator --id "$OPERATOR2_ID")
SUPPLIER_TOKEN=$("$BIN/edigatectl" token issue --tenant smoke --kind partner --id "$SUPPLIER_ID" --partner "$VENDOR")
# partner-emulator posts its 997 with the token of the buyer; it reads the file at every send.
STAND_TOKENS=deploy/compose/stand/partner_tokens.json
printf '{"%s":"%s"}\n' "$BUYER" "$PARTNER_TOKEN" >"$STAND_TOKENS"
chmod 644 "$STAND_TOKENS"

# (a) the agent gets an empty inbox
out=$("$BIN/edigatectl" call --url "$URL" --token "$AGENT_TOKEN" edi.list_inbox '{}')
[ "$out" = '{"documents":[]}' ] || fail "a: expected {\"documents\":[]}, got: $out"
echo "ok: a empty inbox"

# (b) no token: 401 unauthorized
code=$(post "" "$TMP/b")
[ "$code" = 401 ] || fail "b: expected HTTP 401, got $code"
expect "b unauthorized" "$(cat "$TMP/b")" '"reason":"unauthorized"'
expect "b code" "$(cat "$TMP/b")" '"code":-32030'

# (c) Mcp-Name differs from the body: 400 and code -32020
code=$(post "$AGENT_TOKEN" "$TMP/c" -H 'MCP-Protocol-Version: 2026-07-28' -H 'Mcp-Method: tools/call' -H 'Mcp-Name: edi.read_document')
[ "$code" = 400 ] || fail "c: expected HTTP 400, got $code"
expect "c header mismatch" "$(cat "$TMP/c")" '"code":-32020'

# (d) operator token on /mcp: 403 forbidden
code=$(post "$OPERATOR_TOKEN" "$TMP/d")
[ "$code" = 403 ] || fail "d: expected HTTP 403, got $code"
expect "d forbidden" "$(cat "$TMP/d")" '"reason":"forbidden"'
expect "d code" "$(cat "$TMP/d")" '"code":-32031'

# (e) input out of range: exit code 1 and schema_invalid
rc=0
out=$("$BIN/edigatectl" call --url "$URL" --token "$AGENT_TOKEN" edi.list_inbox '{"limit":1000}') || rc=$?
[ "$rc" = 1 ] || fail "e: expected exit code 1, got $rc"
expect "e schema_invalid" "$out" '"reason":"schema_invalid"'

# (f) audit rows of this run
rows=$(sql "SELECT outcome FROM audit_log WHERE tenant_id = 'smoke' AND principal_id = '$AGENT_ID' AND event_type = 'tool_call' ORDER BY seq")
expect "f audit ok" "$rows" "ok"
expect "f audit schema_invalid" "$rows" "schema_invalid"

# (g) the partner sends an 850 with a new ISA13: 200 and an MDN that says processed
ISA13=$(printf '%09d' $(($(date +%s) % 1000000000)))
ORDER="SMK$ISA13"
edi850 "$TMP/first.edi" "$ISA13" "$ORDER"
first=$(send "$TMP/first.edi") || fail "g: send failed: $first"
expect "g HTTP 200" "$first" "HTTP 200"
expect "g MDN processed" "$first" '"disposition":"processed"}'

# (h) the agent lists the document and reads typed fields; the PID05 text never appears
out=$("$BIN/edigatectl" call --url "$URL" --token "$AGENT_TOKEN" edi.list_inbox '{}')
expect "h inbox shows the order" "$out" "\"business_ref\":\"$ORDER\""
DOC_ID=$(printf '%s' "$out" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
[ -n "$DOC_ID" ] || fail "h: no document id in: $out"
doc=$("$BIN/edigatectl" call --url "$URL" --token "$AGENT_TOKEN" edi.read_document "{\"document_id\":\"$DOC_ID\"}")
expect "h business_ref" "$doc" "\"business_ref\":\"$ORDER\""
expect "h lines" "$doc" '"line_no":"1"'
case "$doc" in
*"$PID_TEXT"*) fail "h: the PID05 text reached the agent: $doc" ;;
*) echo "ok: h no free text in the answer" ;;
esac
case "$doc$out" in
*WIDGET*) fail "h: part of the PID05 text reached the agent" ;;
esac

# (m) the free text is listed as notes (reference and place, no text); the texts
# of PID05, MSG01 and N1-02 are not in the answer
expect "m notes" "$doc" '"notes":[{"reason":"free_text","ref":"$note_'
expect "m note place" "$doc" '"segment_path":"PO1[1]/MSG01"'
case "$doc$out" in
*"$MSG_TEXT"* | *"call before delivery"* | *"TEST BUYER"* | *"TEST SHIP-TO"*) fail "m: a free text reached the agent: $doc" ;;
*) echo "ok: m no PID05, MSG01 or N1-02 text" ;;
esac

# (n) the agent extracts a closed value from the MSG01 note through the
# quarantined reader; skipped when Ollama is not reachable
NOTE_REF=$(printf '%s' "$doc" | sed -n 's/.*"ref":"\([^"]*\)","segment_path":"PO1\[1\]\/MSG01".*/\1/p')
[ -n "$NOTE_REF" ] || fail "n: no reference of the MSG01 note in: $doc"
if curl -s -m 3 http://127.0.0.1:11434/api/tags >/dev/null 2>&1; then
	rc=0
	ext=$("$BIN/edigatectl" call --url "$URL" --token "$AGENT_TOKEN" edi.extract_note \
		"{\"document_id\":\"$DOC_ID\",\"ref\":\"$NOTE_REF\",\"schema_id\":\"delivery_instruction\"}") || rc=$?
	[ "$rc" = 0 ] || fail "n: edi.extract_note exited with $rc: $ext"
	expect "n declassified" "$ext" '"declassified":true'
	expect "n schema" "$ext" '"schema_id":"delivery_instruction"'
	case "$ext" in
	*"$MSG_TEXT"*) fail "n: the note text came back: $ext" ;;
	esac
	n=$(sql "SELECT count(*) FROM quarantine_calls WHERE tenant_id = 'smoke' AND outcome = 'ok' AND prompt_tokens > 0")
	[ "$n" = 1 ] || fail "n: expected one cost record with tokens, got $n"
	echo "ok: n cost record"
else
	echo "skip: n (ollama not reachable)"
fi

# (p) the agent creates an order from the 850: accepted and a txn_id
AGENT_OUT="$TMP/agent_answers"
call_agent() { "$BIN/edigatectl" call --url "$URL" --token "$AGENT_TOKEN" "$@"; }
ordered=$(call_agent erp.create_sales_order "{\"source_document_id\":\"$DOC_ID\"}") || fail "p: erp.create_sales_order failed: $ordered"
echo "$ordered" >>"$AGENT_OUT"
expect "p accepted" "$ordered" '"status":"accepted"'
TXN_ID=$(printf '%s' "$ordered" | sed -n 's/.*"txn_id":"\([^"]*\)".*/\1/p')
[ -n "$TXN_ID" ] || fail "p: no txn_id in: $ordered"

# (r) the same call again: the same answer byte for byte, no second key and no second outbox row
again_order=$(call_agent erp.create_sales_order "{\"source_document_id\":\"$DOC_ID\"}") || fail "r: the repeat failed: $again_order"
echo "$again_order" >>"$AGENT_OUT"
[ "$again_order" = "$ordered" ] || fail "r: the answer differs: $ordered / $again_order"
n=$(sql "SELECT count(*) FROM idempotency_keys WHERE tenant_id = 'smoke'")
[ "$n" = 1 ] || fail "r: expected one key, got $n"
n=$(sql "SELECT count(*) FROM outbox WHERE tenant_id = 'smoke'")
[ "$n" = 1 ] || fail "r: expected one outbox row, got $n"
echo "ok: r same txn_id, one key, one outbox row"

# (s) a few seconds later the relay has delivered it: txn.status is completed with an external
# reference; mock-1c holds one order with this business reference and the prices of the price
# list (7.50), not the price of the 850 (PO104 = 5.25)
waited=0
while :; do
	status=$(call_agent txn.status "{\"txn_id\":\"$TXN_ID\"}") || fail "s: txn.status failed: $status"
	case "$status" in *'"status":"completed"'*) break ;; esac
	waited=$((waited + 1))
	[ "$waited" -le 40 ] || fail "s: the order was not delivered in 20 s: $status"
	sleep 0.5
done
echo "$status" >>"$AGENT_OUT"
expect "s completed" "$status" '"status":"completed"'
case "$status" in *'"external_ref":null'* | *'"external_ref":""'*) fail "s: no external reference: $status" ;; esac
expect "s external_ref" "$status" '"external_ref":"'
KEY_HEX=$(sql "SELECT encode(key, 'hex') FROM idempotency_keys WHERE tenant_id = 'smoke'")
row=$(sql "SELECT total::text || '|' || (lines->0->>'unit_price') || '|' || external_id FROM mock_onec.sales_orders WHERE tenant_id = 'smoke' AND business_ref = '$ORDER'")
[ "$row" = "75.00|7.50|$KEY_HEX" ] || fail "s: mock-1c row (total|first price|external id): $row"
n=$(sql "SELECT count(*) FROM mock_onec.sales_orders WHERE tenant_id = 'smoke' AND business_ref = '$ORDER'")
[ "$n" = 1 ] || fail "s: expected one order in mock-1c, got $n"
n=$(sql "SELECT count(*) FROM mock_onec.sales_orders WHERE tenant_id = 'smoke' AND lines::text LIKE '%5.25%'")
[ "$n" = 0 ] || fail "s: the price of the 850 reached mock-1c"
echo "ok: s delivered with the prices of the price list"

# (t) a note reference in delivery_instruction. The input schema holds an enum, so the schema check
# refuses it first and the answer is schema_invalid, not tainted_sink: the taint check cannot be
# reached through this tool, and the argument that could carry a reference (source_document_id)
# is a uuid. The taint check itself is covered by TestT_TAINT_01 (a unit test with the
# schema check switched off). Here we check that the refusal is clean: exit 1, no new key.
rc=0
refused=$(call_agent erp.create_sales_order '{"source_document_id":"'"$DOC_ID"'","delivery_instruction":"$note_1"}') || rc=$?
[ "$rc" = 1 ] || fail "t: expected exit code 1, got $rc"
expect "t schema_invalid (the enum refuses it before the taint check)" "$refused" '"reason":"schema_invalid"'
rc=0
refused=$(call_agent erp.create_sales_order '{"source_document_id":"$note_1"}') || rc=$?
[ "$rc" = 1 ] || fail "t: expected exit code 1, got $rc"
expect "t schema_invalid for a reference as the document" "$refused" '"reason":"schema_invalid"'
n=$(sql "SELECT count(*) FROM idempotency_keys WHERE tenant_id = 'smoke'")
[ "$n" = 1 ] || fail "t: a refused call left a key ($n keys)"

# (q) what the agent saw: no business key, no price of the 850, no idempotency key
for leak in bk_ref "$KEY_HEX" "5.25" "$ORDER"; do
	if grep -q -- "$leak" "$AGENT_OUT"; then
		fail "q: '$leak' is in the answers the agent got: $(cat "$AGENT_OUT")"
	fi
done
echo "ok: q no bk_ref, key, price of the 850 or business reference in the answers"

# (u) the operator entrance /ops/ through edigatectl: a key that is not failed gives 409, an
# unknown key 404, an agent token 403 (a failed key is reopened in TestT_IDEM_05)
rc=0
out=$("$BIN/edigatectl" txn reopen --url http://127.0.0.1:18080 --token "$OPERATOR_TOKEN" "$KEY_HEX") || rc=$?
[ "$rc" = 1 ] || fail "u: expected exit code 1, got $rc"
expect "u HTTP 409" "$out" "HTTP 409"
expect "u key is not failed" "$out" "key is not failed"
rc=0
out=$("$BIN/edigatectl" txn reopen --url http://127.0.0.1:18080 --token "$OPERATOR_TOKEN" 0000000000000000000000000000000000000000000000000000000000000000) || rc=$?
[ "$rc" = 1 ] || fail "u: expected exit code 1, got $rc"
expect "u HTTP 404" "$out" "HTTP 404"
expect "u not_found" "$out" '"reason":"not_found"'
rc=0
out=$("$BIN/edigatectl" txn reopen --url http://127.0.0.1:18080 --token "$AGENT_TOKEN" "$KEY_HEX") || rc=$?
[ "$rc" = 1 ] || fail "u: expected exit code 1, got $rc"
expect "u HTTP 403" "$out" "HTTP 403"

# (i) the same interchange again: 200, the same MDN byte for byte, one row
sleep 1
again=$(send "$TMP/first.edi") || fail "i: send failed: $again"
[ "$again" = "$first" ] || fail "i: the MDN differs: $first / $again"
echo "ok: i same MDN"
n=$(sql "SELECT count(*) FROM edi_interchanges WHERE tenant_id = 'smoke' AND isa13 = '$ISA13'")
[ "$n" = 1 ] || fail "i: expected one interchange row, got $n"
echo "ok: i one row"

# (j) the same ISA13 with another body: 409
sleep 1
edi850 "$TMP/other.edi" "$ISA13" "SMKOTHER"
rc=0
out=$(send "$TMP/other.edi") || rc=$?
[ "$rc" = 1 ] || fail "j: expected exit code 1, got $rc"
expect "j HTTP 409" "$out" "HTTP 409"
n=$(sql "SELECT count(*) FROM edi_rejected WHERE tenant_id = 'smoke' AND reason = 'isa13_conflict'")
[ "$n" = 1 ] || fail "j: expected one conflict row, got $n"

# (k) a partner token with the AS2-From of another partner: 403
sleep 1
rc=0
out=$(send "$TMP/first.edi" ZZ:ZZTESTOTHER) || rc=$?
[ "$rc" = 1 ] || fail "k: expected exit code 1, got $rc"
expect "k HTTP 403" "$out" "HTTP 403"
expect "k forbidden" "$out" '"reason":"forbidden"'

# --- wave 2-6: policies, approval requests and the rule of four eyes ---
OPS_URL=http://127.0.0.1:18080

# send_as <token> <from> <file>: posts a file as another partner.
send_as() {
	"$BIN/edigatectl" send --url "$INBOUND" --token "$1" --from "$2" --to "$GATEWAY" --file "$3"
}

# wait_status <txn id> <status>: waits up to 20 s for txn.status to show the status.
wait_status() {
	waited=0
	while :; do
		cur=$(call_agent txn.status "{\"txn_id\":\"$1\"}") || fail "wait: txn.status failed: $cur"
		case "$cur" in *"\"status\":\"$2\""*) return 0 ;; esac
		waited=$((waited + 1))
		[ "$waited" -le 40 ] || fail "wait: expected $2 within 20 s, got: $cur"
		sleep 0.5
	done
}

# (y) an order above auto_max_amount (1000 pieces at 7.50 = 7500.00 against 5000.00) is not
# refused: it waits as pending_approval. The operator sees it with the rule name and no
# partner text; one approve through edigatectl and a few seconds later it is completed, with
# one order in mock-1c.
sleep 1
BIG_ISA=$(printf '%09d' $((($(date +%s) + 7) % 1000000000)))
BIG_REF="SMKBIG$BIG_ISA"
edi850 "$TMP/big.edi" "$BIG_ISA" "$BIG_REF" 1000
out=$(send "$TMP/big.edi") || fail "y: send failed: $out"
expect "y HTTP 200" "$out" "HTTP 200"
BIG_DOC=$(sql "SELECT id FROM edi_documents WHERE tenant_id = 'smoke' AND bk_ref = '$BIG_REF'")
[ -n "$BIG_DOC" ] || fail "y: the large order was not stored"
pending=$(call_agent erp.create_sales_order "{\"source_document_id\":\"$BIG_DOC\"}") || fail "y: erp.create_sales_order failed: $pending"
expect "y pending_approval" "$pending" '"status":"pending_approval"'
APPROVAL_ID=$(printf '%s' "$pending" | sed -n 's/.*"approval_id":"\([^"]*\)".*/\1/p')
BIG_TXN=$(printf '%s' "$pending" | sed -n 's/.*"txn_id":"\([^"]*\)".*/\1/p')
[ -n "$APPROVAL_ID" ] && [ -n "$BIG_TXN" ] || fail "y: no approval_id or txn_id in: $pending"
n=$(sql "SELECT count(*) FROM outbox o JOIN idempotency_keys k ON k.key = o.idem_key WHERE k.txn_id = '$BIG_TXN'")
[ "$n" = 0 ] || fail "y: a waiting request has $n outbox rows"
listed=$("$BIN/edigatectl" approvals list --url "$OPS_URL" --token "$OPERATOR_TOKEN") || fail "y: approvals list failed: $listed"
expect "y list shows the request" "$listed" "$APPROVAL_ID"
expect "y list shows the rule" "$listed" '"rules":["limit.document"]'
case "$listed" in
*"$PID_TEXT"* | *"$MSG_TEXT"* | *"call before delivery"* | *"TEST BUYER"* | *"TEST SHIP-TO"*) fail "y: partner text in the list for the operator: $listed" ;;
*) echo "ok: y no partner text in the list" ;;
esac
decided=$("$BIN/edigatectl" approvals approve --url "$OPS_URL" --token "$OPERATOR_TOKEN" "$APPROVAL_ID") || fail "y: approve failed: $decided"
expect "y approved" "$decided" '"status":"approved"'
wait_status "$BIG_TXN" completed
n=$(sql "SELECT count(*) FROM mock_onec.sales_orders WHERE tenant_id = 'smoke' AND business_ref = '$BIG_REF'")
[ "$n" = 1 ] || fail "y: expected one order in mock-1c, got $n"
echo "ok: y approved by an operator, completed, one order in mock-1c"

# (f2) a ship-to that is not one of the partner's codes (DC99) is refused with policy_denied
# and the rule name, and no key is kept
sleep 1
DC_ISA=$(printf '%09d' $((($(date +%s) + 11) % 1000000000)))
DC_REF="SMKDC$DC_ISA"
edi850 "$TMP/dc99.edi" "$DC_ISA" "$DC_REF" 10 DC99
out=$(send "$TMP/dc99.edi") || fail "f2: send failed: $out"
DC_DOC=$(sql "SELECT id FROM edi_documents WHERE tenant_id = 'smoke' AND bk_ref = '$DC_REF'")
[ -n "$DC_DOC" ] || fail "f2: the order was not stored"
rc=0
denied=$(call_agent erp.create_sales_order "{\"source_document_id\":\"$DC_DOC\"}") || rc=$?
[ "$rc" = 1 ] || fail "f2: expected exit code 1, got $rc: $denied"
expect "f2 policy_denied" "$denied" '"reason":"policy_denied"'
expect "f2 rule name" "$denied" '"explain":["ship_to.unknown"]'
n=$(sql "SELECT count(*) FROM idempotency_keys WHERE tenant_id = 'smoke' AND business_key->>1 = '$DC_REF'")
[ "$n" = 0 ] || fail "f2: a refused order left $n keys"
rule=$(sql "SELECT policy_explain->'deny'->>0 FROM audit_log WHERE tenant_id = 'smoke' AND event_type = 'policy_denied' ORDER BY seq DESC LIMIT 1")
[ "$rule" = "ship_to.unknown" ] || fail "f2: the audit row holds '$rule'"
echo "ok: f2 audit row with the decision"

# (x) a supplier invoice that matches the order SPO-SMK-1 always waits for two operators:
# one approve leaves it pending (and the same operator again gets 409), the second operator
# completes it, and mock-1c holds one invoice
sleep 1
INV_ISA=$(printf '%09d' $((($(date +%s) + 13) % 1000000000)))
INV_NO="SMKINV$INV_ISA"
edi810 "$TMP/inv.edi" "$INV_ISA" "$INV_NO" SPO-SMK-1
out=$(send_as "$SUPPLIER_TOKEN" "$VENDOR" "$TMP/inv.edi") || fail "x: send failed: $out"
expect "x HTTP 200" "$out" "HTTP 200"
INV_DOC=$(sql "SELECT id FROM edi_documents WHERE tenant_id = 'smoke' AND st01 = '810' AND bk_ref = '$INV_NO'")
[ -n "$INV_DOC" ] || fail "x: the invoice was not stored"
inv=$(call_agent erp.post_vendor_invoice "{\"source_document_id\":\"$INV_DOC\"}") || fail "x: erp.post_vendor_invoice failed: $inv"
expect "x pending_approval" "$inv" '"status":"pending_approval"'
INV_APPROVAL=$(printf '%s' "$inv" | sed -n 's/.*"approval_id":"\([^"]*\)".*/\1/p')
INV_TXN=$(printf '%s' "$inv" | sed -n 's/.*"txn_id":"\([^"]*\)".*/\1/p')
[ -n "$INV_APPROVAL" ] && [ -n "$INV_TXN" ] || fail "x: no approval_id or txn_id in: $inv"
first_ok=$("$BIN/edigatectl" approvals approve --url "$OPS_URL" --token "$OPERATOR_TOKEN" "$INV_APPROVAL") || fail "x: the first approve failed: $first_ok"
expect "x still pending after one operator" "$first_ok" '"status":"pending"'
expect "x one of two" "$first_ok" '"approvals":1'
rc=0
twice=$("$BIN/edigatectl" approvals approve --url "$OPS_URL" --token "$OPERATOR_TOKEN" "$INV_APPROVAL") || rc=$?
[ "$rc" = 1 ] || fail "x: expected exit code 1 for the same operator, got $rc"
expect "x HTTP 409" "$twice" "HTTP 409"
expect "x same operator" "$twice" "already decided by this operator"
n=$(sql "SELECT count(*) FROM mock_onec.vendor_invoices WHERE tenant_id = 'smoke'")
[ "$n" = 0 ] || fail "x: the invoice reached mock-1c after one operator"
second_ok=$("$BIN/edigatectl" approvals approve --url "$OPS_URL" --token "$OPERATOR2_TOKEN" "$INV_APPROVAL") || fail "x: the second approve failed: $second_ok"
expect "x approved by two" "$second_ok" '"status":"approved"'
wait_status "$INV_TXN" completed
row=$(sql "SELECT invoice_number || '|' || order_ref || '|' || total::text FROM mock_onec.vendor_invoices WHERE tenant_id = 'smoke'")
[ "$row" = "$INV_NO|SPO-SMK-1|30.00" ] || fail "x: invoice in mock-1c (number|order|total): $row"
echo "ok: x invoice approved by two operators, one invoice in mock-1c"

# --- wave 2-7: scenario S2, the order through to the 856, with MDN and 997 ---
PARTNER_URL=http://127.0.0.1:18083
curl -fsS -X POST "$PARTNER_URL/control" -d '{"ack_status":"A","ack_delay_ms":0,"mdn_delay_ms":0,"mdn_drop":0}' >/dev/null ||
	fail "s2: partner-emulator does not answer on $PARTNER_URL"

# wait_sql <label> <query> <expected>: waits up to 40 s for the query to give the value.
wait_sql() {
	waited=0
	while :; do
		got=$(sql "$2")
		[ "$got" = "$3" ] && return 0
		waited=$((waited + 1))
		[ "$waited" -le 80 ] || fail "$1: expected '$3', got '$got'"
		sleep 0.5
	done
}

# receive_order <ISA13 offset> <order number>: the partner sends an 850 (10 pieces, 75.00 by the
# price list); S2_DOC is the id of the stored document.
receive_order() {
	sleep 1
	isa=$(printf '%09d' $((($(date +%s) + $1) % 1000000000)))
	edi850 "$TMP/s2-$2.edi" "$isa" "$2"
	out=$(send "$TMP/s2-$2.edi") || fail "s2: send failed: $out"
	S2_DOC=$(sql "SELECT id FROM edi_documents WHERE tenant_id = 'smoke' AND bk_ref = '$2'")
	[ -n "$S2_DOC" ] || fail "s2: the order $2 was not stored"
}

# approve_if_pending <answer of a tool>: a document that the forecast sends to an operator is approved.
approve_if_pending() {
	case "$1" in
	*'"status":"pending_approval"'*)
		id=$(printf '%s' "$1" | sed -n 's/.*"approval_id":"\([^"]*\)".*/\1/p')
		"$BIN/edigatectl" approvals approve --url "$OPS_URL" --token "$OPERATOR_TOKEN" "$id" >/dev/null || fail "s2: approve failed"
		;;
	esac
}

# order_of <order number>: creates the order in the ERP and waits for it; S2_TXN is its txn_id.
order_of() {
	made=$(call_agent erp.create_sales_order "{\"source_document_id\":\"$S2_DOC\"}") || fail "s2: erp.create_sales_order failed: $made"
	S2_TXN=$(printf '%s' "$made" | sed -n 's/.*"txn_id":"\([^"]*\)".*/\1/p')
	[ -n "$S2_TXN" ] || fail "s2: no txn_id in: $made"
	wait_status "$S2_TXN" completed
}

DOC_STATE="SELECT string_agg(d.state || '|' || d.mdn_ok || '|' || COALESCE(d.ack_status, '-'), ',' ORDER BY d.st01)
	FROM outbound_docs d JOIN sagas g ON g.id = d.saga_id WHERE g.tenant_id = 'smoke' AND g.business_key->>1"

# (c) 850 -> order -> 855 AC -> reservation -> shipment -> 856; both sagas done
S2_REF="SMKS2$(date +%s)"
receive_order 17 "$S2_REF"
order_of
ans=$(call_agent erp.confirm_order "{\"sales_order_txn_id\":\"$S2_TXN\",\"ack_type\":\"AC\"}") || fail "s2: erp.confirm_order failed: $ans"
approve_if_pending "$ans"
wait_sql "s2: the 855 is delivered" "$DOC_STATE = '$S2_REF' AND d.st01 = '855'" "delivered|true|A"
wait_sql "s2: the order saga is done" "SELECT state FROM sagas WHERE tenant_id = 'smoke' AND kind = 'order' AND business_key->>1 = '$S2_REF'" done
wait_status "$S2_TXN" completed
case "$(call_agent txn.status "{\"txn_id\":\"$S2_TXN\"}")" in
*'"saga_state":"done"'*) echo "ok: s2 txn.status shows saga_state done" ;;
*) fail "s2: txn.status of the order does not show saga_state done" ;;
esac
ans=$(call_agent wms.reserve "{\"sales_order_txn_id\":\"$S2_TXN\"}") || fail "s2: wms.reserve failed: $ans"
RESERVE_TXN=$(printf '%s' "$ans" | sed -n 's/.*"txn_id":"\([^"]*\)".*/\1/p')
wait_status "$RESERVE_TXN" completed
ans=$(call_agent edi.send_asn "{\"sales_order_txn_id\":\"$S2_TXN\"}") || fail "s2: edi.send_asn failed: $ans"
approve_if_pending "$ans"
wait_sql "s2: the 856 is delivered" "$DOC_STATE = '$S2_REF' AND d.st01 = '856'" "delivered|true|A"
wait_sql "s2: both sagas are done" "SELECT string_agg(state, ',' ORDER BY kind) FROM sagas WHERE tenant_id = 'smoke' AND business_key->>1 = '$S2_REF'" "done,done"
got=$(sql "SELECT string_agg(st01 || ':' || n || ':' || ni, ',' ORDER BY st01) FROM
	(SELECT st01, count(*) AS n, count(DISTINCT isa13) AS ni FROM mock_partner.received WHERE business_ref = '$S2_REF' GROUP BY st01) t")
[ "$got" = "855:1:1,856:1:1" ] || fail "s2: transmissions and interchanges at the partner (set:count:different ISA13): $got"
n=$(sql "SELECT count(DISTINCT isa13) FROM mock_partner.received WHERE business_ref = '$S2_REF'")
[ "$n" = 2 ] || fail "s2: the 855 and the 856 must have different ISA13, got $n"
n=$(sql "SELECT count(*) FROM mock_wms.shipments WHERE tenant_id = 'smoke' AND order_ref = '$S2_REF' AND cardinality(sscc) = 1 AND length(sscc[1]) = 18")
[ "$n" = 1 ] || fail "s2: expected one shipment with an SSCC of 18 digits, got $n"
n=$(sql "SELECT count(*) FROM mock_partner.received r JOIN mock_wms.shipments s ON s.order_ref = r.business_ref
	WHERE r.st01 = '856' AND r.business_ref = '$S2_REF' AND position('MAN*GM*' || s.sscc[1] || '~' in convert_from(r.body, 'UTF8')) > 0")
[ "$n" = 1 ] || fail "s2: the 856 does not carry the SSCC of the warehouse"
echo "ok: s2 (c) order, 855, reservation, shipment, 856: delivered with MDN and 997, both sagas done"

# (ch) a negative 997 for the next order: the saga is an incident and the 855 is not sent again
curl -fsS -X POST "$PARTNER_URL/control" -d '{"ack_status":"R"}' >/dev/null || fail "s2: control failed"
S2R_REF="SMKS2R$(date +%s)"
receive_order 19 "$S2R_REF"
order_of
ans=$(call_agent erp.confirm_order "{\"sales_order_txn_id\":\"$S2_TXN\",\"ack_type\":\"AC\"}") || fail "s2: erp.confirm_order failed: $ans"
approve_if_pending "$ans"
wait_sql "s2: the order saga is an incident" "SELECT state FROM sagas WHERE tenant_id = 'smoke' AND kind = 'order' AND business_key->>1 = '$S2R_REF'" incident
curl -fsS -X POST "$PARTNER_URL/control" -d '{"ack_status":"A"}' >/dev/null || fail "s2: control failed"
sleep 3
got=$(sql "SELECT count(*) || ':' || count(DISTINCT isa13) FROM mock_partner.received WHERE business_ref = '$S2R_REF'")
[ "$got" = "1:1" ] || fail "s2: after a negative 997 the partner got (transmissions:different ISA13) $got, want 1:1"
got=$(sql "$DOC_STATE = '$S2R_REF'")
[ "$got" = "sent|true|R" ] || [ "$got" = "incident|true|R" ] || fail "s2: document after a negative 997: $got"
got=$(sql "SELECT incident_reason FROM outbound_docs d JOIN sagas g ON g.id = d.saga_id WHERE g.business_key->>1 = '$S2R_REF'")
[ "$got" = ack_rejected ] || fail "s2: incident reason: $got"
n=$(sql "SELECT count(*) FROM approvals WHERE tenant_id = 'smoke' AND kind = 'review' AND rules::text = '[\"ack.rejected\"]'")
[ "$n" = 1 ] || fail "s2: expected one review request ack.rejected, got $n"
case "$(call_agent txn.status "{\"txn_id\":\"$S2_TXN\"}")" in
*'"saga_state":"incident"'*) ;;
*) fail "s2: txn.status does not show saga_state incident" ;;
esac
echo "ok: s2 (ch) a negative 997: incident, a review request, no second 855"

# --- wave 2-8: upstream servers (pins, a description that changes), the price list, the letters ---

# pinctl <edigatectl arguments>: edigatectl inside the stand network (the hostile servers have no
# published port). It gets the signing key as a volume of this one container only.
pinctl() {
	$COMPOSE run --rm --no-deps -T -v "$PWD/$KEYS:/keys:ro" --entrypoint /edigatectl edigate "$@"
}
PIN_ARGS="--config /etc/edigate/edigate.yaml --signing-key /keys/manifest.key"

# tools_list: the names of the tools the agent sees now.
tools_list() {
	CALL_BODY='{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'
	code=$(post "$AGENT_TOKEN" "$TMP/list") || fail "tools/list failed"
	[ "$code" = 200 ] || fail "tools/list: HTTP $code"
	cat "$TMP/list"
}

# Fresh hostile servers: the counter of rugpull-rates starts again and its description is clean.
$COMPOSE up -d --force-recreate --wait evil-validator shadow-carrier rugpull-rates >/dev/null 2>&1 || fail "ш: the hostile servers did not start"
sql "DELETE FROM manifests" >/dev/null

# (ш0) a tool without a pin is off: the agent does not see it
case "$(tools_list)" in
*'up.rugpull-rates'* | *'up.evil-validator'* | *'up.shadow-carrier'*) fail "ш0: a tool without a pin is listed" ;;
*) echo "ok: ш0 tools without a pin are off" ;;
esac

# (ш1) the poisoned description is not pinned, also with --accept-lint
rc=0
out=$(pinctl manifests pin $PIN_ARGS --server evil-validator 2>&1) || rc=$?
[ "$rc" = 1 ] || fail "ш1: expected exit code 1, got $rc: $out"
expect "ш1 finding" "$out" 'tool_reference: edi.send_asn'
rc=0
out=$(pinctl manifests pin $PIN_ARGS --server evil-validator --accept-lint 2>&1) || rc=$?
[ "$rc" = 1 ] || fail "ш1: --accept-lint: expected exit code 1, got $rc: $out"
n=$(sql "SELECT count(*) FROM manifests WHERE server = 'evil-validator'")
[ "$n" = 0 ] || fail "ш1: a refused pin left $n rows"
echo "ok: ш1 evil-validator is not pinned, also with --accept-lint"

# (ш2) rugpull-rates is pinned and its tool is listed
out=$(pinctl manifests pin $PIN_ARGS --server rugpull-rates 2>&1) || fail "ш2: pin failed: $out"
expect "ш2 pinned" "$out" 'pinned rugpull-rates.get_rates sha256='
expect "ш2 listed" "$(tools_list)" '"name":"up.rugpull-rates.get_rates"'

# (ш3) three calls through the gateway pass, with the label of the upstream in _meta
CALL_BODY='{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"up.rugpull-rates.get_rates","arguments":{"city":"Almaty"}}}'
code=$(post "$AGENT_TOKEN" "$TMP/rate1") || fail "ш3: curl failed"
[ "$code" = 200 ] || fail "ш3: HTTP $code: $(cat "$TMP/rate1")"
expect "ш3 taint label" "$(cat "$TMP/rate1")" '"edigate/taint":"upstream:rugpull-rates"'
for i in 2 3; do
	out=$(call_agent up.rugpull-rates.get_rates '{"city":"Almaty"}') || fail "ш3: call $i failed: $out"
	expect "ш3 call $i" "$out" '"rate":"12.50"'
done
n=$(sql "SELECT count(*) FROM audit_log WHERE principal_id = '$AGENT_ID' AND event_type = 'upstream_result' AND tool = 'up.rugpull-rates.get_rates' AND detail->>'server' = 'rugpull-rates'")
[ "$n" = 3 ] || fail "ш3: expected 3 upstream_result rows, got $n"

# (ш4) after the third call the server changed its description: the fourth call is manifest_drift,
# the tool is not listed, and the audit row holds both hashes
rc=0
out=$(call_agent up.rugpull-rates.get_rates '{"city":"Almaty"}') || rc=$?
[ "$rc" = 1 ] || fail "ш4: expected exit code 1, got $rc: $out"
expect "ш4 manifest_drift" "$out" '"reason":"manifest_drift"'
case "$(tools_list)" in
*'up.rugpull-rates.get_rates'*) fail "ш4: the tool is still listed" ;;
*) echo "ok: ш4 the tool is off after its description changed" ;;
esac
row=$(sql "SELECT detail->>'pinned_sha256' = (SELECT encode(sha256, 'hex') FROM manifests WHERE server = 'rugpull-rates')
	AND length(detail->>'live_sha256') = 64 AND detail->>'live_sha256' <> detail->>'pinned_sha256'
	FROM audit_log WHERE principal_id = '$AGENT_ID' AND event_type = 'manifest_drift' AND tool = 'up.rugpull-rates.get_rates' AND detail->>'state' = 'changed'
	ORDER BY seq DESC LIMIT 1")
[ "$row" = t ] || fail "ш4: the manifest_drift row does not hold the pinned and the live hash: '$row'"
echo "ok: ш4 manifest_drift after three calls, audit row with both hashes"

# (ш5) the logs of the hostile servers show no token of the client of the gateway
for s in evil-validator shadow-carrier rugpull-rates; do
	$COMPOSE logs --no-color "$s" | grep -q '"event":"request"' || fail "ш5: $s logged no request"
	if $COMPOSE logs --no-color "$s" | grep -q '"client_token_present":true'; then
		fail "ш5: the token of a client reached $s"
	fi
	if $COMPOSE logs --no-color "$s" | grep -q "$AGENT_TOKEN"; then
		fail "ш5: the agent token is in the log of $s"
	fi
done
echo "ok: ш5 no client token at the hostile servers"

# (price) the price list: the tenant's own prices, a SKU that is not there is left out
out=$(call_agent erp.get_price_list '{"sku":["012345678905","NO-SUCH-SKU"]}') || fail "price: erp.get_price_list failed: $out"
expect "price row" "$out" '"unit_price":"7.50"'
case "$out" in *NO-SUCH-SKU*) fail "price: a SKU that is not in the list is in the answer: $out" ;; esac
n=$(sql "SELECT count(*) FROM audit_log WHERE principal_id = '$AGENT_ID' AND event_type = 'price_list_read' AND detail->>'sku_count' = '2'")
[ "$n" = 1 ] || fail "price: expected one price_list_read row, got $n"
rc=0
out=$(call_agent erp.get_price_list "{\"sku\":[$(i=0; while [ "$i" -lt 51 ]; do printf '"S%s",' "$i"; i=$((i + 1)); done)\"S51\"]}") || rc=$?
[ "$rc" = 1 ] || fail "price: 52 SKUs: expected exit code 1, got $rc"
expect "price schema_invalid" "$out" '"reason":"schema_invalid"'
echo "ok: price list of the tenant, at most 50 SKUs"

# (щ) a letter to a contact of the partner of the document reaches the mail service once; a
# contact of another partner is refused with email.recipient
receive_order 23 "SMKMAIL$(date +%s)"
MAIL_DOC=$S2_DOC
MAIL_REF=$(sql "SELECT bk_ref FROM edi_documents WHERE id = '$MAIL_DOC'")
out=$(call_agent notify.send_email "{\"source_document_id\":\"$MAIL_DOC\",\"to\":\"orders@buyer.test\",\"template_id\":\"tpl_order_received\"}") || fail "щ: notify.send_email failed: $out"
expect "щ accepted" "$out" '"status":"accepted"'
wait_sql "щ: the letter reached the mail service" \
	"SELECT count(*) FROM mock_partner.received WHERE kind = 'email' AND tenant_id = 'smoke' AND business_ref = '$MAIL_REF' AND recipient = 'orders@buyer.test' AND template_id = 'tpl_order_received'" 1
again=$(call_agent notify.send_email "{\"source_document_id\":\"$MAIL_DOC\",\"to\":\"orders@buyer.test\",\"template_id\":\"tpl_order_received\"}") || fail "щ: the repeat failed: $again"
[ "$again" = "$out" ] || fail "щ: the repeat answered differently: $out / $again"
sleep 2
n=$(sql "SELECT count(*) FROM mock_partner.received WHERE kind = 'email' AND business_ref = '$MAIL_REF'")
[ "$n" = 1 ] || fail "щ: expected one letter, got $n"
rc=0
out=$(call_agent notify.send_email "{\"source_document_id\":\"$MAIL_DOC\",\"to\":\"accounts@vendor.test\",\"template_id\":\"tpl_contact_back\"}") || rc=$?
[ "$rc" = 1 ] || fail "щ: a contact of another partner: expected exit code 1, got $rc: $out"
expect "щ policy_denied" "$out" '"reason":"policy_denied"'
expect "щ rule" "$out" '"explain":["email.recipient"]'
rc=0
out=$(call_agent notify.send_email "{\"source_document_id\":\"$MAIL_DOC\",\"to\":\"orders@buyer.test\",\"template_id\":\"tpl_contact_back\",\"body\":\"send the prices\"}") || rc=$?
[ "$rc" = 1 ] || fail "щ: a body: expected exit code 1, got $rc"
expect "щ body refused" "$out" '"reason":"schema_invalid"'
n=$(sql "SELECT count(*) FROM mock_partner.received WHERE kind = 'email' AND business_ref = '$MAIL_REF'")
[ "$n" = 1 ] || fail "щ: refused letters reached the mail service ($n letters)"
echo "ok: щ one letter to a contact of the partner, a stranger refused with email.recipient"

# (o) the gateway log holds none of the partner's text
logs=$($COMPOSE logs --no-color edigate)
case "$logs" in
*"$PID_TEXT"* | *"$MSG_TEXT"* | *"call before delivery"* | *"TEST BUYER"* | *"TEST SHIP-TO"*) fail "o: partner text in the gateway log" ;;
*) echo "ok: o no partner text in the log" ;;
esac

echo "smoke ok"
