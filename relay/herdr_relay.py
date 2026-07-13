#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = ["websockets>=14.0", "zeroconf>=0.80.0"]
# ///
"""herdr-remote relay — polls herdr, accepts push events (HTTP POST + WebSocket + UDP), broadcasts to clients."""
import asyncio, glob, json, os, re, shlex, signal, socket, sqlite3, subprocess, uuid

try:
    from websockets.asyncio.server import serve
except ImportError:
    from websockets.server import serve

HERDR = os.environ.get("HERDR_BIN", "/opt/homebrew/bin/herdr")
WS_PORT = int(os.environ.get("HERDR_RELAY_PORT", "8375"))
POLL_INTERVAL = 2
AUTH_TOKEN = os.environ.get("HERDR_RELAY_TOKEN", "")  # Optional: shared secret for relay auth
PRESETS_FILE = os.environ.get("HERDR_PRESETS_FILE", "")
# Native structured stores used by supported coding agents.
CLAUDE_PROJECTS = os.environ.get("HERDR_CLAUDE_PROJECTS", "~/.claude/projects")
OPENCODE_DB = os.environ.get("HERDR_OPENCODE_DB", "~/.local/share/opencode/opencode-stable.db")
TRANSCRIPT_MAX_BYTES = 262144  # tail window read per poll — bounds ssh transfer
TRANSCRIPT_BLOCK_LIMIT = 200   # most recent blocks kept per session

# Remote hosts: comma-separated SSH targets
REMOTES = [r.strip() for r in os.environ.get("HERDR_REMOTES", "").split(",") if r.strip()]

# Kiro CLI free-text permission menus
TOOL_OPTIONS = ["yes, single permission", "trust, always allow", "no (tab to edit)"]
SUBAGENT_OPTIONS = ["approve all pending", "configure individually", "exit (cancel subagents)"]
# OpenCode TUI: left/right + enter (default selection = Allow once)
OPENCODE_OPTIONS = ["Allow once", "Allow always", "Reject"]
# Claude Code numbered selection menus: "❯ 1. Yes" / "  2. No"
CLAUDE_YES_NO = ["1. Yes", "2. No"]
NUMBERED_OPT_RE = re.compile(r"(?:^|\n)[ \t]*[❯>]?[ \t]*(\d+)\.\s+(\S[^\n]*)")
# Bullet-style free-text options: "> yes, single permission" or "• Allow once"
BULLET_OPT_RE = re.compile(
    r"(?:^|\n)[ \t]*(?:[❯>•*-]|\[\s?\])[ \t]+([A-Za-z][^\n]{0,80})"
)
CHROME_RE = re.compile(
    r"^[\s─━═_—│|◔◑◕●\s]+$"
    r"|Kiro\s[·•]"
    r"|esc to cancel"
    r"|type to queue"
    r"|^\s*[◔◑◕●]\s+(Shell|Bash)"
)

clients = set()
last_statuses = {}
event_queue = asyncio.Queue()
pane_remote_map = {}
session_target_map = {}
request_results = {}
pane_cwd_map = {}      # pane_id -> (cwd, agent, remote, ambiguous agent/cwd)
subscriptions = {}     # ws -> pane_id the client is currently viewing
stream_sigs = {}       # (id(ws), pane_id) -> signature of the last blocks pushed


def load_presets():
    if not PRESETS_FILE:
        return []
    with open(PRESETS_FILE, encoding="utf-8") as f:
        document = json.load(f)
    if document.get("schema_version") != 1:
        raise ValueError("unsupported preset schema version")
    presets = document.get("presets")
    if not isinstance(presets, list):
        raise ValueError("presets must be a list")
    seen = set()
    for preset in presets:
        preset_id = preset.get("id", "")
        if not re.fullmatch(r"[a-z0-9][a-z0-9._-]{0,63}", preset_id) or preset_id in seen:
            raise ValueError(f"invalid or duplicate preset id: {preset_id}")
        seen.add(preset_id)
        if not isinstance(preset.get("repository"), str) or not preset["repository"]:
            raise ValueError(f"missing repository in preset {preset_id}")
        if preset.get("agent") not in ("claude", "opencode", "codex"):
            raise ValueError(f"unsupported agent in preset {preset_id}")
        if not isinstance(preset.get("model"), str) or not preset["model"]:
            raise ValueError(f"missing model in preset {preset_id}")
        hosts = preset.get("hosts")
        if not isinstance(hosts, dict) or not hosts:
            raise ValueError(f"missing hosts in preset {preset_id}")
        for host_id, host in hosts.items():
            if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}", host_id):
                raise ValueError(f"invalid host id: {host_id}")
            if not os.path.isabs(host.get("cwd", "")):
                raise ValueError(f"cwd must be absolute for {preset_id}@{host_id}")
            if host.get("target") is not None and not isinstance(host.get("target"), str):
                raise ValueError(f"invalid target for {preset_id}@{host_id}")
    return presets


