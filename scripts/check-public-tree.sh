#!/usr/bin/env bash
# Keeps this repo to what users and contributors need: code, user docs and
# contributor docs. Two checks over the tracked files:
#
# 1. Every path matches the allowlist below. A new kind of file fails until
#    someone adds it here on purpose, in the same PR, with a reason.
# 2. No text file holds a term from scripts/public-denylist.sha256. That file
#    lists only SHA-256 hashes of lowercased terms, so it does not reveal
#    what it blocks. Each word, each word split on . _ and -, and each pair
#    of adjacent words is hashed and compared; a match prints the file and
#    line, never the term.
#
# Usage: scripts/check-public-tree.sh [dir]   (default: this repo; dir must
# be a git checkout, such as a snapshot)
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
DIR=$(cd "${1:-$ROOT}" && pwd)
DENYLIST="$ROOT/scripts/public-denylist.sha256"

# bash patterns, matched against the whole path; * also matches /.
ALLOW=(
  README.md LICENSE AGENTS.md CLAUDE.md CONTRIBUTING.md SECURITY.md
  CODE_OF_CONDUCT.md CHANGELOG.md go.mod go.sum .gitignore .dockerignore
  .golangci.yml install.sh
  'cmd/dugout/*.go'
  'internal/*.go' 'internal/*/testdata/*.json'
  'internal/serve/web/*.html' 'internal/serve/web/*.css' 'internal/serve/web/*.js'
  'internal/serve/web/fonts/*.woff2' internal/serve/web/fonts/OFL.txt
  plugin/.claude-plugin/plugin.json plugin/.mcp.json plugin/bin/dugout-run
  'plugin/commands/*.md' plugin/hooks/hooks.json
  .claude-plugin/marketplace.json
  deploy/.env.example deploy/Caddyfile deploy/Dockerfile deploy/docker-compose.yml
  '.github/workflows/*.yml' '.github/ISSUE_TEMPLATE/*.yml'
  .github/PULL_REQUEST_TEMPLATE.md .github/dependabot.yml
  scripts/check-version.sh scripts/check-public-tree.sh scripts/public-denylist.sha256
  scripts/smoke-claude.sh scripts/smoke-mcp.sh scripts/solo.sh
  docs/install.md docs/self-hosting.md docs/configuration.md docs/demo.tape docs/demo.sh docs/demo.gif
)

files=()
while IFS= read -r -d '' f; do files+=("$f"); done < <(git -C "$DIR" ls-files -z)

failed=0
for f in "${files[@]}"; do
  ok=0
  for pat in "${ALLOW[@]}"; do
    # shellcheck disable=SC2053 # $pat is a pattern on purpose
    if [[ $f == $pat ]]; then ok=1; break; fi
  done
  if [[ $ok == 0 ]]; then
    echo "public-tree: $f is not on the allowlist in scripts/check-public-tree.sh"
    failed=1
  fi
done
if [[ $failed == 1 ]]; then
  cat <<'EOF'
public-tree: this repo holds only code, user docs and contributor docs.
  Design notes, research, plans, reviews, session handoffs, agent notes and
  design-tool artifacts belong outside it, in the maintainer's own notes. If
  users or contributors need the file, add its path to ALLOW in the same PR.
EOF
fi

(cd "$DIR" && printf '%s\0' "${files[@]}" | python3 -I -c '
import hashlib, re, sys

deny = set()
for line in open(sys.argv[1], encoding="utf-8"):
    line = line.split("#", 1)[0].strip()
    if line:
        deny.add(line.lower())

def h(s):
    return hashlib.sha256(s.encode()).hexdigest()

word = re.compile(r"[a-z0-9._-]+")
plain = re.compile(r"\b(?:CEO|ENG)-[0-9]+\b")
bad = 0
for path in sys.stdin.buffer.read().split(b"\0"):
    if not path:
        continue
    path = path.decode()
    data = open(path, "rb").read()
    if b"\0" in data:
        continue
    text = data.decode("utf-8", "replace")
    for n, line in enumerate([path] + text.split("\n")):
        where = path if n == 0 else f"{path}:{n}"
        words = [w.strip("._-") for w in word.findall(line.lower())]
        words = [w for w in words if w]
        cands = set(words)
        for w in words:
            cands.update(p for p in re.split(r"[._-]+", w) if p)
        for a, b in zip(words, words[1:]):
            cands.add(a + " " + b)
        for c in cands:
            if h(c) in deny:
                print(f"public-tree: {where}: a blocked term (sha256 {h(c)[:12]})")
                bad = 1
        if plain.search(line):
            print(f"public-tree: {where}: a design-log id")
            bad = 1
if bad:
    print("public-tree: remove these: they name internal tools, people or places,"
          " or point at internal documents.")
sys.exit(bad)
' "$DENYLIST") || failed=1

if [[ $failed == 0 ]]; then
  echo "public-tree: ${#files[@]} files, all allowed and clean"
fi
exit "$failed"
