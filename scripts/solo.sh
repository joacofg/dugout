#!/usr/bin/env bash
# Dugout on one Mac: one person, several Claude Code sessions in parallel.
# serve listens on loopback as its own LaunchAgent, with a token for this
# person and the repos given; the daemon is installed against it, and
# `dugout office` opens the office view of this Mac. Nothing leaves the laptop.
#
# Run it from the dugout repo. It changes this Mac: two LaunchAgents, the
# token in the Keychain, the binary in $(go env GOPATH)/bin. The plugin is
# installed by hand afterwards, as the script prints. To undo it:
# dugout uninstall; launchctl bootout "gui/$(id -u)/com.dugout.serve"; and
# remove ~/Library/LaunchAgents/com.dugout.serve.plist.
#
# Usage: scripts/solo.sh <repo>...   (e.g. github.com/acme/app)
set -euo pipefail

[[ $# -ge 1 ]] || { echo "usage: $0 <repo>... (e.g. github.com/acme/app)" >&2; exit 2; }
[[ $(uname) == Darwin ]] || { echo "solo.sh is macOS only" >&2; exit 1; }

ADDR=127.0.0.1:7457
SERVER=http://$ADDR
STATE="$HOME/Library/Application Support/dugout-serve"
LABEL=com.dugout.serve
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOG="$HOME/Library/Logs/dugout/serve.log"

go install ./cmd/dugout
BIN="$(go env GOPATH)/bin/dugout"
"$BIN" version

mkdir -p "$STATE" "$(dirname "$LOG")" "$(dirname "$PLIST")"
chmod 700 "$STATE"
first=false
if [[ ! -f "$STATE/serve.json" ]]; then
  first=true
  # Tokens start with dgt_; a hex-only pattern picks another value (a misleading 401).
  "$BIN" admin init -state "$STATE/serve.json" | grep -o 'dgt_[A-Za-z0-9_-]*' >"$STATE/admin-token"
  chmod 600 "$STATE/admin-token"
fi

cat >"$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$LABEL</string>
	<key>ProgramArguments</key>
	<array>
		<string>$BIN</string>
		<string>serve</string>
		<string>-addr</string>
		<string>$ADDR</string>
		<string>-state</string>
		<string>$STATE/serve.json</string>
		<string>-log-format</string>
		<string>json</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>$LOG</string>
	<key>StandardErrorPath</key>
	<string>$LOG</string>
</dict>
</plist>
EOF
launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST"
for _ in $(seq 50); do
  curl -s -o /dev/null "$SERVER/" && break
  sleep 0.1
done

export DUGOUT_SERVER=$SERVER
DUGOUT_ADMIN_TOKEN=$(cat "$STATE/admin-token")
export DUGOUT_ADMIN_TOKEN
for repo in "$@"; do
  "$BIN" admin repo add "$repo"
done
# The person's token, once. A later run keeps the one in the Keychain.
if $first; then
  "$BIN" admin token issue "$(id -un)" | grep -o 'dgt_[A-Za-z0-9_-]*' | "$BIN" login -server "$SERVER"
fi
"$BIN" install -server "$SERVER"
sleep 1
"$BIN" doctor || true

cat <<EOF

serve: $SERVER (LaunchAgent $LABEL, log $LOG)
state: $STATE (serve.json, admin-token)
The plugin is still missing, in Claude Code:
  /plugin marketplace add joacofg/dugout
  /plugin install dugout@dugout
EOF