PRESETS = load_presets()
PRESETS_BY_ID = {preset["id"]: preset for preset in PRESETS}
HOST_TARGETS = {
    host_id: host.get("target")
    for preset in PRESETS
    for host_id, host in preset["hosts"].items()
}


def public_presets():
    return [
        {
            "id": preset["id"], "label": preset["label"],
            "repository": preset["repository"],
            "agent": preset["agent"], "model": preset["model"],
            "hosts": {host_id: {"cwd": host["cwd"]} for host_id, host in preset["hosts"].items()},
        }
        for preset in PRESETS
    ]


def session_id(host_id, pane_id):
    return f"legacy:{host_id}:{pane_id}"


def run_herdr_checked(*args, remote=None):
    try:
        if remote:
            cmd = ["ssh", "-o", "ConnectTimeout=5", "-o", "BatchMode=yes", remote, HERDR, *args]
        else:
            cmd = [HERDR, *args]
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=15)
        return r.returncode == 0, r.stdout.strip()
    except Exception:
        return False, ""


def run_herdr(*args, remote=None):
    return run_herdr_checked(*args, remote=remote)[1]


def get_agents_from_host(remote=None, host_id=None):
    online, raw = run_herdr_checked("pane", "list", remote=remote)
    host_label = host_id or remote or "local"
    try:
        data = json.loads(raw)
        panes = data.get("result", {}).get("panes", [])
        agents = [
            {
                "pane_id": p["pane_id"],
                "agent": p.get("agent", ""),
                "status": p.get("agent_status", "unknown"),
                "cwd": p.get("cwd", ""),
                "project": os.path.basename(p.get("cwd", "")),
                "host": host_label,
                "remote": remote,
            }
            for p in panes if p.get("agent")
        ]
    except (json.JSONDecodeError, KeyError):
        agents = []
    return agents, online


def get_all_agents():
    if HOST_TARGETS:
        agents = []
        hosts = []
        for host_id, remote in HOST_TARGETS.items():
            host_agents, online = get_agents_from_host(remote=remote, host_id=host_id)
            agents.extend(host_agents)
            hosts.append({"host_id": host_id, "online": online})
    else:
        agents, _ = get_agents_from_host(remote=None)
        for remote in REMOTES:
            remote_agents, _ = get_agents_from_host(remote=remote)
            agents.extend(remote_agents)
        hosts = []
    return agents, hosts


def read_pane(pane_id, remote=None):
    raw = run_herdr("pane", "read", pane_id, "--lines", "20", "--source", "recent", remote=remote)
    lines = [l for l in raw.splitlines() if l.strip() and not CHROME_RE.search(l)]
    return "\n".join(lines[-6:])


def _numbered_options(text):
    numbered = NUMBERED_OPT_RE.findall(text)
    if len(numbered) < 2:
        return None
    seen = {}
    for num, label in numbered:
        if num not in seen:
            seen[num] = f"{num}. {label.strip()}"
    opts = [seen[k] for k in sorted(seen, key=int)]
    return opts if len(opts) >= 2 else None


def _bullet_options(text):
    labels = []
    seen = set()
    for label in BULLET_OPT_RE.findall(text):
        cleaned = label.strip().rstrip(".,;")
        key = cleaned.lower()
        if key in seen or len(cleaned) < 2:
            continue
        # Skip chrome / prose that looks like a bullet but isn't a choice.
        if any(x in key for x in ("esc to", "tab to", "ctrl+", "type to", "press ")):
            continue
        seen.add(key)
        labels.append(cleaned)
    return labels if len(labels) >= 2 else None


