#!/usr/bin/env bash
set -euo pipefail

display_number=93
Xvfb ":$display_number" -screen 0 1920x1200x24 &
xvfb_pid=$!
cleanup() {
  kill "$xvfb_pid" 2>/dev/null || true
  wait "$xvfb_pid" 2>/dev/null || true
}
trap cleanup EXIT INT TERM
sleep 1

DISPLAY=":$display_number" GDK_BACKEND=x11 PW_WEBKIT_HEADED=1 npm run test:e2e
