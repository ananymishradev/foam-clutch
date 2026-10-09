#!/usr/bin/env bash
# uninstall.sh - remove the `clutch` CLI binary.
# Usage:
#   ./uninstall.sh [--prefix ~/.local] [--bindir <dir>] [--system] [--all] [-h|--help]
#
# Default removes $PREFIX/bin/clutch (i.e. ~/.local/bin/clutch).
#   --system == --bindir /usr/local/bin
#   --all    == also scan GOPATH/bin, ~/.local/bin, /usr/local/bin.
# Never touches databases, .foam-cache/, or .foam-runs/ (user data).
set -euo pipefail

PREFIX="${PREFIX:-$HOME/.local}"
BINDIR=""
SYSTEM=0
ALL=0

usage() {
  sed -n '2,10p' "$0"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --prefix) PREFIX="${2:?--prefix needs a value}"; shift 2 ;;
    --bindir) BINDIR="${2:?--bindir needs a value}"; shift 2 ;;
    --system) SYSTEM=1; shift ;;
    --all) ALL=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown flag: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [ "$SYSTEM" -eq 1 ]; then
  BINDIR="/usr/local/bin"
elif [ -z "$BINDIR" ]; then
  BINDIR="$PREFIX/bin"
fi

targets=("$BINDIR/clutch")
if [ "$ALL" -eq 1 ]; then
  GOPATH_BIN="$(go env GOPATH 2>/dev/null || true)/bin/clutch"
  for c in "$HOME/.local/bin/clutch" "$GOPATH_BIN" "/usr/local/bin/clutch"; do
    skip=0
    for t in "${targets[@]}"; do [ "$t" = "$c" ] && skip=1; done
    [ "$skip" -eq 0 ] && targets+=("$c")
  done
fi

removed=0
for target in "${targets[@]}"; do
  [ -z "$target" ] || [ "$target" = "/bin/clutch" ] && continue
  if [ -e "$target" ] || [ -L "$target" ]; then
    if [ -w "$(dirname "$target")" ]; then
      rm -f "$target"
    else
      echo "need write access to $(dirname "$target"), retrying with sudo"
      sudo rm -f "$target"
    fi
    echo "removed $target"
    removed=1
  else
    echo "not found: $target"
  fi
done

if command -v clutch >/dev/null 2>&1; then
  echo "warn: clutch still on PATH at $(command -v clutch) (another copy exists, use --all to scan all locations)" >&2
fi
[ "$removed" -eq 1 ] || { echo "nothing removed" >&2; exit 1; }
echo "uninstall done"