def detect_options(text):
    """Return selectable response labels for a blocked-agent prompt, or None.

    Labels are what clients display. respond_action() maps a chosen label to
    either free-text (send-text) or a key sequence (send-keys) for the agent TUI.
    """
    if not text:
        return None
    lower = text.lower()

    # --- Known free-text menus (exact option strings the agent reads) ---
    if "yes, single permission" in lower:
        return TOOL_OPTIONS
    if "approve all pending" in lower or "pending from subagents" in lower:
        return SUBAGENT_OPTIONS

    # OpenCode: "Permission required" with Allow once / Allow always / Reject
    if "permission required" in lower or (
        "allow once" in lower and "allow always" in lower and "reject" in lower
    ):
        return list(OPENCODE_OPTIONS)

    # --- Numbered menus (Claude Code and similar) ---
    numbered = _numbered_options(text)
    if numbered:
        return numbered

    # Bullet-style free-text options (> / • / -)
    bullets = _bullet_options(text)
    if bullets:
        return bullets

    # Claude "Do you want to proceed?" without captured numbers
    if (
        "do you want to proceed" in lower
        or "do you want to allow" in lower
        or "ask rule" in lower
        or "/permissions to let auto mode decide" in lower
    ):
        return list(CLAUDE_YES_NO)

    # Codex / simple y/n
    if "[y/n]" in lower or "yes (y)" in lower or "proceed (y)" in lower:
        return ["y", "n"]

    # Cursor-style write approval
    if "write to this file?" in lower and "proceed (y)" in lower:
        return ["y", "n"]

    # Hermes / generic allow once | session | deny
    if "allow once" in lower and ("deny" in lower or "allow for this session" in lower):
        return ["allow once", "allow for this session", "deny"]

    return None


def respond_action(text):
    """Map a client option label to a send action.

    Returns ("text", payload) for pane send-text, or ("keys", [key...]) for
    pane send-keys. OpenCode uses left/right + enter; Claude uses digits.
    """
    if not text:
        return "text", text
    raw = text.strip()
    lower = raw.lower()

    # Numbered menu label -> digit
    m = re.match(r"^(\d+)\.\s+", raw)
    if m:
        return "text", m.group(1)

    # OpenCode permission dialog (default selection = first = Allow once).
    # Only exact OpenCode labels map to keys — free-text "deny"/"always" stay text.
    if lower == "allow once":
        return "keys", ["Enter"]
    if lower in ("allow always", "always allow"):
        # move right to "Allow always", enter, then confirm stage
        return "keys", ["Right", "Enter", "Enter"]
    if lower == "reject":
        return "keys", ["Escape"]

    # y/n style
    if lower in ("y", "yes"):
        return "text", "y"
    if lower in ("n", "no"):
        return "text", "n"

    return "text", raw


def respond_text(text):
    """Backward-compatible: return free-text payload only (no key sequences)."""
    kind, payload = respond_action(text)
    if kind == "text":
        return payload
    # Callers that only support text fall back to first meaningful token.
    return text


# ---------------------------------------------------------------------------
# Structured output from Claude Code and OpenCode native session stores.
#
# Claude Code persists a fully structured JSONL transcript per project at
# ~/.claude/projects/<escaped-cwd>/<session-uuid>.jsonl. Reading it directly
# gives us real OutputBlocks (assistant prose, tool calls, thinking, prompts)
# with no ANSI/box-drawing/spinner guesswork. The relay already knows each
# OpenCode stores equivalent message parts in SQLite. The relay already knows
# each pane's cwd, so neither path needs a change to `herdr` itself.
# ---------------------------------------------------------------------------

def claude_project_dir(cwd):
    """Escape a cwd the way Claude Code names its per-project transcript dir."""
    return re.sub(r"[/._]", "-", cwd)


