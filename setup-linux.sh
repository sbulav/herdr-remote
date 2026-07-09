#!/bin/bash
# herdr-remote full setup for WSL2/Linux
# Run: curl -sL https://raw.githubusercontent.com/dcolinmorgan/herdr-remote/main/setup-linux.sh | bash
set -e

echo "=== herdr-remote full setup ==="
echo ""

# 1. Install herdr
echo "[1/5] Installing herdr..."
if ! command -v herdr &>/dev/null; then
    curl -fsSL https://herdr.dev/install.sh | bash
    export PATH="$HOME/.local/bin:$PATH"
    echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc
fi
herdr --version && echo "  herdr OK" || echo "  herdr FAILED"

# 2. Install herdr-push plugin
echo ""
echo "[2/5] Installing herdr-push plugin..."
herdr plugin install dcolinmorgan/herdr-push --yes 2>/dev/null || herdr plugin install dcolinmorgan/herdr-push

# 3. Install kiro-cli
echo ""
echo "[3/5] Installing kiro-cli..."
if ! command -v kiro-cli &>/dev/null; then
    curl -fsSL https://kiro.dev/install.sh | bash
    export PATH="$HOME/.local/bin:$PATH"
fi
kiro-cli --version && echo "  kiro-cli OK" || echo "  kiro-cli FAILED"

# 4. Install pi
echo ""
echo "[4/5] Installing pi..."
if ! command -v node &>/dev/null; then
    echo "  Installing Node.js first..."
    curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -
    sudo apt-get install -y nodejs
fi
if ! command -v pi &>/dev/null; then
    npm install -g @earendil-works/pi-coding-agent
fi
pi --version && echo "  pi OK" || echo "  pi FAILED"

# Install pi-provider-kiro
pi install npm:pi-provider-kiro 2>/dev/null && echo "  pi-provider-kiro OK"

# 5. Install Telegram Desktop
echo ""
echo "[5/5] Installing Telegram..."
if ! command -v telegram-desktop &>/dev/null; then
    if command -v snap &>/dev/null; then
        sudo snap install telegram-desktop
    elif command -v apt &>/dev/null; then
        sudo apt install -y telegram-desktop
    elif command -v flatpak &>/dev/null; then
        flatpak install -y flathub org.telegram.desktop
    else
        echo "  Install Telegram manually: https://desktop.telegram.org"
    fi
fi
echo "  Telegram installed (log in manually)"

echo ""
echo "=== Setup complete ==="
echo ""
echo "Next steps:"
echo "  1. Log into kiro-cli:"
echo "     kiro-cli login"
echo ""
echo "  2. Bridge kiro creds to pi:"
echo "     node -e \"\$(curl -s https://raw.githubusercontent.com/dcolinmorgan/herdr-remote/main/pi-kiro-auth.js)\""
echo ""
echo "  3. Configure herdr-push (get relay URL from the relay machine):"
echo "     echo 'HERDR_RELAY=https://your-tunnel.trycloudflare.com' > \"\$(herdr plugin config-dir herdr.push)/.env\""
echo "     herdr plugin action invoke herdr.push test"
echo ""
echo "  4. Open Telegram and log into the @kiro_notbot chat"
echo ""
echo "  5. Start herdr:"
echo "     herdr"
echo ""
echo "  6. Start pi:"
echo "     pi"
echo "     /model auto"
