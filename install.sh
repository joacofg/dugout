#!/bin/sh
# The one-line installer:
#
#   curl -fsSL https://raw.githubusercontent.com/joacofg/dugout/main/install.sh | sh
#
# downloads the release binary for this machine, checks it against the
# release's SHA256SUMS, puts it in ~/.local/bin and runs dugout setup.
# Arguments go to dugout instead of setup, so an invite is
#
#   curl -fsSL https://raw.githubusercontent.com/joacofg/dugout/main/install.sh | sh -s -- join 'https://server#dgi_...'
#
# DUGOUT_VERSION picks a release (default: the latest), DUGOUT_BIN_DIR where
# the binary goes and DUGOUT_RELEASES the releases URL.
#
# Everything is in functions and main runs on the last line, so a download cut
# short runs nothing.
set -eu

say() { printf '%s\n' "$*" >&2; }

fail() {
  say "dugout install: $*"
  exit 1
}

need() {
  command -v "$1" >/dev/null 2>&1 || fail "$1 is needed and not on the PATH"
}

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    fail "sha256sum or shasum is needed to check the download"
  fi
}

main() {
  releases=${DUGOUT_RELEASES:-https://github.com/joacofg/dugout/releases}
  releases=${releases%/}
  bindir=${DUGOUT_BIN_DIR:-${HOME:?HOME is not set}/.local/bin}
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  case $os in
  darwin | linux) ;;
  *) fail "Dugout runs on macOS and Linux, not $(uname -s)" ;;
  esac
  case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "there is no Dugout build for $(uname -m)" ;;
  esac
  need curl
  need tar
  version=${DUGOUT_VERSION:-}
  if [ -z "$version" ]; then
    # /releases/latest redirects to /releases/tag/vX.Y.Z.
    url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$releases/latest") || fail "could not reach $releases"
    version=${url##*/tag/}
    case $version in
    v[0-9]*) ;;
    *) fail "could not tell the latest release from $url" ;;
    esac
  fi
  version=${version#v}
  name=dugout_${version}_${os}_${arch}
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  say "Downloading dugout $version for $os/$arch"
  curl -fsSL -o "$tmp/$name.tar.gz" "$releases/download/v$version/$name.tar.gz" || fail "could not download $name.tar.gz from $releases"
  curl -fsSL -o "$tmp/SHA256SUMS" "$releases/download/v$version/SHA256SUMS" || fail "could not download SHA256SUMS from $releases"
  want=$(awk -v f="$name.tar.gz" '$2 == f || $2 == "./" f || $2 == "*" f {print $1}' "$tmp/SHA256SUMS")
  [ -n "$want" ] || fail "SHA256SUMS has no line for $name.tar.gz"
  got=$(sha256 "$tmp/$name.tar.gz")
  [ "$got" = "$want" ] || fail "the checksum of $name.tar.gz does not match SHA256SUMS: nothing was installed"
  tar -xzf "$tmp/$name.tar.gz" -C "$tmp"
  [ -f "$tmp/$name/dugout" ] || fail "$name.tar.gz has no dugout binary"
  mkdir -p "$bindir"
  # A rename, so a running daemon keeps its binary until it restarts.
  cp "$tmp/$name/dugout" "$bindir/.dugout.new"
  chmod 0755 "$bindir/.dugout.new"
  mv -f "$bindir/.dugout.new" "$bindir/dugout"
  rm -rf "$tmp"
  trap - EXIT
  say "Installed $bindir/dugout"
  case ":${PATH:-}:" in
  *":$bindir:"*) ;;
  *) say "Add $bindir to your PATH to run dugout from a shell; the plugin finds it without." ;;
  esac
  if [ $# -eq 0 ]; then
    set -- setup
  fi
  # curl | sh leaves the script on stdin: give dugout the terminal, when there
  # is one, so init can ask; an agent's shell has none, and setup asks nothing.
  if [ -t 1 ] && (: </dev/tty) 2>/dev/null; then
    exec "$bindir/dugout" "$@" </dev/tty
  fi
  exec "$bindir/dugout" "$@" </dev/null
}

main "$@"
