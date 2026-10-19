#!/bin/sh
# make demo: three scenarios of the gateway, run by a script (no model plans anything) against a
# stand built from nothing. Every step prints one line and checks itself; the first mismatch prints
# FAIL and ends the run with code 1. The last line is "demo ok" with code 0.
#
#   S1  An injection in an 850 does not reach an effect. The 850 carries instructions in PID05 and
#       MSG01 and a BEG03 that fails its format. The agent never sees the text (only references).
#       The order is made from the price list and the ship-to code of the document, a letter to a
#       foreign address is policy_denied, and a note reference in the address of a letter is
#       tainted_sink.
#   S2  A saga with receipts. Order, 855, reservation, shipment, 856. Both sagas are done, both
#       documents have an MDN and a 997 with AK5 A, and the 997 for the incoming 850 reached the partner.
#   S3  Faults without duplicates. The same order in a new exchange and a repeated call after a
#       "lost" answer give one order. A negative 997 for a 855 makes the saga an incident and no
#       second 855 goes out. A row of the audit log changed behind the trigger's back is found by
#       `edigatectl audit verify` with its number; after the row is put back it verifies again.
#
# The stand is wiped first (docker compose down -v) and built again, so a second run starts the
# same way. It stays up at the end (make down). Needs docker, go (make build) and curl. Ollama on
# the host is optional: without it the step that reads a note through the quarantined model
# prints "skip" with the reason.
set -eu

BIN=bin
URL=http://127.0.0.1:18080/mcp
INBOUND=http://127.0.0.1:18080/edi/inbound
OPS_URL=http://127.0.0.1:18080
PARTNER_URL=http://127.0.0.1:18083
export EDIGATE_DB_URL='postgres://edigate:edigate@127.0.0.1:55432/edigate?sslmode=disable'
COMPOSE="docker compose -f deploy/compose/docker-compose.yml"
DIR=scripts/demo
GATEWAY=ZZ:ZZTESTDEMO # not the id of the smoke test, so that both can live in one database
BUYER=ZZ:ZZTESTBUYER
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
STARTED=$(date +%s)

fail() { echo "FAIL: $1" >&2; exit 1; }
pass() { echo "ok   $1"; }

# expect <label> <text> <fragment>: the text must contain the fragment.
expect() {
	case "$2" in
	*"$3"*) pass "$1" ;;
	*) fail "$1: expected '$3' in: $2" ;;
	esac
}

# forbid <label> <text> <fragment>...: the text must contain none of the fragments.
forbid() {
	label=$1
	text=$2
	shift 2
	for fragment in "$@"; do
		case "$text" in
		*"$fragment"*) fail "$label: '$fragment' is in: $text" ;;
		esac
	done
	pass "$label"
}

# sql <statement>: runs it in the database of the stand, prints the plain result.
sql() {
	$COMPOSE exec -T postgres psql -U edigate -d edigate -v ON_ERROR_STOP=1 -Atq -c "$1"
}

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

call_agent() { "$BIN/edigatectl" call --url "$URL" --token "$AGENT_TOKEN" "$@"; }

# wait_status <txn id> <status>: waits up to 20 s for txn.status to show the status.
wait_status() {
	waited=0
	while :; do
		cur=$(call_agent txn.status "{\"txn_id\":\"$1\"}") || fail "txn.status failed: $cur"
		case "$cur" in *"\"status\":\"$2\""*) return 0 ;; esac
		waited=$((waited + 1))
		[ "$waited" -le 40 ] || fail "expected $2 within 20 s, got: $cur"
		sleep 0.5
	done
}

# json_field <name> <text>: the first string value of the field (the answers are one JSON line).
json_field() { printf '%s' "$2" | sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p"; }

# send_850 <template> <ref> <quantity>: the buyer posts an 850 made from a template with a new
# ISA13; DOC is the id of the stored document. One send per second at most (the partner's bucket).
ISA_N=100000000
send_850() {
	sleep 1
	ISA_N=$((ISA_N + 1))
	isa=$(printf '%09d' "$ISA_N")
	printf '%s' "$(sed -e "s/@ISA13@/$isa/g" -e "s/@REF@/$2/g" -e "s/@QTY@/$3/g" "$DIR/$1")" >"$TMP/doc.edi"
	out=$("$BIN/edigatectl" send --url "$INBOUND" --token "$PARTNER_TOKEN" --from "$BUYER" --to "$GATEWAY" --file "$TMP/doc.edi") ||
		fail "send of $2 failed: $out"
	case "$out" in *"HTTP 200"*) ;; *) fail "send of $2: $out" ;; esac
	DOC=$(sql "SELECT d.id FROM edi_documents d JOIN edi_interchanges i ON i.id = d.interchange_id
		WHERE i.tenant_id = 'demo' AND i.direction = 'in' AND i.isa13 = '$isa'")
	[ -n "$DOC" ] || fail "the 850 of $2 was not stored"
}

