#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = ["python-telegram-bot>=21.0", "websockets>=14.0"]
# ///
"""herdr-remote Telegram bot — monitor and approve agents from Telegram."""
import asyncio, json, os, logging

from telegram import Update, InlineKeyboardButton, InlineKeyboardMarkup
from telegram.ext import Application, CallbackQueryHandler, CommandHandler, MessageHandler, ContextTypes, filters

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("herdr-tg")

TOKEN = os.environ.get("HERDR_TG_TOKEN", "")
CHAT_ID = os.environ.get("HERDR_TG_CHAT_ID", "")
RELAY_WS = os.environ.get("HERDR_RELAY", "ws://127.0.0.1:8375")

if not TOKEN:
    print("Set HERDR_TG_TOKEN (from @BotFather)")
    exit(1)

# State
pending: dict[int, str] = {}  # message_id -> pane_id
agents: list[dict] = []       # current agent list from relay
relay_connected = False


# --- Relay communication ---

async def send_to_relay(pane_id: str, text: str):
    """Send a response to the relay via WebSocket."""
    import websockets
    try:
        async with websockets.connect(RELAY_WS) as ws:
            await ws.send(json.dumps({"type": "respond", "pane_id": pane_id, "text": text}))
    except Exception as e:
        log.warning(f"Failed to send to relay: {e}")


async def read_pane(pane_id: str, lines: int = 15) -> str:
    """Read pane content from relay."""
    import websockets
    try:
        async with websockets.connect(RELAY_WS) as ws:
            await ws.send(json.dumps({"type": "read_pane", "pane_id": pane_id, "lines": lines}))
            raw = await asyncio.wait_for(ws.recv(), timeout=5)
            msg = json.loads(raw)
            # Might get an agents broadcast first, skip to pane_content
            for _ in range(5):
                if msg.get("type") == "pane_content":
                    return msg.get("content", "(empty)")
                raw = await asyncio.wait_for(ws.recv(), timeout=3)
                msg = json.loads(raw)
    except Exception as e:
        return f"(error reading pane: {e})"
    return "(no response)"


# --- Bot commands ---

async def cmd_start(update: Update, ctx: ContextTypes.DEFAULT_TYPE):
    """Handle /start command."""
    await update.message.reply_text(
        "herdr-remote bot\n\n"
        "Commands:\n"
        "/agents — list all agents\n"
        "/status — relay connection info\n"
        "/read — read last output from an agent\n"
        "/interrupt — send Ctrl+C to an agent\n\n"
        f"Chat ID: `{update.effective_chat.id}`",
        parse_mode="Markdown"
    )


async def cmd_agents(update: Update, ctx: ContextTypes.DEFAULT_TYPE):
    """Handle /agents — list current agents with status."""
    if not agents:
        await update.message.reply_text("No agents connected." if relay_connected else "Not connected to relay.")
        return

    blocked = [a for a in agents if a.get("status") == "blocked"]
    working = [a for a in agents if a.get("status") == "working"]
    idle = [a for a in agents if a.get("status") in ("idle", "unknown")]

    lines = []
    if blocked:
        lines.append("BLOCKED:")
        for a in blocked:
            host = f" @{a['host']}" if a.get('host', 'local') != 'local' else ''
            lines.append(f"  {a['project']} ({a['agent']}){host}")
    if working:
        lines.append("WORKING:")
        for a in working:
            host = f" @{a['host']}" if a.get('host', 'local') != 'local' else ''
            lines.append(f"  {a['project']} ({a['agent']}){host}")
    if idle:
        lines.append("IDLE:")
        for a in idle:
            host = f" @{a['host']}" if a.get('host', 'local') != 'local' else ''
            lines.append(f"  {a['project']} ({a['agent']}){host}")

    await update.message.reply_text("\n".join(lines))


