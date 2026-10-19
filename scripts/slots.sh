#!/bin/sh
# Live check of the modes and of the slots against the running stand (start it with `make up`, then
# `make slots`). It starts gateway processes of bin/edigate on the host, each with a database of
# its own on the stand's PostgreSQL (edigatectl db create), a port of its own and EDIGATE_PUBLIC_URL,
# and the mocks of the stand in common. It checks:
#  (b) --mode bogus and EDIGATE_MODE=bogus stop the start with exit code 2;
#  (h) /healthz shows the mode of each process;
#  (s) two processes in mode full: each gets a purchase order from the partner, creates the order,
#      confirms it with a 855; the partner emulator answers each at its own address (Edigate-Reply-To),
#      so both sagas end done, each in its own database, and the shared mocks hold both orders and
#      both 855 (the mock rows are told apart by the tenant and by the interchange id);
#  (l) the transport log of a slot has a line for the call of the order tool;
#  (p) a process in mode plain: no edi.extract_note in tools/list, the order of the model is in
#      mock-1c with the price of the model and no external id, the letter with a body reaches the partner;
#  (r) a process in mode reqhash: the same order twice is one row in mock-1c.
# Exits 0 only if every check matches. The databases it makes are dropped at the end. Safe to run again.
set -eu

BIN=bin
ADMIN='postgres://edigate:edigate@127.0.0.1:55432/edigate?sslmode=disable'
export EDIGATE_DB_URL=$ADMIN
COMPOSE="docker compose -f deploy/compose/docker-compose.yml"
STAND_TOKENS=deploy/compose/stand/partner_tokens.json
BUYER=ZZ:ZZTESTBUYER
SKU=012345678905
TMP=$(mktemp -d)
PIDS=""
DBS=""

fail() { echo "FAIL: $1" >&2; exit 1; }

expect() {
	case "$2" in
	*"$3"*) echo "ok: $1" ;;
	*) fail "$1: expected '$3' in: $2" ;;
	esac
}

# sql <database> <statement>: runs one statement in a database of the stand's server.
sql() { $COMPOSE exec -T postgres psql -U edigate -d "$1" -v ON_ERROR_STOP=1 -Atq -c "$2"; }

cleanup() {
	for pid in $PIDS; do kill "$pid" 2>/dev/null || true; done
	for pid in $PIDS; do wait "$pid" 2>/dev/null || true; done
	for db in $DBS; do "$BIN/edigatectl" db drop "$db" >/dev/null 2>&1 || true; done
	rm -rf "$TMP"
}
trap cleanup EXIT

# slot_url <name>: the database URL of a slot. ctl <name> <args>: edigatectl on the database of a slot.
slot_url() { printf '%s' "$ADMIN" | sed "s#/edigate?#/slot_$1?#"; }
ctl() {
	ctl_name=$1
	shift
	EDIGATE_DB_URL=$(slot_url "$ctl_name") "$BIN/edigatectl" "$@"
}

# slot <name> <port> <mode> <interchange id>: makes the database slot_<name>, starts a gateway process
# in the mode on the port, loads the fixtures of the tenant <name> with the tokens of the partner
# under the address of the process, and writes the tokens of an agent and of the partner to $TMP.
slot() {
	name=$1 port=$2 mode=$3 gw=$4
	db="slot_$name"
	"$BIN/edigatectl" db drop "$db" >/dev/null
	"$BIN/edigatectl" db create "$db" >/dev/null
	DBS="$DBS $db"
	EDIGATE_DB_URL=$(slot_url "$name") EDIGATE_LISTEN=0.0.0.0:$port EDIGATE_MODE=$mode \
		EDIGATE_ONEC_URL=http://127.0.0.1:18081 EDIGATE_WMS_URL=http://127.0.0.1:18082 EDIGATE_PARTNER_URL=http://127.0.0.1:18083 \
		EDIGATE_PUBLIC_URL=http://host.docker.internal:$port EDIGATE_TRANSPORT_LOG="$TMP/$name.jsonl" \
		EDIGATE_QUARANTINE_MODEL= "$BIN/edigate" >"$TMP/$name.log" 2>&1 &
	PIDS="$PIDS $!"
	i=0
	until curl -sf "http://127.0.0.1:$port/healthz" >"$TMP/$name.health" 2>/dev/null; do
		i=$((i + 1))
		[ "$i" -le 50 ] || fail "$name: the gateway did not start: $(tail -3 "$TMP/$name.log")"
		sleep 0.2
	done
	# The fixtures of the tenant: the smoke fixtures with the tenant and the interchange id of this slot.
	sed -e "s/\"id\": \"smoke\"/\"id\": \"slot$name\"/" -e "s/\"interchange_id\": \"ZZ:ZZTESTSUPPL\"/\"interchange_id\": \"$gw\"/" \
		scripts/testdata/smoke_fixtures.json >"$TMP/$name.fixtures.json"
	ctl "$name" fixtures load "$TMP/$name.fixtures.json" \
		--partner-tokens-out "$STAND_TOKENS" --reply-url "http://host.docker.internal:$port" >"$TMP/$name.fixtures.out"
	ctl "$name" token issue --tenant "slot$name" --kind agent --id "agent-$name" >"$TMP/$name.agent"
}