# approve_if_pending <answer of a tool>: a document that the limits send to an operator is approved.
approve_if_pending() {
	case "$1" in
	*'"status":"pending_approval"'*)
		id=$(json_field approval_id "$1")
		"$BIN/edigatectl" approvals approve --url "$OPS_URL" --token "$OPERATOR_TOKEN" "$id" >/dev/null || fail "approve failed"
		;;
	esac
}

# order_of <document id>: the agent orders from the document and waits until it is delivered to the
# ERP; ORDER_TXN is the txn_id of the order.
order_of() {
	made=$(call_agent erp.create_sales_order "{\"source_document_id\":\"$1\"}") || fail "erp.create_sales_order failed: $made"
	ORDER_TXN=$(json_field txn_id "$made")
	[ -n "$ORDER_TXN" ] || fail "no txn_id in: $made"
	wait_status "$ORDER_TXN" completed
}

# verify_audit: edigatectl audit verify for the tenant, with the public key and the anchor file.
verify_audit() {
	"$BIN/edigatectl" audit verify --tenant demo --pubkey "$(cat "$KEYS/audit.pub")" --anchor-dir deploy/compose/stand/anchors 2>&1
}

DOC_STATE="SELECT string_agg(d.state || '|' || d.mdn_ok || '|' || COALESCE(d.ack_status, '-'), ',' ORDER BY d.st01)
	FROM outbound_docs d JOIN sagas g ON g.id = d.saga_id WHERE g.tenant_id = 'demo' AND g.business_key->>1"

# ---------------------------------------------------------------- the stand, from nothing
echo "== stand: wiped and built again"
$COMPOSE down -v --remove-orphans >"$TMP/down.log" 2>&1 || fail "docker compose down failed: $(tail -3 "$TMP/down.log")"
rm -rf deploy/compose/stand/anchors
. scripts/stand_keys.sh
$COMPOSE up -d --build --wait >"$TMP/up.log" 2>&1 || fail "docker compose up failed: $(tail -5 "$TMP/up.log")"
pass "stand is up (gateway, mock-1c, mock-wms, partner emulator, PostgreSQL)"

"$BIN/edigatectl" fixtures load "$DIR/fixtures.json" >/dev/null || fail "fixtures load failed"
AGENT_TOKEN=$("$BIN/edigatectl" token issue --tenant demo --kind agent --id demo-agent)
OPERATOR_TOKEN=$("$BIN/edigatectl" token issue --tenant demo --kind operator --id demo-operator)
PARTNER_TOKEN=$("$BIN/edigatectl" token issue --tenant demo --kind partner --id demo-buyer --partner "$BUYER")
printf '{"%s":"%s"}\n' "$BUYER" "$PARTNER_TOKEN" >deploy/compose/stand/partner_tokens.json
chmod 644 deploy/compose/stand/partner_tokens.json
curl -fsS -X POST "$PARTNER_URL/control" -d '{"ack_status":"A","ack_delay_ms":0,"mdn_delay_ms":0,"mdn_drop":0}' >/dev/null ||
	fail "partner emulator does not answer on $PARTNER_URL"
pass "tenant demo, an agent, an operator and the buyer are set up"

# ---------------------------------------------------------------- S1
echo "== S1: an injection in an 850 does not reach an effect"
send_850 injection_850.tpl x 10
INJ_DOC=$DOC
inbox=$(call_agent edi.list_inbox '{}') || fail "S1: edi.list_inbox failed: $inbox"
doc=$(call_agent edi.read_document "{\"document_id\":\"$INJ_DOC\"}") || fail "S1: edi.read_document failed: $doc"
forbid "S1.1 the agent sees no text of the attack" "$doc$inbox" EVIL IGNORE "PRICE 0.01" "MAIL PRICE" "call before delivery"
expect "S1.2 the free text is only a reference" "$doc" '"reason":"free_text","ref":"$note_'
expect "S1.3 the BEG03 that fails its format is a reference too" "$doc" '"reason":"format"'