def read_transcript(cwd, remote=None):
    """Return (path, jsonl_text) for the newest transcript in cwd, or (None, None).

    Reads only the trailing TRANSCRIPT_MAX_BYTES so a long session stays cheap
    to poll; the (possibly partial) first line is tolerated by the parser.
    """
    if not cwd:
        return None, None
    proj = claude_project_dir(cwd)
    if remote:
        root = CLAUDE_PROJECTS.replace("~", "$HOME")
        script = (
            f'd="{root}/$1"; '
            'f=$(ls -t "$d"/*.jsonl 2>/dev/null | head -1); '
            '[ -n "$f" ] || exit 0; '
            'printf "%s\\n" "$f"; '
            f'tail -c {TRANSCRIPT_MAX_BYTES} "$f"'
        )
        remote_cmd = "sh -c " + shlex.quote(script) + " sh " + shlex.quote(proj)
        try:
            r = subprocess.run(
                ["ssh", "-o", "ConnectTimeout=5", "-o", "BatchMode=yes", remote, remote_cmd],
                capture_output=True, text=True, timeout=15)
        except Exception:
            return None, None
        if r.returncode != 0 or not r.stdout:
            return None, None
        path, _, body = r.stdout.partition("\n")
        return (path.strip() or None), body
    # local
    try:
        d = os.path.join(os.path.expanduser(CLAUDE_PROJECTS), proj)
        files = glob.glob(os.path.join(d, "*.jsonl"))
        if not files:
            return None, None
        path = max(files, key=os.path.getmtime)
        with open(path, "rb") as fh:
            fh.seek(0, os.SEEK_END)
            size = fh.tell()
            fh.seek(max(0, size - TRANSCRIPT_MAX_BYTES))
            body = fh.read().decode("utf-8", "replace")
        return path, body
    except Exception:
        return None, None


def summarize_tool(inp):
    """Pick the most descriptive single line from a tool_use input dict."""
    if not isinstance(inp, dict):
        return ""
    for key in ("file_path", "filePath", "command", "pattern", "path", "url", "query", "description", "prompt"):
        v = inp.get(key)
        if isinstance(v, str) and v.strip():
            return v.strip().splitlines()[0][:200]
    return ""


def transcript_to_blocks(jsonl_text, limit=TRANSCRIPT_BLOCK_LIMIT):
    """Map a Claude Code JSONL transcript into a list of OutputBlock dicts."""
    blocks = []

    def add(kind, **kw):
        kw["id"] = f"b{len(blocks)}"
        kw["kind"] = kind
        blocks.append(kw)

    for line in jsonl_text.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            rec = json.loads(line)
        except Exception:
            continue  # partial first line or non-JSON meta
        if not isinstance(rec, dict) or rec.get("isMeta") or rec.get("isSidechain"):
            continue
        rtype = rec.get("type")
        msg = rec.get("message")
        if not isinstance(msg, dict):
            continue
        if rtype == "assistant":
            for b in msg.get("content") or []:
                if not isinstance(b, dict):
                    continue
                bt = b.get("type")
                if bt == "text" and (b.get("text") or "").strip():
                    add("assistant_text", markdown=b["text"])
                elif bt == "thinking" and (b.get("thinking") or "").strip():
                    add("status", label="Thought", text=b["thinking"].strip().splitlines()[0][:200])
                elif bt == "tool_use":
                    add("tool", label=b.get("name") or "tool", text=summarize_tool(b.get("input")))
        elif rtype == "user":
            content = msg.get("content")
            if isinstance(content, str):
                t = content.strip()
                if t and not t.startswith("<command-") and "<command-name>" not in t:
                    add("status", label="You", text=t[:2000])
            # list content (tool_result / multimodal) is skipped in v1
    return blocks[-limit:]


OPENCODE_PART_QUERY = """
SELECT json_extract(m.data, '$.role'), p.data
FROM message m
JOIN part p ON p.message_id = m.id
WHERE m.session_id = ?
ORDER BY m.time_created DESC, p.time_created DESC, p.id DESC
LIMIT ?
"""


def _read_opencode_local(db_path, cwd):
    """Return the newest top-level OpenCode session and its recent parts."""
    db_uri = "file:" + os.path.expanduser(db_path) + "?mode=ro"
    with sqlite3.connect(db_uri, uri=True, timeout=2) as db:
        session = db.execute(
            "SELECT id, time_updated FROM session "
            "WHERE directory = ? AND parent_id IS NULL "
            "ORDER BY time_updated DESC LIMIT 1", (cwd,)
        ).fetchone()
        if not session:
            return None
        session_id, updated = session
        rows = db.execute(
            OPENCODE_PART_QUERY, (session_id, TRANSCRIPT_BLOCK_LIMIT * 4)
        ).fetchall()
    rows.reverse()
    return {"session_id": session_id, "updated": updated, "rows": rows}