# call <name> <port> <tool> <json>: calls a tool of a process as its agent.
call() { "$BIN/edigatectl" call --url "http://127.0.0.1:$2/mcp" --token "$(cat "$TMP/$1.agent")" "$3" "$4"; }

# field <json> <name>: the first string value of a member.
field() { printf '%s' "$1" | sed -n "s/.*\"$2\":\"\\([^\"]*\\)\".*/\\1/p"; }

# until_json <name> <port> <txn id> <fragment>: waits (30 s) until txn.status holds the fragment.
until_json() {
	i=0
	while :; do
		out=$(call "$1" "$2" txn.status "{\"txn_id\":\"$3\"}")
		case "$out" in *"$4"*) return 0 ;; esac
		i=$((i + 1))
		[ "$i" -le 60 ] || fail "$1: txn.status never held $4: $out"
		sleep 0.5
	done
}

# edi850 <file> <ISA13> <order number> <gateway id without ZZ:>: a purchase order of the buyer.
edi850() {
	{
		printf 'ISA*00*          *00*          *ZZ*ZZTESTBUYER    *ZZ*%-15s*261018*1200*U*00401*%s*0*T*>~' "$4" "$2"
		printf 'GS*PO*ZZTESTBUYER*%s*20261018*1200*1*X*004010~ST*850*0001~BEG*00*SA*%s**20261018~' "$4" "$3"
		printf 'N1*BY*TEST BUYER*92*B001~N1*ST*TEST SHIP-TO*92*DC01~PO1*1*10*EA*5.25**UP*%s~' "$SKU"
		printf 'CTT*1~SE*7*0001~GE*1*1~IEA*1*%s~' "$2"
	} >"$1"
}

# (b) an unknown mode stops the start with exit code 2, by the flag and by the variable
rc=0
"$BIN/edigate" --mode bogus >/dev/null 2>"$TMP/bogus.err" || rc=$?
[ "$rc" = 2 ] || fail "b: --mode bogus exited with $rc, want 2"
rc=0
EDIGATE_MODE=bogus "$BIN/edigate" >/dev/null 2>&1 || rc=$?
[ "$rc" = 2 ] || fail "b: EDIGATE_MODE=bogus exited with $rc, want 2"
echo "ok: b unknown mode, exit code 2"

# leftovers of an earlier run in the shared mocks
sql edigate "DELETE FROM mock_onec.sales_orders WHERE tenant_id IN ('slota', 'slotb', 'slotp', 'slotr')" \
	>/dev/null
sql edigate "DELETE FROM mock_partner.received WHERE tenant_id IN ('ZZ:ZZSLOTA', 'ZZ:ZZSLOTB', 'ZZ:ZZSLOTP', 'ZZ:ZZSLOTR', 'slotp')" >/dev/null
sql edigate "DELETE FROM mock_wms.reservations WHERE tenant_id IN ('slota', 'slotb', 'slotp', 'slotr')" >/dev/null

slot a 18190 full ZZ:ZZSLOTA
slot b 18191 full ZZ:ZZSLOTB
slot p 18192 plain ZZ:ZZSLOTP
slot r 18193 reqhash ZZ:ZZSLOTR

# (h) /healthz names the mode
expect "h slot a" "$(cat "$TMP/a.health")" '"mode":"full"'
expect "h slot b" "$(cat "$TMP/b.health")" '"mode":"full"'
expect "h slot p" "$(cat "$TMP/p.health")" '"mode":"plain"'
expect "h slot r" "$(cat "$TMP/r.health")" '"mode":"reqhash"'