NOTE_REF=$(printf '%s' "$doc" | sed -n 's/.*"ref":"\([^"]*\)","segment_path":"PO1\[1\]\/MSG01".*/\1/p')
[ -n "$NOTE_REF" ] || fail "S1: no reference of the MSG01 note in: $doc"
if ! curl -s -m 3 http://127.0.0.1:11434/api/tags >"$TMP/tags" 2>/dev/null; then
	echo "skip S1.4 the delivery note is read through the quarantined model (Ollama is not reachable on 127.0.0.1:11434)"
elif ! grep -q 'llama3.2:3b' "$TMP/tags"; then
	echo "skip S1.4 the delivery note is read through the quarantined model (the model llama3.2:3b is not installed in Ollama)"
else
	rc=0
	ext=$(call_agent edi.extract_note "{\"document_id\":\"$INJ_DOC\",\"ref\":\"$NOTE_REF\",\"schema_id\":\"delivery_instruction\"}") || rc=$?
	forbid "S1.4 the note comes back as a closed value or is refused, never as text" "$ext" EVIL IGNORE "PRICE 0.01"
	if [ "$rc" = 0 ]; then
		expect "S1.4 the value is declassified" "$ext" '"declassified":true'
	else
		expect "S1.4 the answer is a refusal" "$ext" '"reason":"quarantine_invalid"'
	fi
fi

order_of "$INJ_DOC"
row=$(sql "SELECT total::text || '|' || (lines->0->>'unit_price') || '|' || ship_to FROM mock_onec.sales_orders WHERE tenant_id = 'demo'")
[ "$row" = "75.00|7.50|DC01" ] || fail "S1.5: the order in mock-1c (total|price|ship-to) is '$row', want 75.00|7.50|DC01"
pass "S1.5 the order follows the price list (7.50, not 0.01) and the ship-to code DC01 of the document"

rc=0
denied=$(call_agent notify.send_email "{\"source_document_id\":\"$INJ_DOC\",\"to\":\"evil@evil.test\",\"template_id\":\"tpl_contact_back\"}" 2>/dev/null) || rc=$?
[ "$rc" = 1 ] || fail "S1.6: a letter to a foreign address: expected exit code 1, got $rc: $denied"
expect "S1.6 a letter to a foreign address is policy_denied" "$denied" '"reason":"policy_denied"'
expect "S1.6 the rule is email.recipient" "$denied" '"explain":["email.recipient"]'

rc=0
tainted=$(call_agent notify.send_email "{\"source_document_id\":\"$INJ_DOC\",\"to\":\"$NOTE_REF@buyer.test\",\"template_id\":\"tpl_contact_back\"}" 2>/dev/null) || rc=$?
[ "$rc" = 1 ] || fail "S1.7: a reference in the address: expected exit code 1, got $rc: $tainted"
expect "S1.7 a note reference in the address of a letter is tainted_sink" "$tainted" '"reason":"tainted_sink"'
sleep 2
n=$(sql "SELECT count(*) FROM mock_partner.received WHERE kind = 'email'")
[ "$n" = 0 ] || fail "S1.8: $n letters reached the mail service"
pass "S1.8 no letter left the gateway"