async def cmd_status(update: Update, ctx: ContextTypes.DEFAULT_TYPE):
    """Handle /status — show connection info."""
    b = len([a for a in agents if a.get("status") == "blocked"])
    w = len([a for a in agents if a.get("status") == "working"])
    i = len([a for a in agents if a.get("status") in ("idle", "unknown")])

    status = "Connected" if relay_connected else "Disconnected"
    text = (
        f"Relay: {RELAY_WS}\n"
        f"Status: {status}\n"
        f"Agents: {len(agents)} ({b} blocked, {w} working, {i} idle)"
    )
    await update.message.reply_text(text)


async def cmd_read(update: Update, ctx: ContextTypes.DEFAULT_TYPE):
    """Handle /read [project] — read pane output."""
    args = ctx.args
    if not args:
        # Show agent picker
        if not agents:
            await update.message.reply_text("No agents. Use /agents to check.")
            return
        keyboard = [[InlineKeyboardButton(
            f"{a['project']} ({a['agent']})",
            callback_data=json.dumps({"action": "read", "pane_id": a["pane_id"]})
        )] for a in agents[:8]]
        await update.message.reply_text("Read which agent?", reply_markup=InlineKeyboardMarkup(keyboard))
        return

    # Find agent by project name
    query = " ".join(args).lower()
    match = next((a for a in agents if query in a.get("project", "").lower() or query in a.get("agent", "").lower()), None)
    if not match:
        await update.message.reply_text(f"No agent matching '{query}'. Use /agents to see list.")
        return

    content = await read_pane(match["pane_id"])
    if len(content) > 3500:
        content = content[-3500:]
    await update.message.reply_text(f"{match['project']}:\n\n{content}")


async def cmd_interrupt(update: Update, ctx: ContextTypes.DEFAULT_TYPE):
    """Handle /interrupt [project] — send Ctrl+C."""
    args = ctx.args
    if not args:
        if not agents:
            await update.message.reply_text("No agents.")
            return
        working = [a for a in agents if a.get("status") in ("working", "blocked")]
        if not working:
            await update.message.reply_text("No active agents to interrupt.")
            return
        keyboard = [[InlineKeyboardButton(
            f"{a['project']} ({a['agent']})",
            callback_data=json.dumps({"action": "interrupt", "pane_id": a["pane_id"]})
        )] for a in working[:8]]
        await update.message.reply_text("Interrupt which agent?", reply_markup=InlineKeyboardMarkup(keyboard))
        return

    query = " ".join(args).lower()
    match = next((a for a in agents if query in a.get("project", "").lower() or query in a.get("agent", "").lower()), None)
    if not match:
        await update.message.reply_text(f"No agent matching '{query}'.")
        return

    import websockets
    try:
        async with websockets.connect(RELAY_WS) as ws:
            await ws.send(json.dumps({"type": "send_keys", "pane_id": match["pane_id"], "keys": ["Ctrl+c"]}))
        await update.message.reply_text(f"Sent Ctrl+C to `{match['project']}`", parse_mode="Markdown")
    except Exception as e:
        await update.message.reply_text(f"Failed: {e}")


# --- Callback handler (buttons) ---

async def handle_callback(update: Update, ctx: ContextTypes.DEFAULT_TYPE):
    """Handle inline keyboard button presses."""
    query = update.callback_query
    await query.answer()

    data = json.loads(query.data)
    action = data.get("action", "respond")

    if action == "read":
        content = await read_pane(data["pane_id"])
        if len(content) > 3500:
            content = content[-3500:]
        await query.message.reply_text(f"```\n{content}\n```", parse_mode="Markdown")
        return

    if action == "interrupt":
        import websockets
        try:
            async with websockets.connect(RELAY_WS) as ws:
                await ws.send(json.dumps({"type": "send_keys", "pane_id": data["pane_id"], "keys": ["Ctrl+c"]}))
            await query.message.reply_text("Sent Ctrl+C")
        except Exception as e:
            await query.message.reply_text(f"Failed: {e}")
        return

    # Default: respond to blocked agent
    pane_id = data["pane_id"]
    response = data["response"]
    await send_to_relay(pane_id, response)
    await query.edit_message_reply_markup(reply_markup=None)
    await query.message.reply_text(f"Sent: `{response}`", parse_mode="Markdown")


