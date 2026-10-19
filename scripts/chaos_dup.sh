#!/bin/sh
# make chaos-dup: the guarantee "one order per document" while the gateway keeps dying.
#
# Starts a stand with fault injection (deploy/compose/chaos.yml: eight gateway
# processes that exit at random, mock-1c that commits an order and then answers 500
# for 30 % of the requests). A partner sends N own 850 documents; an agent calls
# erp.create_sales_order for each of them and repeats the same call after any error,
# as a client does after a lost answer. When the documents are done it keeps
# repeating the calls over the same documents until Docker has restarted the
# gateway processes at least MIN_RESTARTS times (every repeat must give the same
# txn_id), then waits for the outbox to empty.
#
# Exit 0 only if: restarts >= MIN_RESTARTS; mock-1c holds exactly N orders of the
# tenant, N different business references, each with its own external id; every
# document got one txn_id through the whole run; all keys are completed and every
# outbox row is acked. The normal stand is brought back at the end.
#
# Test only. Needs docker, bin/edigatectl (make build) and a free port 18080.
set -eu

N=${CHAOS_DOCS:-50}
MIN_RESTARTS=${CHAOS_MIN_RESTARTS:-100}
BIN=bin
URL=http://127.0.0.1:18080/mcp
INBOUND=http://127.0.0.1:18080/edi/inbound
GATEWAY=ZZ:ZZCHAOSGW
BUYER=ZZ:ZZCHAOSBUY
TENANT=chaos
GATEWAYS="edigate edigate-r1 edigate-r2 edigate-r3 edigate-r4 edigate-r5 edigate-r6 edigate-r7"
BASE_FILE=deploy/compose/docker-compose.yml
COMPOSE_BASE="docker compose -f $BASE_FILE"
COMPOSE_CHAOS="docker compose -f $BASE_FILE -f deploy/compose/chaos.yml"
export EDIGATE_DB_URL='postgres://edigate:edigate@127.0.0.1:55432/edigate?sslmode=disable'
TMP=$(mktemp -d)
START=$(date +%s)
CALLS=0
RETRIES=0
ROUNDS=0

# The normal stand comes back whatever happens; the exit code of the script is kept.
restore() {
	echo "restoring the normal stand"
	$COMPOSE_BASE up -d --build --remove-orphans --wait >"$TMP/restore.log" 2>&1 || {
		echo "warning: the normal stand did not come back, run make up" >&2
		tail -5 "$TMP/restore.log" >&2
	}
	rm -rf "$TMP"
}
trap restore EXIT

fail() { echo "FAIL: $1" >&2; exit 1; }

sql() {
	for statement in "$@"; do
		$COMPOSE_BASE exec -T postgres psql -U edigate -d edigate -v ON_ERROR_STOP=1 -Atq -c "$statement"
	done
}

# restarts: the sum of Docker's RestartCount over the gateway containers.
restarts() {
	total=0
	for id in $IDS; do
		n=$(docker inspect -f '{{.RestartCount}}' "$id")
		total=$((total + n))
	done
	echo "$total"
}

# place <agent token> <document id>: calls the tool until it answers "accepted",
# whatever errors come on the way, and writes "<document> <txn_id>" to txns.txt.
place() {
	attempts=0
	while :; do
		CALLS=$((CALLS + 1))
		if out=$("$BIN/edigatectl" call --url "$URL" --token "$1" erp.create_sales_order "{\"source_document_id\":\"$2\"}" 2>/dev/null); then
			case "$out" in
			*'"status":"accepted"'*)
				txn=$(printf '%s' "$out" | sed -n 's/.*"txn_id":"\([^"]*\)".*/\1/p')
				[ -n "$txn" ] || fail "no txn_id in: $out"
				echo "$2 $txn" >>"$TMP/txns.txt"
				return 0
				;;
			esac
			fail "unexpected answer for $2: $out"
		fi
		RETRIES=$((RETRIES + 1))
		attempts=$((attempts + 1))
		[ "$attempts" -le 300 ] || fail "document $2: the call kept failing"
		sleep 0.3
	done
}

echo "starting the chaos stand"
$COMPOSE_CHAOS up -d --build --remove-orphans --wait postgres mock-onec >"$TMP/up.log" 2>&1 || { tail -20 "$TMP/up.log" >&2; fail "postgres and mock-onec did not start"; }
$COMPOSE_CHAOS up -d --force-recreate $GATEWAYS >>"$TMP/up.log" 2>&1 || { tail -20 "$TMP/up.log" >&2; fail "the gateways did not start"; }
IDS=""
for s in $GATEWAYS; do
	IDS="$IDS $($COMPOSE_CHAOS ps -q "$s")"
done
tries=0
until curl -fsS -o /dev/null http://127.0.0.1:18080/healthz 2>/dev/null; do
	tries=$((tries + 1))
	[ "$tries" -le 60 ] || fail "the gateway did not become healthy"
	sleep 1
done

# Fresh data: the rows of an earlier run of this tenant go (audit rows stay: the log is append-only).
sql "DELETE FROM outbound_docs WHERE tenant_id = '$TENANT'" \
	"DELETE FROM saga_steps WHERE saga_id IN (SELECT id FROM sagas WHERE tenant_id = '$TENANT')" "DELETE FROM sagas WHERE tenant_id = '$TENANT'" \
	"DELETE FROM approvals WHERE tenant_id = '$TENANT'" "DELETE FROM daily_auto_totals WHERE tenant_id = '$TENANT'" \
	"DELETE FROM outbox WHERE tenant_id = '$TENANT'" "DELETE FROM idempotency_keys WHERE tenant_id = '$TENANT'" \
	"DELETE FROM edi_notes WHERE tenant_id = '$TENANT'" "DELETE FROM edi_documents WHERE tenant_id = '$TENANT'" \
	"DELETE FROM edi_interchanges WHERE tenant_id = '$TENANT'" "DELETE FROM edi_rejected WHERE tenant_id = '$TENANT'" \
	"DELETE FROM mock_onec.sales_orders WHERE tenant_id = '$TENANT'" >/dev/null