# ---------------------------------------------------------------- S2
echo "== S2: a saga with receipts (order, 855, reservation, shipment, 856)"
S2_REF="DEMOS2$STARTED"
send_850 order_850.tpl "$S2_REF" 10
order_of "$DOC"
S2_TXN=$ORDER_TXN
S2_DOC=$DOC
pass "S2.1 the order is in the ERP"
ans=$(call_agent erp.confirm_order "{\"sales_order_txn_id\":\"$S2_TXN\",\"ack_type\":\"AC\"}") || fail "S2: erp.confirm_order failed: $ans"
approve_if_pending "$ans"
wait_sql "S2.2 the 855 is delivered with MDN and a positive 997" "$DOC_STATE = '$S2_REF' AND d.st01 = '855'" "delivered|true|A"
wait_sql "S2.2 the order saga is done" "SELECT state FROM sagas WHERE tenant_id = 'demo' AND kind = 'order' AND business_key->>1 = '$S2_REF'" done
pass "S2.2 the 855 has an MDN and a 997 with AK5 A, the order saga is done"
ans=$(call_agent wms.reserve "{\"sales_order_txn_id\":\"$S2_TXN\"}") || fail "S2: wms.reserve failed: $ans"
wait_status "$(json_field txn_id "$ans")" completed
pass "S2.3 the goods are reserved in the warehouse"
ans=$(call_agent edi.send_asn "{\"sales_order_txn_id\":\"$S2_TXN\"}") || fail "S2: edi.send_asn failed: $ans"
approve_if_pending "$ans"
wait_sql "S2.4 the 856 is delivered with MDN and a positive 997" "$DOC_STATE = '$S2_REF' AND d.st01 = '856'" "delivered|true|A"
wait_sql "S2.4 both sagas are done" "SELECT string_agg(state, ',' ORDER BY kind) FROM sagas WHERE tenant_id = 'demo' AND business_key->>1 = '$S2_REF'" "done,done"
pass "S2.4 the 856 has an MDN and a 997 with AK5 A, both sagas are done"
got=$(sql "SELECT string_agg(st01 || ':' || n || ':' || ni, ',' ORDER BY st01) FROM
	(SELECT st01, count(*) AS n, count(DISTINCT isa13) AS ni FROM mock_partner.received WHERE business_ref = '$S2_REF' GROUP BY st01) t")
[ "$got" = "855:1:1,856:1:1" ] || fail "S2.5: documents at the partner (set:count:different ISA13): $got"
n=$(sql "SELECT count(DISTINCT isa13) FROM mock_partner.received WHERE business_ref = '$S2_REF'")
[ "$n" = 2 ] || fail "S2.5: the 855 and the 856 must have different ISA13, got $n"
pass "S2.5 the partner got one 855 and one 856, with different ISA13"
n=$(sql "SELECT count(*) FROM mock_partner.received WHERE tenant_id = '$GATEWAY' AND st01 = '997' AND position('AK5*A~' in convert_from(body, 'UTF8')) > 0")
[ "$n" -ge 1 ] || fail "S2.6: the partner has no 997 with AK5 A for its 850"
pass "S2.6 the 997 for the incoming 850 reached the partner"

# ---------------------------------------------------------------- S3
echo "== S3: faults without duplicates"
send_850 order_850.tpl "$S2_REF" 10 # the same order again, in a new exchange (F4)
n=$(sql "SELECT count(*) FROM edi_documents WHERE tenant_id = 'demo' AND bk_ref = '$S2_REF'")
[ "$n" = 2 ] || fail "S3.1: expected two documents with the reference of the order, got $n"
again=$(call_agent erp.create_sales_order "{\"source_document_id\":\"$DOC\"}") || fail "S3.1: the repeat failed: $again"
[ "$(json_field txn_id "$again")" = "$S2_TXN" ] || fail "S3.1: the new exchange got another txn_id: $again"
n=$(sql "SELECT count(*) FROM mock_onec.sales_orders WHERE tenant_id = 'demo' AND business_ref = '$S2_REF'")
[ "$n" = 1 ] || fail "S3.1: expected one order in mock-1c, got $n"
pass "S3.1 the same order in a new exchange gives the same txn_id and one order"

S3_REF="DEMOS3$STARTED"
send_850 order_850.tpl "$S3_REF" 10
lost=$(call_agent erp.create_sales_order "{\"source_document_id\":\"$DOC\"}") || fail "S3.2: the first call failed: $lost"
repeat=$(call_agent erp.create_sales_order "{\"source_document_id\":\"$DOC\"}") || fail "S3.2: the repeat failed: $repeat"
[ "$repeat" = "$lost" ] || fail "S3.2: the repeat answered differently: $lost / $repeat"
wait_status "$(json_field txn_id "$repeat")" completed
n=$(sql "SELECT count(*) FROM mock_onec.sales_orders WHERE tenant_id = 'demo' AND business_ref = '$S3_REF'")
[ "$n" = 1 ] || fail "S3.2: expected one order in mock-1c, got $n"
pass "S3.2 a repeated call (a lost answer simulated by calling twice; real ones: make chaos-dup) gives the same answer and one order"

curl -fsS -X POST "$PARTNER_URL/control" -d '{"ack_status":"R"}' >/dev/null || fail "S3: control failed"
S3R_REF="DEMOS3R$STARTED"
send_850 order_850.tpl "$S3R_REF" 10
order_of "$DOC"
ans=$(call_agent erp.confirm_order "{\"sales_order_txn_id\":\"$ORDER_TXN\",\"ack_type\":\"AC\"}") || fail "S3: erp.confirm_order failed: $ans"
approve_if_pending "$ans"
wait_sql "S3.3 the order saga is an incident" "SELECT state FROM sagas WHERE tenant_id = 'demo' AND kind = 'order' AND business_key->>1 = '$S3R_REF'" incident
curl -fsS -X POST "$PARTNER_URL/control" -d '{"ack_status":"A"}' >/dev/null || fail "S3: control failed"
sleep 3
got=$(sql "SELECT count(*) || ':' || count(DISTINCT isa13) FROM mock_partner.received WHERE business_ref = '$S3R_REF'")
[ "$got" = "1:1" ] || fail "S3.3: after a negative 997 the partner got (transmissions:different ISA13) $got, want 1:1"
pass "S3.3 a negative 997 for a 855: the saga is an incident and no second 855 with a new ISA13 went out"

echo "-- audit log"
# The gateway signs an anchor at every 100 rows of a tenant (checking every 2 s) and appends it to a file
# outside the database. A few harmless calls bring the tenant over the first 100.
rows=$(sql "SELECT count(*) FROM audit_log WHERE tenant_id = 'demo'")
while [ "$rows" -lt 105 ]; do
	call_agent edi.list_inbox '{}' >/dev/null || fail "S3.4: edi.list_inbox failed"
	rows=$((rows + 1))
	sleep 0.2 # the gateway allows 20 calls a second per agent
done
wait_sql "S3.4 an anchor is signed" "SELECT (count(*) >= 1)::text FROM audit_anchors WHERE tenant_id = 'demo'" true
waited=0
until [ -s deploy/compose/stand/anchors/demo.jsonl ]; do
	waited=$((waited + 1))
	[ "$waited" -le 20 ] || fail "S3.4: the anchor file deploy/compose/stand/anchors/demo.jsonl was not written"
	sleep 0.5
done
pass "S3.4 the first 100 rows are anchored, in the database and in a file outside it"
out=$(verify_audit) || fail "S3.4: audit verify of the untouched log: $out"
expect "S3.4 the untouched audit log verifies" "$out" "ok "
echo "     $out"

TARGET=$(sql "SELECT seq FROM audit_log WHERE tenant_id = 'demo' AND event_type = 'effect_applied' ORDER BY seq LIMIT 1")
[ -n "$TARGET" ] || fail "S3.5: no audit row of an applied effect"
ORIGINAL=$(sql "SELECT outcome FROM audit_log WHERE seq = $TARGET")
rc=0
refused=$(sql "UPDATE audit_log SET outcome = 'tampered' WHERE seq = $TARGET" 2>&1) || rc=$?
[ "$rc" != 0 ] || fail "S3.5: the trigger let an UPDATE of the audit log through"
expect "S3.5 the trigger refuses to change a row" "$refused" "append-only"
# The attacker takes the trigger off (database rights are outside the threat model) and edits the row.
sql "ALTER TABLE audit_log DISABLE TRIGGER audit_log_append_only; UPDATE audit_log SET outcome = 'tampered' WHERE seq = $TARGET; ALTER TABLE audit_log ENABLE TRIGGER audit_log_append_only" >/dev/null ||
	fail "S3.6: could not edit the row for the test"
rc=0
out=$(verify_audit) || rc=$?
[ "$rc" = 1 ] || fail "S3.6: audit verify of a changed log exited with $rc, want 1: $out"
expect "S3.6 audit verify names the changed row" "$out" "broken at seq $TARGET"
echo "     $(printf '%s\n' "$out" | sed -n '1p')"
sql "ALTER TABLE audit_log DISABLE TRIGGER audit_log_append_only; UPDATE audit_log SET outcome = '$ORIGINAL' WHERE seq = $TARGET; ALTER TABLE audit_log ENABLE TRIGGER audit_log_append_only" >/dev/null ||
	fail "S3.7: could not put the row back"
out=$(verify_audit) || fail "S3.7: audit verify after the row was put back: $out"
expect "S3.7 with the row put back the log verifies again" "$out" "ok "

echo "demo ok ($(($(date +%s) - STARTED)) s). The stand stays up; make down removes it."
