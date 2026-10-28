# Helpers for docs/demo.tape, sourced by its shell from the repo root. They
# build dugout, start a team server on loopback with two people, alice and
# bob, each with their own daemon and clone of a sample repo, and play the
# hook and MCP calls Claude Code makes, so the recording needs no model.
# Nothing touches the real Dugout install: every daemon has its own
# DUGOUT_HOME and HOME. demo_stop ends it all.

DEMO_PORT=${DEMO_PORT:-47490}

demo_start() {
  DEMO=$(mktemp -d /tmp/dugout-demo.XXXXXX)
  DEMO_PIDS=()
  export DUGOUT_SERVER=http://127.0.0.1:$DEMO_PORT
  export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
  export GIT_AUTHOR_NAME=demo GIT_AUTHOR_EMAIL=demo@example.com GIT_COMMITTER_NAME=demo GIT_COMMITTER_EMAIL=demo@example.com
  go build -o "$DEMO/dugout" ./cmd/dugout || return 1
  mkdir -p "$DEMO/home"

  git init -q --bare -b main "$DEMO/origin.git"
  git init -q -b main "$DEMO/seed"
  cat >"$DEMO/seed/api.go" <<'EOF'
package api

import "net/http"

// Login checks the user's password and starts a session.
func Login(w http.ResponseWriter, r *http.Request) {
	user := r.FormValue("user")
	if !checkPassword(user, r.FormValue("password")) {
		http.Error(w, "wrong user or password", http.StatusUnauthorized)
		return
	}
	startSession(w, user)
}
EOF
  printf 'repo_id: github.com/acme/app\ntarget_branch: origin/main\n' >"$DEMO/seed/.dugout.yml"
  git -C "$DEMO/seed" add -A && git -C "$DEMO/seed" commit -qm "the login endpoint"
  git -C "$DEMO/seed" push -q "$DEMO/origin.git" main
  git clone -q "$DEMO/origin.git" "$DEMO/alice" && git -C "$DEMO/alice" switch -q -c feat-login-errors
  git clone -q "$DEMO/origin.git" "$DEMO/bob" && git -C "$DEMO/bob" switch -q -c feat-rate-limit
  git -C "$DEMO/alice" config user.name alice
  git -C "$DEMO/bob" config user.name bob

  local admin
  admin=$("$DEMO/dugout" admin init -state "$DEMO/serve.json" | grep -o 'dgt_[A-Za-z0-9_-]*')
  "$DEMO/dugout" serve -addr "127.0.0.1:$DEMO_PORT" -state "$DEMO/serve.json" 2>"$DEMO/serve.log" &
  DEMO_PIDS+=($!)
  _demo_wait curl -s -o /dev/null "$DUGOUT_SERVER/" || return 1
  DUGOUT_ADMIN_TOKEN=$admin "$DEMO/dugout" admin repo add github.com/acme/app >/dev/null
  local who
  for who in alice bob; do
    DUGOUT_ADMIN_TOKEN=$admin "$DEMO/dugout" admin token issue "$who" | grep -o 'dgt_[A-Za-z0-9_-]*' >"$DEMO/$who.token"
    mkdir -p "$DEMO/h-$who"
    HOME=$DEMO/home DUGOUT_HOME=$DEMO/h-$who DUGOUT_TOKEN=$(cat "$DEMO/$who.token") \
      "$DEMO/dugout" daemon -poll 300ms -log-format json 2>"$DEMO/$who.log" &
    DEMO_PIDS+=($!)
  done
  for who in alice bob; do
    _demo_wait test -S "$DEMO/h-$who/dugout.sock" || return 1
    _demo_hook "$who" SessionStart >/dev/null
  done
  cd "$DEMO/bob" || return 1
}

demo_stop() {
  cd "$DEMO/.." 2>/dev/null || cd /
  kill "${DEMO_PIDS[@]}" 2>/dev/null
  wait "${DEMO_PIDS[@]}" 2>/dev/null
  rm -rf "$DEMO"
}

# _demo_as <who> <command...>: as that person, on their own daemon.
_demo_as() {
  local who=$1
  shift
  HOME=$DEMO/home DUGOUT_HOME=$DEMO/h-$who DUGOUT_TOKEN=$(cat "$DEMO/$who.token") "$@"
}

_demo_wait() {
  local i
  for i in $(seq 100); do "$@" && return 0; sleep 0.1; done
  echo "demo: timed out: $*" >&2
  return 1
}

