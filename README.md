# herdr-remote

Mobile & desktop interface for [herdr](https://herdr.dev) AI coding agents. Monitor agent status, approve requests, and send responses from your phone, menu bar, terminal, or Telegram.

## Architecture

```
┌─────────────┐  ┌─────────────┐  ┌─────────────┐  ┌──────────┐
│  iOS App    │  │  Mac Menu   │  │  TUI        │  │ Telegram │
│  (SwiftUI)  │  │  Bar App    │  │  (Textual)  │  │ Bot      │
└──────┬──────┘  └──────┬──────┘  └──────┬──────┘  └────┬─────┘
       │                 │                │               │
       └────────────── WebSocket ─────────┴───────────────┘
                         │
              ┌──────────┴──────────┐
              │   herdr-remote      │ :8375
              │   relay             │ (WS + HTTP POST + UDP)
              └──────────┬──────────┘
                         │
          ┌──────────────┼──────────────┐
          │ CLI poll     │ herdr-push   │ SSH poll
          │ (local)      │ (event push) │ (remote)
          │              │              │
       ┌──┴──┐     ┌────┴────┐    ┌────┴────┐
       │herdr│     │herdr    │    │herdr    │
       │local│     │(remote) │    │(remote) │
       └─────┘     └─────────┘    └─────────┘
```

## Install — macOS Menu Bar App

Download the latest DMG from [Releases](https://github.com/dcolinmorgan/herdr-remote/releases), or build from source:

```bash
cd herdi-mac
./build.sh
cp -r dist/Herdi.app /Applications/
open /Applications/Herdi.app
```

The app lives in your menu bar. Toggle "Launch at Login" in Settings.

## Install — Terminal TUI

```bash
pip install textual websockets
python3 relay/herdr_tui.py

# Or split into a herdr pane:
./relay/herdr-dash.sh
```

## Setup

### Relay (on your Mac)

```bash
cd relay
python3 -m venv .venv && .venv/bin/pip install websockets zeroconf
.venv/bin/python3 herdr_relay.py
```

### Remote Herdr Instances

Monitor agents running on remote machines — no SSH required. Install the [herdr-push](https://github.com/dcolinmorgan/herdr-push) plugin on each machine:

```bash
# On the remote machine:
herdr plugin install dcolinmorgan/herdr-push

# Set your relay address:
export HERDR_RELAY="wss://your-tunnel.trycloudflare.com"
# or LAN: export HERDR_RELAY="http://192.168.1.x:8375"

launchctl setenv HERDR_RELAY "$HERDR_RELAY"  # macOS
herdr server reload-config
```

The plugin pushes status events via HTTP POST (zero deps, just curl) on every agent state change. No polling, no SSH, no inbound ports needed.

#### Exposing the relay (no firewall changes needed)

```bash
# Quick tunnel (free, URL changes on restart):
cloudflared tunnel --url http://localhost:8375

# Or Tailscale Funnel:
tailscale funnel 8375
```

#### Alternative: SSH polling

```bash
export HERDR_REMOTES="user@server1,user@server2"
python3 relay/herdr_relay.py
```

### Telegram Bot

```bash
export HERDR_TG_TOKEN="your-token"
export HERDR_TG_CHAT_ID="your-chat-id"
python3 relay/herdr_telegram.py
```

When an agent blocks, you get a Telegram message with inline buttons:
- **✅ Yes (once)** → `yes, single permission`
- **🔓 Trust (always)** → `trust, always allow`
- **❌ No** → `no (tab to edit)`

Reply with free text to send a custom response.

## LaunchAgent

```bash
cp relay/com.herdr-remote.relay.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.herdr-remote.relay.plist
```

## Features

- Agent kanban board (Blocked → Working → Idle)
- One-tap/click approval for blocked agents
- Custom text responses
- Auto-reconnect on network changes
- Bonjour service discovery
- Auto-update from GitHub releases
- Push notifications (macOS + Telegram)
- Local + remote agent monitoring