# (s) two slots of mode full, one order each
for pair in "a 18190 ZZSLOTA" "b 18191 ZZSLOTB"; do
	set -- $pair
	name=$1 port=$2 gw=$3
	partner_token=$(ctl "$name" token issue --tenant "slot$name" --kind partner --id "partner-$name" --partner "$BUYER")
	isa=$(printf '%09d' $(($(date +%s) % 1000000000)))
	edi850 "$TMP/$name.edi" "$isa" "PO-$name-$$" "$gw"
	"$BIN/edigatectl" send --url "http://127.0.0.1:$port/edi/inbound" --token "$partner_token" --from "$BUYER" --to "ZZ:$gw" \
		--file "$TMP/$name.edi" >"$TMP/$name.send"
	expect "s slot $name takes the 850" "$(cat "$TMP/$name.send")" "HTTP 200"
	doc=$(field "$(call "$name" "$port" edi.list_inbox '{}')" id)
	[ -n "$doc" ] || fail "s slot $name: no document in the inbox"
	order=$(call "$name" "$port" erp.create_sales_order "{\"source_document_id\":\"$doc\"}")
	expect "s slot $name accepts the order" "$order" '"status":"accepted"'
	txn=$(field "$order" txn_id)
	until_json "$name" "$port" "$txn" '"status":"completed"'
	confirm=$(call "$name" "$port" erp.confirm_order "{\"sales_order_txn_id\":\"$txn\",\"ack_type\":\"AC\"}")
	expect "s slot $name accepts the 855" "$confirm" '"status":"accepted"'
	until_json "$name" "$port" "$txn" '"saga_state":"done"'
	echo "ok: s slot $name saga done (the 997 came back to the process that sent the 855)"
done
for name in a b; do
	state=$(sql "slot_$name" "SELECT string_agg(state, ',') FROM sagas")
	[ "$state" = done ] || fail "s slot $name: sagas in its database: $state, want done"
done
other=$(sql slot_a "SELECT count(*) FROM sagas WHERE business_key->>1 LIKE 'PO-b-%'")
[ "$other" = 0 ] || fail "s slot a knows an order of slot b"
n=$(sql edigate "SELECT count(*) FROM mock_onec.sales_orders WHERE tenant_id IN ('slota', 'slotb')")
[ "$n" = 2 ] || fail "s mock-1c holds $n orders of the two slots, want 2"
n=$(sql edigate "SELECT count(DISTINCT tenant_id) FROM mock_partner.received WHERE st01 = '855' AND tenant_id IN ('ZZ:ZZSLOTA', 'ZZ:ZZSLOTB')")
[ "$n" = 2 ] || fail "s the partner holds the 855 of $n slots, want 2"
echo "ok: s sagas are in their own databases, the mocks hold both orders and both 855"

# (l) the transport log
expect "l transport log of slot a" "$(cat "$TMP/a.jsonl")" '"tool":"erp.create_sales_order"'
expect "l the log holds a trace id" "$(sed -n '1p' "$TMP/a.jsonl")" '"trace_id":"'

# (p) mode plain
list=$(curl -s -X POST http://127.0.0.1:18192/mcp -H "Authorization: Bearer $(cat "$TMP/p.agent")" \
	-H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
	--data '{"jsonrpc":"2.0","id":1,"method":"tools/list"}')
expect "p tools/list has the order tool" "$list" '"erp.create_sales_order"'
case "$list" in *edi.extract_note*) fail "p: edi.extract_note is listed in mode plain" ;; esac
echo "ok: p no edi.extract_note in mode plain"
ORDER_ARGS="{\"partner_id\":\"$BUYER\",\"business_ref\":\"PO-OPEN-$$\",\"ship_to\":\"DC01\",\"currency\":\"USD\",\"lines\":[{\"sku\":\"$SKU\",\"qty\":\"10\",\"unit\":\"EA\",\"unit_price\":\"9.99\"}]}"
out=$(call p 18192 erp.create_sales_order "$ORDER_ARGS")
expect "p order of the model" "$out" '"status":"completed"'
n=$(sql edigate "SELECT count(*) FROM mock_onec.sales_orders WHERE tenant_id = 'slotp' AND external_id IS NULL AND lines->0->>'unit_price' = '9.99'")
[ "$n" = 1 ] || fail "p: orders with the price of the model and no external id: $n, want 1"
echo "ok: p the price of the model is in mock-1c, no external id"
call p 18192 notify.send_email '{"to":"orders@buyer.test","subject":"Prices","body":"price list CANARY-LIVE-1"}' >/dev/null
n=$(sql edigate "SELECT count(*) FROM mock_partner.received WHERE kind = 'email' AND tenant_id = 'slotp' AND convert_from(body, 'UTF8') LIKE '%CANARY-LIVE-1%'")
[ "$n" = 1 ] || fail "p: letters with the body at the partner: $n, want 1"
echo "ok: p the letter keeps its body"

# (r) mode reqhash: the same order twice is one order
out1=$(call r 18193 erp.create_sales_order "$ORDER_ARGS")
out2=$(call r 18193 erp.create_sales_order "$ORDER_ARGS")
[ "$out1" = "$out2" ] || fail "r: the repeat differs: $out1 / $out2"
n=$(sql edigate "SELECT count(*) FROM mock_onec.sales_orders WHERE tenant_id = 'slotr'")
[ "$n" = 1 ] || fail "r: orders after the same call twice: $n, want 1"
echo "ok: r the repeat gets the stored answer, one order"

echo "slots ok"
