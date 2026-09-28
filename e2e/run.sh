#!/usr/bin/env sh
# Builds node-go, starts a fresh node on a temporary data directory and walks
# the brand admin + retailer portal in a headless browser.
#   e2e/run.sh            (needs Go, Node.js and zip; installs Playwright on first run)
set -eu
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
PORT="${PORT:-18080}"
TMP="$HERE/tmp"
rm -rf "$TMP" "$HERE/screenshots"
mkdir -p "$TMP/data" "$HERE/screenshots"
(cd "$ROOT" && go build -o "$TMP/node-go" .)
(cd "$ROOT/testdata/spec/kestrel" && zip -qr "$TMP/images.zip" images)
# A "nightly export" for the scheduled import: the same catalog with one renamed article.
mkdir -p "$TMP/export"
cp -R "$ROOT/testdata/spec/kestrel/." "$TMP/export/"
sed 's/Merino Crew Sweater/Merino Crew Jumper/' "$ROOT/testdata/spec/kestrel/products.csv" >"$TMP/export/products.csv"
(cd "$TMP/export" && zip -qr "$TMP/package.zip" products.csv assets.csv images)
[ -d "$HERE/node_modules" ] || (cd "$HERE" && npm ci && npx playwright install chromium)

URL="http://localhost:$PORT"
OBX_DATA_DIR="$TMP/data" PORT="$PORT" OBX_PUBLIC_URL="$URL" \
  OBX_SETUP_CODE=SETUP-1234 OBX_ALLOW_PRIVATE_URLS=true OBX_MAIL_LOG_BODIES=true \
  "$TMP/node-go" serve >"$TMP/node.log" 2>&1 &
NODE_PID=$!
trap 'kill "$NODE_PID" 2>/dev/null || true' EXIT INT TERM
i=0
until curl -sf "http://localhost:$PORT/healthz" >/dev/null; do
  i=$((i + 1)); [ "$i" -lt 30 ] || { cat "$TMP/node.log"; exit 1; }
  sleep 1
done
cd "$HERE"
OBX_E2E_URL="http://localhost:$PORT" OBX_E2E_NODE_LOG="$TMP/node.log" OBX_E2E_IMAGES_ZIP="$TMP/images.zip" OBX_E2E_PACKAGE_ZIP="$TMP/package.zip" \
  npx playwright test "$@"
