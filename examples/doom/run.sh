#!/bin/sh
# Downloads the latest musql release for this machine and starts Doom.
#   curl -fsSL https://raw.githubusercontent.com/samyfodil/musql/main/examples/doom/run.sh | sh
set -eu
case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "unsupported OS $(uname -s); on Windows use run.ps1" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "unsupported CPU $(uname -m)" >&2; exit 1 ;;
esac
name="musql-$os-$arch"
dir="${XDG_CACHE_HOME:-$HOME/.cache}/musql-release"
mkdir -p "$dir"
echo "downloading $name ..."
curl -fsSL "https://github.com/samyfodil/musql/releases/latest/download/$name.tar.gz" | tar xz -C "$dir"
bin="$dir/$name/musql-doom"
# The binary is not signed; let macOS run it.
[ "$os" = darwin ] && xattr -d com.apple.quarantine "$bin" 2>/dev/null || true
exec "$bin" "$@"
