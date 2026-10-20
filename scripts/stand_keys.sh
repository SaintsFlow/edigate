# Sourced by scripts/smoke.sh and scripts/demo.sh (not run on its own). Needs $BIN (the folder of the
# built edigatectl). Makes the keys of the stand if they are missing and tells the gateway about them:
#   - the pair that signs the pins of upstream tools (the gateway gets the public half);
#   - the pair that signs the anchors of the audit chain (the gateway gets the private half, as it signs
#     the anchors itself; stand value only);
#   - the folder deploy/compose/stand/anchors, which the gateway container sees as /anchors: the anchor
#     files outside the database.
# The values go to deploy/compose/.env, which compose reads. Both keys stay out of git (keys/ and .env).
KEYS=deploy/compose/keys
mkdir -p "$KEYS" deploy/compose/stand/anchors
chmod 777 deploy/compose/stand/anchors # the gateway runs as another user in its container
[ -f "$KEYS/manifest.key" ] || "$BIN/edigatectl" manifests keygen --out "$KEYS/manifest.key" >"$KEYS/manifest.pub"
chmod 644 "$KEYS/manifest.key" # the stand's own key; the one-off container of pinctl runs as another user
[ -f "$KEYS/audit.key" ] || "$BIN/edigatectl" audit keygen --out "$KEYS/audit.key" >"$KEYS/audit.pub"
{
	printf 'EDIGATE_MANIFEST_PUBKEY=%s\n' "$(cat "$KEYS/manifest.pub")"
	printf 'EDIGATE_AUDIT_SIGNING_KEY=%s\n' "$(cat "$KEYS/audit.key")"
} >deploy/compose/.env