def read_opencode(cwd, remote=None):
    """Read bounded structured parts for the newest OpenCode session in cwd."""
    if not cwd:
        return None
    if not remote:
        try:
            return _read_opencode_local(OPENCODE_DB, cwd)
        except Exception:
            return None
    script = """
import json, os, sqlite3, sys
db = sqlite3.connect("file:" + os.path.expanduser(sys.argv[1]) + "?mode=ro", uri=True, timeout=2)
session = db.execute("SELECT id, time_updated FROM session WHERE directory = ? AND parent_id IS NULL ORDER BY time_updated DESC LIMIT 1", (sys.argv[2],)).fetchone()
if session:
    rows = db.execute(sys.argv[3], (session[0], int(sys.argv[4]))).fetchall()
    rows.reverse()
    print(json.dumps({"session_id": session[0], "updated": session[1], "rows": rows}))
"""
    remote_cmd = " ".join([
        "python3", "-c", shlex.quote(script), shlex.quote(OPENCODE_DB),
        shlex.quote(cwd), shlex.quote(OPENCODE_PART_QUERY),
        str(TRANSCRIPT_BLOCK_LIMIT * 4),
    ])
    try:
        result = subprocess.run(
            ["ssh", "-o", "ConnectTimeout=5", "-o", "BatchMode=yes", remote, remote_cmd],
            capture_output=True, text=True, timeout=15,
        )
        if result.returncode != 0 or not result.stdout.strip():
            return None
        return json.loads(result.stdout)
    except Exception:
        return None


def opencode_to_blocks(document, limit=TRANSCRIPT_BLOCK_LIMIT):
    """Map OpenCode message parts into OutputBlock dictionaries."""
    if not isinstance(document, dict):
        return []
    blocks = []

    def add(kind, **kw):
        kw["id"] = f"o{len(blocks)}"
        kw["kind"] = kind
        blocks.append(kw)

    for row in document.get("rows") or []:
        if not isinstance(row, (list, tuple)) or len(row) != 2:
            continue
        role, raw_part = row
        try:
            part = json.loads(raw_part) if isinstance(raw_part, str) else raw_part
        except Exception:
            continue
        if not isinstance(part, dict):
            continue
        part_type = part.get("type")
        text = part.get("text")
        if role == "user" and part_type == "text" and isinstance(text, str) and text.strip():
            add("status", label="You", text=text.strip()[:2000])
        elif role == "assistant" and part_type == "text" and isinstance(text, str) and text.strip():
            add("assistant_text", markdown=text)
        elif role == "assistant" and part_type == "reasoning" and isinstance(text, str) and text.strip():
            add("status", label="Thought", text=text.strip().splitlines()[0][:200])
        elif role == "assistant" and part_type == "tool":
            state = part.get("state") if isinstance(part.get("state"), dict) else {}
            summary = summarize_tool(state.get("input")) or str(state.get("title") or "")[:200]
            add("tool", label=part.get("tool") or "tool", text=summary)
    return blocks[-limit:]


def pane_blocks(pane_id):
    """(blocks, signature) for a Claude pane's transcript, else (None, None)."""
    info = pane_cwd_map.get(pane_id)
    if not info:
        return None, None
    cwd, agent, remote, ambiguous = info
    # Without an agent session id/path, cwd is the only correlation available.
    # Refuse structured output when multiple live same-agent panes share it rather
    # than risk showing one conversation in another pane.
    if agent not in ("claude", "opencode") or not cwd or ambiguous:
        return None, None
    if agent == "claude":
        try:
            path, body = read_transcript(cwd, remote)
        except Exception:
            return None, None
        if not body:
            return None, None
        return transcript_to_blocks(body), hash((path, body))
    document = read_opencode(cwd, remote)
    if not document:
        return None, None
    blocks = opencode_to_blocks(document)
    return blocks, hash(json.dumps(document, sort_keys=True))


async def broadcast(msg):
    data = json.dumps(msg)
    dead = set()
    for ws in clients:
        try:
            await ws.send(data)
        except Exception:
            dead.add(ws)
    clients.difference_update(dead)