# _demo_hook <who> <event> [extra-json]: what Claude Code sends a hook.
_demo_hook() {
  local who=$1 event=$2 extra=${3:-'{}'}
  jq -nc --arg e "$event" --arg s "s-$who" --arg d "$DEMO/$who" --argjson x "$extra" \
    '{hook_event_name:$e,session_id:$s,cwd:$d}+$x' |
    (cd "$DEMO/$who" && _demo_as "$who" "$DEMO/dugout" hook "$event")
}

_demo_edit_json() { jq -nc --arg p "$DEMO/$1/$2" '{tool_name:"Edit",tool_input:{file_path:$p}}'; }

# intent <who> <text>: the agent declares its task with office_declare_intent.
intent() {
  local who=$1 text=$2
  {
    echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"demo","version":"1"}}}'
    jq -nc --arg s "s-$who" --arg t "$text" \
      '{jsonrpc:"2.0",id:2,method:"tools/call",params:{name:"office_declare_intent",arguments:{session_id:$s,summary:$t}}}'
  } | (cd "$DEMO/$who" && _demo_as "$who" "$DEMO/dugout" mcp) >/dev/null
  echo "$who's agent: \"$text\""
}

# edit <who> <file> <line> <text>: the agent changes one line, as an Edit
# tool call: PreToolUse, the write, PostToolUse. It prints what Dugout tells
# the agent and its human.
edit() {
  local who=$1 file=$2 n=$3 text=$4 out
  out=$(_demo_hook "$who" PreToolUse "$(_demo_edit_json "$who" "$file")")
  if [[ $(jq -r '.hookSpecificOutput.permissionDecision // empty' <<<"$out") == deny ]]; then
    echo "Dugout held $who's edit once. What the agent reads:"
    # The conflict and the other side's lines; the advice that follows is cut.
    jq -r '.hookSpecificOutput.permissionDecisionReason' <<<"$out" |
      sed '2d' | sed '/^Dugout held this edit/,$d' | sed -E 's/ \(daemon [0-9a-f]+\)//' |
      fold -s -w 96 | _demo_quote
    printf '  │ Dugout held this edit once so you know before editing. …\n  │ </dugout-context>\n'
    return
  fi
  # The new text keeps the line's indentation.
  awk -v n="$n" -v t="$text" 'NR==n{match($0,/^[ \t]*/); print substr($0,1,RLENGTH) t; next}{print}' \
    "$DEMO/$who/$file" >"$DEMO/edit.tmp" && mv "$DEMO/edit.tmp" "$DEMO/$who/$file"
  _demo_hook "$who" PostToolUse "$(_demo_edit_json "$who" "$file")" >/dev/null
  echo "$who's agent edited $file:$n"
}

# prompt <who>: the human sends the next prompt; Dugout's context comes with it.
prompt() {
  local who=$1 out i
  for i in $(seq 50); do
    out=$(_demo_hook "$who" UserPromptSubmit '{"prompt":"go on"}')
    if [[ $(jq -r '.hookSpecificOutput.additionalContext // empty' <<<"$out") == *conflict* ]]; then break; fi
    sleep 0.2
  done
  jq -r '.systemMessage // empty' <<<"$out"
}

_demo_quote() { sed 's/^/  │ /'; }

# dugout runs the demo's binary as bob, on his own daemon ($DEMO_AS picks
# another person).
dugout() { _demo_as "${DEMO_AS:-bob}" "$DEMO/dugout" "$@"; }

# office_shot <png>: a screenshot of the office view as it is now, with
# headless Chrome ($DEMO_CHROME), for demo_compose.
office_shot() {
  local chrome=${DEMO_CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}
  "$chrome" --headless=new --hide-scrollbars --window-size=1200,720 --force-device-scale-factor=1 \
    --virtual-time-budget=4000 --screenshot="$1" "$(dugout office -no-open | head -1)" >/dev/null 2>&1
}

# demo_compose <terminal.mp4> <office.png> <out.gif>: the terminal recording,
# then the office view held for a few seconds, as one GIF.
demo_compose() {
  ffmpeg -loglevel error -y -i "$1" -loop 1 -t 5 -i "$2" -filter_complex \
    "[0:v]fps=12,scale=1200:720:flags=lanczos,setsar=1[a];[1:v]fps=12,scale=1200:720:flags=lanczos,setsar=1[b];[a][b]concat=n=2:v=1,split[x][y];[x]palettegen=max_colors=128[p];[y][p]paletteuse=dither=bayer:bayer_scale=4" \
    "$3"
}
