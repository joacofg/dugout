#!/usr/bin/env bash
# Manual smoke test of the hooks with a real Claude Code session.
# It costs real money (SMOKE_BUDGET_USD).
#
# It builds dugout into plugin/bin, sets up an origin with a clone (A, on
# feat-a) and a linked worktree (B, on feat-b), runs serve and one daemon,
# and leaves shared.txt changed in A. Then it asks `claude -p` in B to edit
# shared.txt, wait, and edit it again. Dugout warns only when both sides
# have the file in flight, so the first edit goes through. The second one
# must get a Dugout deny, and the retry must apply it.
#
# Usage: scripts/smoke-claude.sh   (SMOKE_MODEL, SMOKE_BUDGET_USD, SMOKE_PORT)
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
MODEL=${SMOKE_MODEL:-sonnet}
BUDGET=${SMOKE_BUDGET_USD:-0.50}
PORT=${SMOKE_PORT:-47457}
WORK=$(mktemp -d "${TMPDIR:-/tmp}/dugout-smoke.XXXXXX")
export DUGOUT_HOME
DUGOUT_HOME=$(mktemp -d /tmp/dgs.XXXXXX) # short: unix socket paths cap at 104 bytes
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

"$DUGOUT" serve -addr "127.0.0.1:$PORT" 2>"$WORK/serve.log" &
SERVE_PID=$!
"$DUGOUT" daemon -server "http://127.0.0.1:$PORT" -poll 1s 2>"$WORK/daemon.log" &
DAEMON_PID=$!
trap 'kill "$SERVE_PID" "$DAEMON_PID" 2>/dev/null || true' EXIT
sleep 1

printf 'original line\nchange from A\n' >"$WORK/A/shared.txt"
"$DUGOUT" track "$WORK/A"
"$DUGOUT" track "$WORK/B"

PROMPT='This is an integration test. Do these steps in order:
1. With the Edit tool, add the line "one" at the end of shared.txt.
2. Run exactly `sleep 4` with the Bash tool.
3. With the Edit tool, add the line "two" at the end of shared.txt. If the edit is rejected, read the reason and retry it just once.
4. At the end, say in one line whether you saw any dugout-context block and what it said.'

# Hooks run plugin/bin/dugout-run; this puts the binary just built first on PATH.
export PATH="$ROOT/plugin/bin:$PATH"

(cd "$WORK/B" && command claude -p "$PROMPT" \
  --plugin-dir "$ROOT/plugin" \
  --setting-sources project \
  --permission-mode acceptEdits \
  --allowedTools "Read,Edit,Bash(sleep 4)" \
  --model "$MODEL" \
  --max-budget-usd "$BUDGET" \
  --output-format stream-json --verbose \
  >"$WORK/claude.stream.jsonl" 2>"$WORK/claude.stderr") || echo "claude exited $?" >>"$WORK/claude.stderr"

ok=1
if grep -q 'Dugout held this edit' "$WORK/claude.stream.jsonl"; then
  echo "PASS: the second edit of shared.txt got a Dugout deny"
else
  echo "FAIL: no Dugout deny in the session"
  ok=0
fi
if grep -q '^two$' "$WORK/B/shared.txt"; then
  echo "PASS: the retry applied the edit"
else
  echo "FAIL: shared.txt in B does not have the retried line"
  ok=0
fi
echo "logs: $WORK"
[[ $ok == 1 ]]
