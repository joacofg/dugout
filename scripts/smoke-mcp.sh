#!/usr/bin/env bash
# Manual smoke test of the MCP surface with a real Claude Code session: the
# plugin's MCP server, an intent declared, who_touches answered inside
# <dugout-context> and dugout status.
# Same pattern as scripts/smoke-claude.sh. It costs real money
# (SMOKE_BUDGET_USD for the agent session).
#
# It builds dugout into plugin/bin, sets up an origin with a clone (A, on
# feat-a) and a linked worktree (B, on feat-b), runs serve and one daemon,
# and leaves shared.txt changed in A. Then it asks `claude -p` in B to declare
# its intent, edit shared.txt and ask who touches it. Then `dugout status`
# must show that intent.
#
# Usage: scripts/smoke-mcp.sh   (SMOKE_MODEL, SMOKE_BUDGET_USD, SMOKE_PORT)
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
MODEL=${SMOKE_MODEL:-sonnet}
BUDGET=${SMOKE_BUDGET_USD:-0.50}
PORT=${SMOKE_PORT:-47467}
WORK=$(mktemp -d "${TMPDIR:-/tmp}/dugout-smoke4.XXXXXX")
export DUGOUT_HOME
DUGOUT_HOME=$(mktemp -d /tmp/dg4.XXXXXX) # short: unix socket paths cap at 104 bytes
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=smoke GIT_AUTHOR_EMAIL=smoke@local GIT_COMMITTER_NAME=smoke GIT_COMMITTER_EMAIL=smoke@local

# dugout-run (versioned) finds the built binary next to itself when dugout is not on PATH.
go build -C "$ROOT" -o "$ROOT/plugin/bin/dugout" ./cmd/dugout
DUGOUT="$ROOT/plugin/bin/dugout"

git init -q --bare -b main "$WORK/origin.git"
git init -q -b main "$WORK/seed"
printf 'original line\n' >"$WORK/seed/shared.txt"
git -C "$WORK/seed" add -A
git -C "$WORK/seed" commit -qm init
git -C "$WORK/seed" push -q "$WORK/origin.git" main
git clone -q "$WORK/origin.git" "$WORK/A"
git -C "$WORK/A" switch -q -c feat-a
git -C "$WORK/A" worktree add -q -b feat-b "$WORK/B"

"$DUGOUT" serve -addr "127.0.0.1:$PORT" -log-format json 2>"$WORK/serve.log" &
SERVE_PID=$!
"$DUGOUT" daemon -server "http://127.0.0.1:$PORT" -poll 1s -log-format json 2>"$WORK/daemon.log" &
DAEMON_PID=$!
trap 'kill "$SERVE_PID" "$DAEMON_PID" 2>/dev/null || true' EXIT
sleep 1

printf 'original line\nchange from A\n' >"$WORK/A/shared.txt"
"$DUGOUT" track "$WORK/A" >/dev/null
"$DUGOUT" track "$WORK/B" >/dev/null

PROMPT='This is a Dugout integration test. Do these steps in order:
1. Declare your intent with Dugout'"'"'s office_declare_intent tool: "adding a test line to shared.txt", with the session_id Dugout gave you at the start.
2. With the Edit tool, add the line "one" at the end of shared.txt.
3. Run exactly `sleep 3` with the Bash tool.
4. Call Dugout'"'"'s office_who_touches tool with the path shared.txt.
5. At the end, say in one line what each Dugout tool answered and whether you saw a dugout-context block.'

# Hooks run plugin/bin/dugout-run; this puts the binary just built first on PATH.
export PATH="$ROOT/plugin/bin:$PATH"

(cd "$WORK/B" && command claude -p "$PROMPT" \
  --plugin-dir "$ROOT/plugin" \
  --setting-sources project \
  --permission-mode acceptEdits \
  --allowedTools "Read,Edit,Bash(sleep 3),mcp__plugin_dugout_dugout" \
  --model "$MODEL" \
  --max-budget-usd "$BUDGET" \
  --output-format stream-json --verbose \
  >"$WORK/agent.stream.jsonl" 2>"$WORK/agent.stderr") || echo "claude exited $?" >>"$WORK/agent.stderr"

"$DUGOUT" status >"$WORK/status.txt" 2>&1 || true

ok=1
check() { # check <description> <command...>
  local what=$1
  shift
  if "$@"; then
    echo "PASS: $what"
  else
    echo "FAIL: $what"
    ok=0
  fi
}
# The init event of the stream lists the MCP servers and the tools.
mcp_connected() { grep -q '"name":"plugin:dugout:dugout","status":"connected"' "$WORK/agent.stream.jsonl"; }
mcp_tools() {
  grep -q 'mcp__plugin_dugout_dugout__office_declare_intent' "$WORK/agent.stream.jsonl" &&
    grep -q 'mcp__plugin_dugout_dugout__office_who_touches' "$WORK/agent.stream.jsonl"
}
intent_declared() { grep -q 'intent declared' "$WORK/daemon.log"; }
# The tool result of who_touches is a user message of the stream.
who_touches_names_a() { grep 'tool_result' "$WORK/agent.stream.jsonl" | grep '<dugout-context>' | grep -q 'feat-a'; }
status_has_intent() { grep -q 'Intent: "adding a test line to shared.txt"' "$WORK/status.txt"; }

check "the plugin's MCP server is connected" mcp_connected
check "it exposes office_declare_intent and office_who_touches" mcp_tools
check "the agent declared its intent (the daemon logged it)" intent_declared
check "office_who_touches answered inside <dugout-context> naming feat-a" who_touches_names_a
check "dugout status shows the intent" status_has_intent
echo "logs: $WORK"
[[ $ok == 1 ]]
