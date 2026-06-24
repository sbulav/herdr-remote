#!/bin/bash
set -e
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Kill any existing relay
lsof -ti :8375 | xargs kill -9 2>/dev/null || true
sleep 1

echo "🐑 herdr-remote"

# Start relay
uv run "$SCRIPT_DIR/herdr_relay.py" &
RELAY_PID=$!
sleep 2

if ! kill -0 $RELAY_PID 2>/dev/null; then
    echo "✗ Failed to start relay"; exit 1
fi

# Start tunnel if cloudflared available
if command -v cloudflared >/dev/null 2>&1; then
    LOGFILE=$(mktemp)
    cloudflared tunnel --url http://localhost:8375 > "$LOGFILE" 2>&1 &
    CF_PID=$!

    # Wait for tunnel URL
    for i in $(seq 1 15); do
        URL=$(grep -o 'https://[^ ]*\.trycloudflare.com' "$LOGFILE" 2>/dev/null | tail -1)
        [ -n "$URL" ] && break
        sleep 1
    done
    rm -f "$LOGFILE"

    if [ -n "$URL" ]; then
        WS_URL="wss://$(echo $URL | sed 's|https://||')"
        echo "✓ Ready"
        echo ""
        echo "  $WS_URL"
        echo ""
        echo "  Phone: herdr-remote.pages.dev → ⚙ → paste URL above"
        echo "  Plugin: herdr plugin install dcolinmorgan/herdr-push"
        echo "          echo 'HERDR_RELAY=$URL' > \"\$(herdr plugin config-dir herdr.push)/.env\""
        echo ""
    else
        echo "✗ Tunnel failed. Relay still running on ws://localhost:8375"
    fi

    trap "kill $RELAY_PID $CF_PID 2>/dev/null" EXIT
    wait $RELAY_PID
else
    echo "✓ Relay on ws://localhost:8375"
    echo "  Install cloudflared for remote access: brew install cloudflared"
    trap "kill $RELAY_PID 2>/dev/null" EXIT
    wait $RELAY_PID
fi
