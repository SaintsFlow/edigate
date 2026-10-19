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
# idempotency key or price of the 850.
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
PARTNER_ID="smoke-partner-$SUFFIX"
GATEWAY=ZZ:ZZTESTSUPPL
BUYER=ZZ:ZZTESTBUYER
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

# edi850 <file> <ISA13> <order number>: writes an 850 of the smoke partner. The
# PID05, MSG01 and N1-02 texts are what the agent must never see.
PID_TEXT="SMOKE SECRET WIDGET TEXT"
MSG_TEXT="Please call before delivery."
edi850() {
	{
		printf 'ISA*00*          *00*          *ZZ*ZZTESTBUYER    *ZZ*ZZTESTSUPPL    *261018*1200*U*00401*%s*0*T*>~' "$2"
		printf 'GS*PO*ZZTESTBUYER*ZZTESTSUPPL*20261018*1200*1*X*004010~ST*850*0001~BEG*00*SA*%s**20261018~' "$3"
		printf 'N1*BY*TEST BUYER*92*B001~N1*ST*TEST SHIP-TO*92*DC01~PO1*1*10*EA*5.25**UP*012345678905~'
		printf 'PID*F****%s~MSG*%s~CTT*1~SE*9*0001~GE*1*1~IEA*1*%s~' "$PID_TEXT" "$MSG_TEXT" "$2"
	} >"$1"
}

# send <file> [from]: posts the file as the smoke partner; prints "HTTP <code>" and the body.
send() {
	"$BIN/edigatectl" send --url "$INBOUND" --token "$PARTNER_TOKEN" --from "${2:-$BUYER}" --to "$GATEWAY" --file "$1"
}

CALL_BODY='{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"edi.list_inbox","arguments":{}}}'

sql "DELETE FROM outbox WHERE tenant_id = 'smoke'" "DELETE FROM idempotency_keys WHERE tenant_id = 'smoke'" \
	"DELETE FROM mock_onec.sales_orders WHERE tenant_id = 'smoke'" \
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

# (a) the agent gets an empty inbox
out=$("$BIN/edigatectl" call --url "$URL" --token "$AGENT_TOKEN" edi.list_inbox '{}')
[ "$out" = '{"documents":[]}' ] || fail "a: expected {\"documents\":[]}, got: $out"
echo "ok: a empty inbox"

# (b) no token: 401 unauthorized
code=$(post "" "$TMP/b")
[ "$code" = 401 ] || fail "b: expected HTTP 401, got $code"
expect "b unauthorized" "$(cat "$TMP/b")" '"reason":"unauthorized"'

# (c) Mcp-Name differs from the body: 400 and code -32020
code=$(post "$AGENT_TOKEN" "$TMP/c" -H 'MCP-Protocol-Version: 2026-07-28' -H 'Mcp-Method: tools/call' -H 'Mcp-Name: edi.read_document')
[ "$code" = 400 ] || fail "c: expected HTTP 400, got $code"
expect "c header mismatch" "$(cat "$TMP/c")" '"code":-32020'

# (d) operator token on /mcp: 403 forbidden
code=$(post "$OPERATOR_TOKEN" "$TMP/d")
[ "$code" = 403 ] || fail "d: expected HTTP 403, got $code"
expect "d forbidden" "$(cat "$TMP/d")" '"reason":"forbidden"'

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

# (o) the gateway log holds none of the partner's text
logs=$($COMPOSE logs --no-color edigate)
case "$logs" in
*"$PID_TEXT"* | *"$MSG_TEXT"* | *"call before delivery"* | *"TEST BUYER"* | *"TEST SHIP-TO"*) fail "o: partner text in the gateway log" ;;
*) echo "ok: o no partner text in the log" ;;
esac

echo "smoke ok"
