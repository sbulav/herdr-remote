#!/usr/bin/env node
// .pi-kiro-auth.js — Bridge kiro-cli credentials to Pi
// Usage: node .pi-kiro-auth.js
// Prerequisites: kiro-cli installed and logged in, Node 22+

const { join } = require("path");
const { homedir, platform } = require("os");
const { writeFileSync, mkdirSync, existsSync, chmodSync } = require("fs");
const { DatabaseSync } = require("node:sqlite");

const p = platform();
const dbPath = p === "darwin"
  ? join(homedir(), "Library/Application Support/kiro-cli/data.sqlite3")
  : p === "win32"
    ? join(process.env.APPDATA || join(homedir(), "AppData", "Roaming"), "kiro-cli", "data.sqlite3")
    : join(homedir(), ".local/share/kiro-cli/data.sqlite3");

if (!existsSync(dbPath)) {
  console.error("kiro-cli not found. Run: kiro-cli login");
  process.exit(1);
}

const db = new DatabaseSync(dbPath, { readOnly: true });
const tokenRows = db.prepare("SELECT value FROM auth_kv WHERE key='kirocli:odic:token'").all();
const regRows = db.prepare("SELECT value FROM auth_kv WHERE key='kirocli:odic:device-registration'").all();
db.close();

if (!tokenRows[0]) {
  console.error("No token found. Run: kiro-cli login");
  process.exit(1);
}

const token = JSON.parse(tokenRows[0].value);
const reg = JSON.parse(regRows[0].value);

async function main() {
  let profileArn = "";
  for (const region of ["us-east-1", "eu-central-1", "ap-northeast-1"]) {
    try {
      const r = await fetch("https://q." + region + ".amazonaws.com/", {
        method: "POST",
        headers: {
          Authorization: "Bearer " + token.access_token,
          "Content-Type": "application/x-amz-json-1.0",
          "X-Amz-Target": "AmazonCodeWhispererService.ListAvailableProfiles",
        },
        body: "{}",
      });
      const j = await r.json();
      const arn = j.profiles?.find((p) => p.arn)?.arn;
      if (arn) { profileArn = arn; break; }
    } catch {}
  }

  if (!profileArn) {
    console.error("Could not resolve profile. Run: kiro-cli debug refresh-auth-token");
    process.exit(1);
  }

  const apiRegion = profileArn.split(":")[3] || "us-east-1";
  const piAuth = {
    kiro: {
      type: "oauth",
      access: token.access_token,
      refresh: [token.refresh_token, reg.client_id, reg.client_secret, "idc"].join("|"),
      expires: new Date(token.expires_at).getTime(),
      clientId: reg.client_id,
      clientSecret: reg.client_secret,
      region: apiRegion,
      authMethod: "idc",
      profileArn,
    },
  };

  const authDir = join(homedir(), ".pi/agent");
  const authPath = join(authDir, "auth.json");
  mkdirSync(authDir, { recursive: true, mode: 0o700 });
  writeFileSync(authPath, JSON.stringify(piAuth, null, 2), { mode: 0o600 });
  chmodSync(authPath, 0o600);

  console.log("Pi configured for Kiro.");
  console.log("  Region:", apiRegion);
  console.log("  Expires:", new Date(piAuth.kiro.expires).toISOString());
}

main();
