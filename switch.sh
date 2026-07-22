#!/usr/bin/env bash
# Point Lidarr at a different metadata server, or put it back.
#
# Lidarr keeps metadataSource in its config but renders no field for it, so
# this script is the interface. The change takes effect immediately and needs
# no restart.
set -euo pipefail

usage() {
    cat <<'EOF'
Usage:
  switch.sh --lidarr <url> --api-key <key> --to <metadata-url>
  switch.sh --lidarr <url> --api-key <key> --revert
  switch.sh --lidarr <url> --api-key <key> --show

Options:
  --lidarr    Lidarr's base URL, for example http://localhost:8686
  --api-key   Lidarr API key, from Settings > General > Security
  --to        Metadata server to use, for example http://localhost:5001/
  --revert    Go back to the official cloud service
  --show      Print the current setting and change nothing
  --force     Skip the check that the new server actually answers

Your API key is sent to Lidarr only, and is never logged.
EOF
}

LIDARR="" API_KEY="" TARGET="" ACTION="" FORCE=0

while [ $# -gt 0 ]; do
    case "$1" in
        --lidarr)  LIDARR="${2:-}"; shift 2 ;;
        --api-key) API_KEY="${2:-}"; shift 2 ;;
        --to)      TARGET="${2:-}"; ACTION="set"; shift 2 ;;
        --revert)  ACTION="revert"; shift ;;
        --show)    ACTION="show"; shift ;;
        --force)   FORCE=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "switch.sh: unknown option $1" >&2; usage; exit 2 ;;
    esac
done

if [ -z "$LIDARR" ] || [ -z "$API_KEY" ] || [ -z "$ACTION" ]; then
    usage; exit 2
fi

for tool in curl jq; do
    command -v "$tool" >/dev/null || { echo "switch.sh: $tool is required" >&2; exit 1; }
done

LIDARR="${LIDARR%/}"
CONFIG_URL="$LIDARR/api/v1/config/metadataprovider"

# Folds curl's stderr into stdout so a failure carries its reason back to the
# caller, which is captured and shown under our own message rather than
# printed alongside it.
api() {
    local method="$1" body="${2:-}"
    if [ -n "$body" ]; then
        curl -fsS -X "$method" "$CONFIG_URL" \
            -H "X-Api-Key: $API_KEY" -H "Content-Type: application/json" -d "$body" 2>&1
    else
        curl -fsS -X "$method" "$CONFIG_URL" -H "X-Api-Key: $API_KEY" 2>&1
    fi
}

if ! CURRENT=$(api GET); then
    echo "switch.sh: could not read Lidarr's config at $CONFIG_URL" >&2
    [ -n "$CURRENT" ] && echo "  $CURRENT" >&2
    echo "  check the URL is right and the API key is correct" >&2
    exit 1
fi

describe() {
    local value="$1"
    if [ -z "$value" ] || [ "$value" = "null" ]; then
        echo "the official cloud service (api.lidarr.audio)"
    else
        echo "$value"
    fi
}

CURRENT_SOURCE=$(echo "$CURRENT" | jq -r '.metadataSource // ""')

if [ "$ACTION" = "show" ]; then
    echo "Lidarr is using: $(describe "$CURRENT_SOURCE")"
    exit 0
fi

NEW_SOURCE=""
if [ "$ACTION" = "set" ]; then
    # Lidarr appends /{route} to this value, so a missing trailing slash
    # produces requests to hostartist/{mbid}.
    case "$TARGET" in */) ;; *) TARGET="$TARGET/" ;; esac
    NEW_SOURCE="$TARGET"

    if [ "$FORCE" -eq 0 ]; then
        # Checking first turns a silently broken library into a refusal here.
        if ! curl -fsS --max-time 10 "$TARGET" >/dev/null 2>&1; then
            echo "switch.sh: $TARGET did not answer, so Lidarr was left alone" >&2
            echo "  start the metadata server first, or pass --force to switch anyway" >&2
            exit 1
        fi
    fi
fi

# PUT the whole object back with one field changed. Sending only that field
# would blank every other setting in the section.
UPDATED=$(echo "$CURRENT" | jq --arg src "$NEW_SOURCE" '.metadataSource = $src')

if ! RESULT=$(api PUT "$UPDATED"); then
    echo "switch.sh: Lidarr rejected the update" >&2
    [ -n "$RESULT" ] && echo "  $RESULT" >&2
    exit 1
fi

VERIFY=$(api GET | jq -r '.metadataSource // ""')
if [ "$VERIFY" != "$NEW_SOURCE" ]; then
    echo "switch.sh: the change did not stick, Lidarr still reports $(describe "$VERIFY")" >&2
    exit 1
fi

echo "Lidarr was using: $(describe "$CURRENT_SOURCE")"
echo "Lidarr is now using: $(describe "$VERIFY")"
if [ "$ACTION" = "set" ]; then
    echo
    echo "To undo this:"
    echo "  $0 --lidarr $LIDARR --api-key <key> --revert"
fi
