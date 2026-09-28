#!/usr/bin/env sh
# Fails when a Go dependency compiled into node-go has a licence that is not
# compatible with distributing node-go under the AGPL-3.0 (or is unknown).
# With --save DIR it also copies every dependency's licence and notice files
# into DIR (release archives include them).
#
#   scripts/check-licenses.sh [--save DIR]
#
# Uses go-licenses (installed on first run with the Go toolchain of go.mod).
set -eu
HERE="$(cd "$(dirname "$0")/.." && pwd)"
cd "$HERE"
VERSION=v2.0.1
BIN="$(go env GOPATH)/bin/go-licenses"
if [ ! -x "$BIN" ] || [ "$("$BIN" --version 2>/dev/null | grep -c "$VERSION" || true)" = 0 ]; then
  # Build it with this module's toolchain so it can read the standard library.
  GOTOOLCHAIN="$(go env GOVERSION)" go install "github.com/google/go-licenses/v2@$VERSION"
fi
ALLOWED=Apache-2.0,BSD-2-Clause,BSD-3-Clause,MIT,ISC,MPL-2.0,Unlicense,0BSD
"$BIN" check ./... --ignore github.com/openbrandexchange/node-go --allowed_licenses="$ALLOWED" 2>/dev/null || {
  "$BIN" report ./... --ignore github.com/openbrandexchange/node-go 2>/dev/null
  echo "A dependency has a licence outside: $ALLOWED" >&2
  exit 1
}
echo "Dependency licences OK ($("$BIN" report ./... --ignore github.com/openbrandexchange/node-go 2>/dev/null | wc -l | tr -d ' ') modules, all in: $ALLOWED)"
if [ "${1:-}" = "--save" ]; then
  rm -rf "${2:?--save needs a directory}"
  "$BIN" save ./... --ignore github.com/openbrandexchange/node-go --save_path="$2" 2>/dev/null
  echo "Licence texts saved to $2"
fi