async def poll_loop():
    while True:
        agents, hosts = get_all_agents()
        current_pane_ids = {a["pane_id"] for a in agents}
        pane_remote_map.clear()
        session_target_map.clear()
        pane_cwd_map.clear()
        agent_cwd_counts = {}
        for a in agents:
            if a.get("agent") in ("claude", "opencode") and a.get("cwd"):
                cwd_key = (a.get("remote"), a["cwd"], a["agent"])
                agent_cwd_counts[cwd_key] = agent_cwd_counts.get(cwd_key, 0) + 1
        for a in agents:
            pane_remote_map[a["pane_id"]] = a.get("remote")
            session_target_map[session_id(a["host"], a["pane_id"])] = (a["pane_id"], a.get("remote"))
            cwd_key = (a.get("remote"), a.get("cwd", ""), a.get("agent", ""))
            pane_cwd_map[a["pane_id"]] = (
                a.get("cwd", ""), a.get("agent", ""), a.get("remote"),
                agent_cwd_counts.get(cwd_key, 0) > 1,
            )

        # Always send a complete snapshot. In particular, an empty snapshot
        # removes stale agents after every remote host goes offline.
        await broadcast({
            "type": "agents", "agents": agents,
            "presets": public_presets(),
            "hosts": hosts,
        })
        for a in agents:
            pid, status = a["pane_id"], a["status"]
            if status == "blocked" and last_statuses.get(pid) != "blocked":
                content = read_pane(pid, remote=a.get("remote"))
                options = detect_options(content)
                await broadcast({
                    "type": "blocked", "pane_id": pid,
                    "agent": a["agent"], "project": a["project"],
                    "host": a.get("host", "local"),
                    "prompt": content[:500],
                    "options": options or TOOL_OPTIONS
                })
            last_statuses[pid] = status
        for pid in set(last_statuses) - current_pane_ids:
            del last_statuses[pid]

        # Live-stream structured transcript blocks to subscribed clients. Only
        # watched Claude panes are read; a changed signature (path or content)
        # triggers a push. Failures are swallowed so one bad host can't stall.
        watchers = {
            pid: [ws for ws, subscribed_pid in list(subscriptions.items()) if subscribed_pid == pid]
            for pid in set(subscriptions.values()) if pid in current_pane_ids
        }
        pane_results = await asyncio.gather(
            *(asyncio.to_thread(pane_blocks, pid) for pid in watchers),
            return_exceptions=True,
        )
        for pid, result in zip(watchers, pane_results):
            if isinstance(result, Exception):
                continue
            blocks, sig = result
            if blocks is None:
                continue
            payload = json.dumps({"type": "pane_content", "pane_id": pid, "output_blocks": blocks})
            for ws in watchers[pid]:
                key = (id(ws), pid)
                if stream_sigs.get(key) == sig:
                    continue
                stream_sigs[key] = sig
                try:
                    await ws.send(payload)
                except Exception:
                    pass
        await asyncio.sleep(POLL_INTERVAL)


async def event_push():
    while True:
        event = await event_queue.get()
        pane_id = event.get("pane_id", "")
        status = event.get("status", "")
        host = event.get("host", "local")

        if status == "blocked" and pane_id:
            remote = pane_remote_map.get(pane_id)
            if remote or host == "local":
                content = read_pane(pane_id, remote=remote)
            else:
                content = event.get("prompt", "Agent is blocked")
            options = detect_options(content)
            await broadcast({
                "type": "blocked", "pane_id": pane_id,
                "agent": event.get("agent", ""),
                "project": event.get("project", ""),
                "host": host,
                "prompt": content[:500],
                "options": options or TOOL_OPTIONS
            })

        if pane_id and event.get("type") == "agent_event":
            await broadcast({
                "type": "agents", "agents": [{
                    "pane_id": pane_id,
                    "agent": event.get("agent", ""),
                    "status": status,
                    "cwd": event.get("cwd", ""),
                    "project": event.get("project", ""),
                    "host": host,
                }]
            })


