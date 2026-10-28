#!/usr/bin/env bash
# The binary, the plugin and (on a release) the tag must say the same version:
# Claude Code caches the plugin by its version, and a binary that says another
# one would leave people on an old plugin. Usage: scripts/check-version.sh [X.Y.Z]
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)

bin=$(sed -n 's/^var version = "\(.*\)"$/\1/p' "$root/cmd/dugout/install.go")
plugin=$(jq -r .version "$root/plugin/.claude-plugin/plugin.json")

[ -n "$bin" ] || { echo "no 'var version' in cmd/dugout/install.go"; exit 1; }
[ "$bin" = "$plugin" ] || { echo "cmd/dugout/install.go is $bin but plugin/.claude-plugin/plugin.json is $plugin"; exit 1; }
if [ $# -ge 1 ] && [ "$1" != "$bin" ]; then
  echo "the tag is $1 but the version in the code is $bin"
  exit 1
fi
echo "version $bin"