# --- Free text reply ---

async def handle_text(update: Update, ctx: ContextTypes.DEFAULT_TYPE):
    """Reply to a blocked notification to send custom text."""
    if not update.message.reply_to_message:
        return
    orig_id = update.message.reply_to_message.message_id
    pane_id = pending.get(orig_id)
    if not pane_id:
        await update.message.reply_text("Reply to a blocked notification to send text to that agent.")
        return
    await send_to_relay(pane_id, update.message.text)
    await update.message.reply_text("Sent")


# --- Blocked notification ---

TOOL_BUTTONS = [
    ("Yes (once)", "yes, single permission"),
    ("Trust (always)", "trust, always allow"),
    ("No", "no (tab to edit)"),
]

SUBAGENT_BUTTONS = [
    ("Approve all", "approve all pending"),
    ("Configure", "configure individually"),
    ("Cancel", "exit (cancel subagents)"),
]


def make_keyboard(pane_id: str, options: list[str] | None) -> InlineKeyboardMarkup:
    if options and "trust" in " ".join(options).lower():
        buttons = TOOL_BUTTONS
    elif options and "approve all" in " ".join(options).lower():
        buttons = SUBAGENT_BUTTONS
    else:
        buttons = [(opt.split(",")[0], opt) for opt in (options or ["yes, single permission", "no (tab to edit)"])]

    keyboard = [
        [InlineKeyboardButton(label, callback_data=json.dumps({"pane_id": pane_id, "response": resp}))]
        for label, resp in buttons
    ]
    return InlineKeyboardMarkup(keyboard)


async def notify_blocked(app: Application, pane_id: str, agent: str, project: str, prompt: str, options: list[str] | None):
    if not CHAT_ID:
        return
    text = f"*{agent}* blocked in `{project}`\n\n```\n{prompt[:400]}\n```"
    keyboard = make_keyboard(pane_id, options)
    msg = await app.bot.send_message(
        chat_id=int(CHAT_ID), text=text, parse_mode="Markdown", reply_markup=keyboard
    )
    pending[msg.message_id] = pane_id


# --- Relay listener ---

async def relay_listener(app: Application):
    """Persistent WebSocket connection to relay."""
    import websockets
    global agents, relay_connected

    while True:
        try:
            async with websockets.connect(RELAY_WS) as ws:
                relay_connected = True
                log.info(f"Connected to relay at {RELAY_WS}")
                async for raw in ws:
                    try:
                        msg = json.loads(raw)
                    except json.JSONDecodeError:
                        continue

                    if msg.get("type") == "agents":
                        agents = msg.get("agents", [])
                    elif msg.get("type") == "blocked":
                        await notify_blocked(
                            app,
                            pane_id=msg["pane_id"],
                            agent=msg.get("agent", "unknown"),
                            project=msg.get("project", ""),
                            prompt=msg.get("prompt", ""),
                            options=msg.get("options"),
                        )
        except Exception as e:
            relay_connected = False
            log.warning(f"Relay connection lost: {e}, reconnecting in 5s...")
            await asyncio.sleep(5)


# --- Main ---

def main():
    app = Application.builder().token(TOKEN).build()
    app.add_handler(CommandHandler("start", cmd_start))
    app.add_handler(CommandHandler("agents", cmd_agents))
    app.add_handler(CommandHandler("status", cmd_status))
    app.add_handler(CommandHandler("read", cmd_read))
    app.add_handler(CommandHandler("interrupt", cmd_interrupt))
    app.add_handler(CallbackQueryHandler(handle_callback))
    app.add_handler(MessageHandler(filters.TEXT & ~filters.COMMAND, handle_text))

    loop = asyncio.new_event_loop()
    asyncio.set_event_loop(loop)

    async def run():
        async with app:
            await app.start()
            await app.updater.start_polling()
            await relay_listener(app)

    loop.run_until_complete(run())


if __name__ == "__main__":
    main()