"$BIN/edigatectl" fixtures load scripts/testdata/chaos_fixtures.json >/dev/null
SUFFIX=$(date +%s)$$
PARTNER_TOKEN=$("$BIN/edigatectl" token issue --tenant "$TENANT" --kind partner --id "chaos-partner-$SUFFIX" --partner "$BUYER")
for k in 1 2 3 4 5; do
	"$BIN/edigatectl" token issue --tenant "$TENANT" --kind agent --id "chaos-agent-$k-$SUFFIX" >>"$TMP/agents.txt"
done

# A. the partner sends N documents (one partner is allowed 2 interchanges a second).
echo "sending $N documents"
i=1
while [ "$i" -le "$N" ]; do
	n=$(printf '%04d' "$i")
	tr -d '\n' <scripts/testdata/chaos_850.tpl |
		sed -e "s/@ISA13@/$((600000000 + i))/g" -e "s/@GS06@/$i/g" -e "s/@BEG03@/CHAOS-$n/g" >"$TMP/doc-$n.edi"
	attempts=0
	until out=$("$BIN/edigatectl" send --url "$INBOUND" --token "$PARTNER_TOKEN" --from "$BUYER" --to "$GATEWAY" --file "$TMP/doc-$n.edi" 2>&1); do
		attempts=$((attempts + 1))
		[ "$attempts" -le 300 ] || fail "document $n could not be sent, the last answer: $out"
		sleep 0.6
	done
	sleep 0.6
	i=$((i + 1))
done
sql "SELECT bk_ref || ' ' || id FROM edi_documents WHERE tenant_id = '$TENANT' ORDER BY bk_ref" >"$TMP/docs.txt"
[ "$(wc -l <"$TMP/docs.txt" | tr -d ' ')" = "$N" ] || fail "expected $N stored documents"

# B. one call per document, then C. the same calls again until enough restarts.
pass() {
	k=0
	while read -r _ doc; do
		k=$((k + 1))
		place "$(sed -n "$((k % 5 + 1))p" "$TMP/agents.txt")" "$doc"
	done <"$TMP/docs.txt"
	ROUNDS=$((ROUNDS + 1))
}
echo "calling erp.create_sales_order for $N documents"
pass
started=$(date +%s)
while [ "$(restarts)" -lt "$MIN_RESTARTS" ]; do
	[ $(($(date +%s) - started)) -le 900 ] || fail "only $(restarts) restarts in 15 minutes"
	echo "restarts so far: $(restarts); repeating the calls"
	pass
done

# D. the outbox must empty: every row acked, none failed or dead.
echo "waiting for the outbox"
waited=0
while :; do
	state=$(sql "SELECT count(*) FILTER (WHERE status <> 'acked'), count(*) FILTER (WHERE status IN ('failed', 'dead')) FROM outbox WHERE tenant_id = '$TENANT'")
	[ "${state#*|}" = 0 ] || fail "outbox rows failed or dead: $state"
	[ "${state%|*}" != 0 ] || break
	waited=$((waited + 1))
	[ "$waited" -le 120 ] || fail "the outbox did not empty in 120 s: $state"
	sleep 1
done

# Checks.
RESTARTS=$(restarts)
[ "$RESTARTS" -ge "$MIN_RESTARTS" ] || fail "restarts $RESTARTS < $MIN_RESTARTS"
orders=$(sql "SELECT count(*), count(DISTINCT business_ref), count(DISTINCT external_id), count(external_id) FROM mock_onec.sales_orders WHERE tenant_id = '$TENANT'")
[ "$orders" = "$N|$N|$N|$N" ] || fail "orders in mock-1c (rows|references|external ids|with an external id): $orders, want $N each"
matched=$(sql "SELECT count(*) FROM mock_onec.sales_orders o JOIN idempotency_keys k ON o.external_id = encode(k.key, 'hex') WHERE o.tenant_id = '$TENANT' AND k.tenant_id = '$TENANT' AND k.state = 'completed'")
[ "$matched" = "$N" ] || fail "orders matched to completed keys: $matched, want $N"
sort -u "$TMP/txns.txt" >"$TMP/txns.uniq"
per_doc=$(awk '{c[$1]++} END {bad = 0; for (d in c) if (c[d] != 1) bad++; print bad "|" length(c)}' "$TMP/txns.uniq")
[ "$per_doc" = "0|$N" ] || fail "documents with more than one txn_id | documents seen: $per_doc, want 0|$N"
awk '{print $2}' "$TMP/txns.uniq" | sort >"$TMP/seen.txt"
sql "SELECT txn_id FROM idempotency_keys WHERE tenant_id = '$TENANT' ORDER BY txn_id" >"$TMP/stored.txt"
cmp -s "$TMP/seen.txt" "$TMP/stored.txt" || fail "the txn_ids the agent saw differ from the keys in the database"

echo "restarts: $RESTARTS"
echo "time: $(($(date +%s) - START)) s"
echo "calls: $CALLS (repeat rounds over the documents: $((ROUNDS - 1)))"
echo "retries after errors: $RETRIES"
echo "chaos-dup ok: $N documents, $N orders, one txn_id per document"