async def process_request(connection, request):
    """Handle HTTP POST on the same port as WebSocket."""
    from websockets.http11 import Response
    from websockets.datastructures import Headers

    # Token auth (if configured)
    if AUTH_TOKEN:
        token = None
        for key, value in request.headers.raw_items():
            if key.lower() == "authorization":
                token = value.replace("Bearer ", "")
        # Also check query param ?token=
        if not token and "token=" in (request.path or ""):
            import urllib.parse
            _, qs = request.path.split("?", 1) if "?" in request.path else (request.path, "")
            params = urllib.parse.parse_qs(qs)
            token = params.get("token", [None])[0]
        if token != AUTH_TOKEN:
            headers = Headers([("Content-Type", "text/plain")])
            return Response(401, "Unauthorized", headers, b"Invalid token\n")

    # Check if this is a WebSocket upgrade
    upgrade = None
    for key, value in request.headers.raw_items():
        if key.lower() == "upgrade":
            upgrade = value.lower()
    if upgrade == "websocket":
        return None  # proceed with WebSocket handshake

    # For CORS preflight
    if request.path and "OPTIONS" in str(request.headers):
        headers = Headers([
            ("Access-Control-Allow-Origin", "*"),
            ("Access-Control-Allow-Methods", "POST, OPTIONS"),
            ("Access-Control-Allow-Headers", "Content-Type"),
        ])
        return Response(204, "No Content", headers, b"")

    # HTTP POST — parse event from URL query params as fallback
    # (since we can't read request body in websockets 16)
    # Plugins should encode payload in the URL path: POST /push?payload=...
    import urllib.parse
    if "?" in (request.path or ""):
        _, qs = request.path.split("?", 1)
        params = urllib.parse.parse_qs(qs)
        if "d" in params:
            try:
                event = json.loads(urllib.parse.unquote(params["d"][0]))
                event_queue.put_nowait(event)
            except Exception:
                pass

    headers = Headers([("Access-Control-Allow-Origin", "*")])
    return Response(200, "OK", headers, b"ok\n")


async def handle_client(ws):
    clients.add(ws)
    try:
        async for raw in ws:
            try:
                msg = json.loads(raw)
            except json.JSONDecodeError:
                continue
            msg_type = msg.get("type")
            request_id = msg.get("request_id")
            if request_id and request_id in request_results:
                await ws.send(json.dumps(request_results[request_id]))
            elif msg_type == "launch_session":
                response = launch_session(msg)
                if request_id:
                    request_results[request_id] = response
                    if len(request_results) > 512:
                        request_results.pop(next(iter(request_results)))
                await ws.send(json.dumps(response))
            elif msg_type == "terminate_session":
                response = terminate_session(msg)
                if request_id:
                    request_results[request_id] = response
                    if len(request_results) > 512:
                        request_results.pop(next(iter(request_results)))
                await ws.send(json.dumps(response))
            elif msg_type == "respond":
                pane_id = msg["pane_id"]
                remote = pane_remote_map.get(pane_id)
                kind, payload = respond_action(msg.get("text", ""))
                if kind == "keys":
                    run_herdr("pane", "send-keys", pane_id, *payload, remote=remote)
                else:
                    run_herdr("pane", "send-text", pane_id, payload + "\n", remote=remote)
            elif msg_type == "agent_event":
                event_queue.put_nowait(msg)
            elif msg_type == "read_pane":
                pane_id = msg["pane_id"]
                lines = msg.get("lines", "30")
                remote = pane_remote_map.get(pane_id)
                content = run_herdr("pane", "read", pane_id, "--lines", str(lines), "--source", "recent", remote=remote)
                payload = {"type": "pane_content", "pane_id": pane_id, "content": content}
                # Include structured blocks on demand without changing which pane
                # this client explicitly subscribed to for live updates.
                try:
                    blocks, sig = await asyncio.to_thread(pane_blocks, pane_id)
                except Exception:
                    blocks, sig = None, None
                if blocks is not None:
                    payload["output_blocks"] = blocks
                    if subscriptions.get(ws) == pane_id:
                        stream_sigs[(id(ws), pane_id)] = sig
                await ws.send(json.dumps(payload))
            elif msg_type == "subscribe_pane":
                pane_id = msg.get("pane_id")
                if pane_id in pane_cwd_map:
                    previous = subscriptions.get(ws)
                    subscriptions[ws] = pane_id
                    if previous is not None:
                        stream_sigs.pop((id(ws), previous), None)
                    try:
                        blocks, sig = await asyncio.to_thread(pane_blocks, pane_id)
                    except Exception:
                        blocks, sig = None, None
                    if blocks is not None:
                        stream_sigs[(id(ws), pane_id)] = sig
                        await ws.send(json.dumps({"type": "pane_content", "pane_id": pane_id, "output_blocks": blocks}))
            elif msg_type == "unsubscribe_pane":
                previous = subscriptions.pop(ws, None)
                if previous is not None:
                    stream_sigs.pop((id(ws), previous), None)
            elif msg_type == "send_keys":
                pane_id = msg["pane_id"]
                keys = msg.get("keys", [])
                remote = pane_remote_map.get(pane_id)
                run_herdr("pane", "send-keys", pane_id, *keys, remote=remote)
            elif msg_type == "send_text":
                pane_id = msg["pane_id"]
                text = msg.get("text", "")
                remote = pane_remote_map.get(pane_id)
                run_herdr("pane", "send-text", pane_id, text, remote=remote)
    finally:
        clients.discard(ws)
        subscriptions.pop(ws, None)
        for key in [k for k in stream_sigs if k[0] == id(ws)]:
            stream_sigs.pop(key, None)


