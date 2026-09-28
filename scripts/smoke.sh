#!/usr/bin/env bash
# End-to-end smoke test of a running node over HTTP(S), the way a brand uses
# it: first-run setup, import of the Kestrel example package, then the
# portal and the retailer API. Used by the Docker, Compose and backup tests.
#
#   scripts/smoke.sh setup   BASE_URL SETUP_CODE    set up, import, verify
#   scripts/smoke.sh verify  BASE_URL               log in again and verify the data
#
# CURL_OPTS adds curl options (for example "-k --resolve portal.test:443:127.0.0.1").
# Needs curl and jq.
set -euo pipefail
MODE="${1:?usage: smoke.sh setup|verify BASE_URL [SETUP_CODE]}"
BASE="${2:?base URL}"
CODE="${3:-}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
JAR="$(mktemp)"
trap 'rm -f "$JAR"' EXIT
EMAIL="smoke-admin@kestrel.example"
PASSWORD="smoke test password 2026"
# shellcheck disable=SC2086
c() { curl -sS --fail-with-body --max-time 120 -b "$JAR" -c "$JAR" ${CURL_OPTS:-} "$@"; }
csrf() { c "$BASE/admin/api/session" | jq -r .csrf_token; }
say() { printf '  %s\n' "$*"; }

for _ in $(seq 1 60); do
  c -o /dev/null "$BASE/readyz" 2>/dev/null && break
  sleep 1
done
c "$BASE/readyz" | jq -e '.status == "ready"' >/dev/null
say "ready: $BASE"

login() {
  local t
  t="$(csrf)"
  c -H "X-CSRF-Token: $t" -H 'Content-Type: application/json' \
    -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}" "$BASE/admin/api/session/login" | jq -r .csrf_token
}

if [ "$MODE" = setup ]; then
  [ -n "$CODE" ] || { echo "setup needs the setup code" >&2; exit 2; }
  t="$(csrf)"
  T="$(c -H "X-CSRF-Token: $t" -H 'Content-Type: application/json' -d "{
    \"setup_code\": \"$CODE\",
    \"company\": {\"name\": \"Kestrel Outfitters\", \"first_brand_name\": \"Kestrel\", \"default_language\": \"nl\", \"languages\": [\"nl\", \"en\"]},
    \"admin\": {\"name\": \"Smoke Admin\", \"email\": \"$EMAIL\", \"password\": \"$PASSWORD\"}}" "$BASE/admin/api/setup" | jq -r .csrf_token)"
  say "setup done"
  run="$(c -H "X-CSRF-Token: $T" -F "package=@$HERE/testdata/kestrel-example.zip" "$BASE/admin/api/imports" | jq -r .id)"
  for _ in $(seq 1 120); do
    st="$(c "$BASE/admin/api/imports/$run" | jq -r .status)"
    [ "$st" = succeeded ] && break
    [ "$st" = failed ] && { c "$BASE/admin/api/imports/$run/errors"; exit 1; }
    sleep 1
  done
  [ "$st" = succeeded ] || { echo "import did not finish" >&2; exit 1; }
  say "import of the Kestrel example succeeded"
else
  T="$(login)"
  say "logged in again"
fi

products="$(c "$BASE/admin/api/portal/products?limit=200" | jq '.items | length')"
[ "$products" -ge 8 ] || { echo "expected the Kestrel products, got $products" >&2; exit 1; }
say "portal lists $products products"
pid="$(c "$BASE/admin/api/portal/products?q=KO-1001" | jq -r '.items[0].id')"
# Images are made in the background; wait until the product's images are there.
for _ in $(seq 1 60); do
  n="$(c "$BASE/admin/api/portal/products/$pid" | jq '[.assets[]?] | length')"
  [ "$n" -ge 1 ] && break
  sleep 1
done
aid="$(c "$BASE/admin/api/portal/products/$pid" | jq -r '.assets[0].id')"
bytes="$(c -o /dev/null -w '%{size_download}' "$BASE/admin/api/portal/assets/$aid/download?variant=original")"
[ "$bytes" -gt 100 ] || { echo "image download returned $bytes bytes" >&2; exit 1; }
say "image download works ($bytes bytes)"
ean="$(c -H "X-CSRF-Token: $T" -H 'Content-Type: application/json' -d '{"gtins":["2000100101001","2000100101018"]}' \
  "$BASE/admin/api/portal/gtin-lookup" | jq '[.results[] | select(.status == "matched")] | length')"
[ "$ean" -ge 1 ] || { echo "EAN lookup matched nothing" >&2; exit 1; }
say "EAN lookup matches"
c "$BASE/.well-known/obx" | jq -e '.node.software == "obx-node-go"' >/dev/null
say "discovery document served"
echo "smoke test passed ($MODE)"
