#!/bin/sh
# tests/run.sh — basic tests for herdr-remote relay
PASS=0; FAIL=0

assert_eq() {
  if [ "$1" = "$2" ]; then PASS=$((PASS+1)); echo "  pass: $3"
  else FAIL=$((FAIL+1)); echo "  FAIL: $3 (expected '$2', got '$1')"; fi
}

echo "herdr-remote tests"
echo ""

RELAY_SCRIPT="$(dirname "$0")/../relay/herdr_relay.py"

# Test 1: relay script has valid Python syntax
echo "1. relay script syntax valid"
python3 -c "import ast; ast.parse(open('$RELAY_SCRIPT').read())" 2>&1
assert_eq "$?" "0" "herdr_relay.py parses"

# Test 2: relay script has PEP 723 metadata
echo "2. has inline script metadata (uv compatible)"
grep -q "requires-python" "$RELAY_SCRIPT"
assert_eq "$?" "0" "PEP 723 metadata present"

# Test 3: TUI script syntax
echo "3. TUI script syntax valid"
python3 -c "import ast; ast.parse(open('$(dirname "$0")/../relay/herdr_tui.py').read())" 2>&1
assert_eq "$?" "0" "herdr_tui.py parses"

# Test 4: Telegram bot syntax
echo "4. Telegram bot syntax valid"
python3 -c "import ast; ast.parse(open('$(dirname "$0")/../relay/herdr_telegram.py').read())" 2>&1
assert_eq "$?" "0" "herdr_telegram.py parses"

# Test 5: web app exists and has key elements
echo "5. web app has required elements"
WEB="$(dirname "$0")/../web/index.html"
grep -q "WebSocket" "$WEB" && grep -q "herdr" "$WEB" && grep -q "theme" "$WEB"
assert_eq "$?" "0" "index.html has WebSocket, herdr, theme"

# Test 6: demo worker syntax (skip if not present)
echo "6. demo worker syntax valid"
DEMO="$(dirname "$0")/../demo-worker/src/index.js"
if [ -f "$DEMO" ]; then
  node --check "$DEMO" 2>&1
  assert_eq "$?" "0" "demo worker parses"
else
  PASS=$((PASS+1)); echo "  skip: demo-worker not present"
fi

# Test 7: start.sh is executable
echo "7. start.sh is executable"
[ -x "$(dirname "$0")/../relay/start.sh" ]
assert_eq "$?" "0" "start.sh executable"

echo ""
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] && exit 0 || exit 1