def command_error(request_id, code, message):
    return {"type": "command_error", "request_id": request_id, "code": code, "message": message}


def launch_session(msg):
    request_id = msg.get("request_id")
    if not isinstance(request_id, str) or not request_id:
        return command_error(None, "INVALID_REQUEST", "request_id is required")
    preset = PRESETS_BY_ID.get(msg.get("preset_id"))
    if not preset:
        return command_error(request_id, "UNKNOWN_PRESET", "Unknown preset")
    host_id = msg.get("host_id")
    host = preset["hosts"].get(host_id)
    if not host:
        return command_error(request_id, "HOST_NOT_ALLOWED", "Preset is not allowed on this host")
    remote = host.get("target")
    agent = preset["agent"]
    argv = [agent]
    if preset["model"] != "default":
        argv.extend(["--model", preset["model"]])
    name = f"mobile-{preset['id']}-{uuid.uuid4().hex[:8]}"
    success, output = run_herdr_checked("agent", "start", name, "--cwd", host["cwd"], "--no-focus", "--", *argv, remote=remote)
    if not success:
        return command_error(request_id, "LAUNCH_FAILED", "Herdr did not start the client")
    return {"type": "command_ack", "request_id": request_id, "result": {"host_id": host_id}}


def terminate_session(msg):
    request_id = msg.get("request_id")
    if not isinstance(request_id, str) or not request_id:
        return command_error(None, "INVALID_REQUEST", "request_id is required")
    if not isinstance(msg.get("confirmation_nonce"), str) or not msg["confirmation_nonce"]:
        return command_error(request_id, "CONFIRMATION_REQUIRED", "confirmation_nonce is required")
    target = session_target_map.get(msg.get("session_id"))
    if not target:
        return command_error(request_id, "STALE_SESSION", "Session is no longer active")
    pane_id, remote = target
    success, output = run_herdr_checked("pane", "close", pane_id, remote=remote)
    if not success:
        return command_error(request_id, "TERMINATE_FAILED", "Herdr did not terminate the client")
    session_target_map.pop(msg["session_id"], None)
    return {"type": "command_ack", "request_id": request_id, "result": {"output": output}}


class UDPPlugin(asyncio.DatagramProtocol):
    def datagram_received(self, data, addr):
        try:
            event_queue.put_nowait(json.loads(data.decode()))
        except Exception:
            pass


def start_mdns():
    try:
        from zeroconf import Zeroconf, ServiceInfo
        import socket as sock_mod
        import threading
        ip = sock_mod.gethostbyname(sock_mod.gethostname())
        info = ServiceInfo(
            "_herdr-remote._tcp.local.", "herdr-remote._herdr-remote._tcp.local.",
            addresses=[sock_mod.inet_aton(ip)], port=WS_PORT,
        )
        zc = Zeroconf()
        threading.Thread(target=zc.register_service, args=(info,), daemon=True).start()
        print(f"mDNS registering at {ip}")
        return zc, info
    except Exception as e:
        print(f"mDNS skipped: {e}")
        return None, None


async def main():
    zc, info = start_mdns()
    loop = asyncio.get_running_loop()
    try:
        await loop.create_datagram_endpoint(UDPPlugin, local_addr=("127.0.0.1", 8376))
    except OSError:
        print("UDP 8376 in use, plugin push disabled")
    asyncio.create_task(poll_loop())
    asyncio.create_task(event_push())
    server = await serve(handle_client, "0.0.0.0", WS_PORT, process_request=process_request)
    hosts = ["local"] + REMOTES
    print(f"herdr-remote relay on :{WS_PORT} (WebSocket + HTTP POST)")
    print(f"  polling: {', '.join(hosts)}")
    stop = loop.create_future()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, stop.set_result, None)
    await stop
    server.close()
    if zc and info:
        zc.unregister_service(info)
        zc.close()


if __name__ == "__main__":
    asyncio.run(main())
