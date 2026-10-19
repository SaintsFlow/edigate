#!/bin/sh
# Smoke test of the running stand (started by `make smoke` / `make up`).
# Creates a tenant and two tokens, then checks that an agent gets an empty
# inbox, that bad requests get the right errors, and that the audit log has the
# calls. Exits 0 only if every check matches. Safe to run again: principal ids
# carry a random suffix.
set -eu

BIN=bin
URL=http://127.0.0.1:18080/mcp
export EDIGATE_DB_URL='postgres://edigate:edigate@127.0.0.1:55432/edigate?sslmode=disable'
COMPOSE="docker compose -f deploy/compose/docker-compose.yml"
SUFFIX=$(date +%s)$$
AGENT_ID="smoke-agent-$SUFFIX"
OPERATOR_ID="smoke-operator-$SUFFIX"
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

CALL_BODY='{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"edi.list_inbox","arguments":{}}}'

"$BIN/edigatectl" tenant add smoke "Smoke tenant" >/dev/null
AGENT_TOKEN=$("$BIN/edigatectl" token issue --tenant smoke --kind agent --id "$AGENT_ID")
OPERATOR_TOKEN=$("$BIN/edigatectl" token issue --tenant smoke --kind operator --id "$OPERATOR_ID")

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
rows=$($COMPOSE exec -T postgres psql -U edigate -d edigate -Atc \
	"SELECT outcome FROM audit_log WHERE tenant_id = 'smoke' AND principal_id = '$AGENT_ID' AND event_type = 'tool_call' ORDER BY seq")
expect "f audit ok" "$rows" "ok"
expect "f audit schema_invalid" "$rows" "schema_invalid"

echo "smoke ok"
