#!/usr/bin/env bash
# Tests docker-compose.yml (node + Caddy) with only DOMAIN and ADMIN_EMAIL
# set, as a brand would start it. The domain is "localhost", for which Caddy
# issues a certificate from its own local authority instead of Let's Encrypt
# (a real domain additionally needs DNS pointing at the server; the Caddy
# configuration is the same). Checks HTTPS, the security headers, the full
# smoke test through Caddy, and that a restart keeps the data.
#
#   scripts/test-compose-https.sh     (needs Docker with compose, curl and jq)
#
# Uses the compose project obx-compose-test and removes only that project.
set -euo pipefail
HERE="$(cd "$(dirname "$0")/.." && pwd)"
PROJECT=obx-compose-test
export DOMAIN=localhost ADMIN_EMAIL=ops@kestrel.example HTTPS_PORT="${HTTPS_PORT:-18443}" HTTP_PORT="${HTTP_PORT:-18080}"
export OBX_SETUP_CODE=COMP-OSET-ESTC
dc() { docker compose -p "$PROJECT" -f "$HERE/docker-compose.yml" "$@"; }
trap 'dc down -v >/dev/null 2>&1 || true' EXIT
# Start from zero; only DOMAIN and ADMIN_EMAIL (+ the test's own setup code and ports).
dc up -d --build --wait --wait-timeout 180 >/dev/null
BASE="https://localhost:${HTTPS_PORT}"
export CURL_OPTS="-k"
for _ in $(seq 1 60); do curl -skf "$BASE/readyz" >/dev/null && break; sleep 1; done
issuer="$(curl -skv "$BASE/readyz" 2>&1 | grep -i 'issuer:' | head -1)"
echo "  certificate ${issuer# *}"
echo "$issuer" | grep -qi "caddy" || { echo "not served by Caddy's HTTPS: $issuer"; dc logs caddy | tail -20; exit 1; }
h="$(curl -skI "$BASE/")"
for want in "strict-transport-security: max-age=" "content-security-policy: default-src 'self'" "x-frame-options: DENY" "x-request-id:"; do
  echo "$h" | tr -d '\r' | grep -qi "^$want" || { echo "missing header $want"; echo "$h"; exit 1; }
done
echo "  HTTPS with HSTS, CSP, frame protection and request ids"
"$HERE/scripts/smoke.sh" setup "$BASE" "$OBX_SETUP_CODE"
dc restart node >/dev/null
dc up -d --wait --wait-timeout 120 >/dev/null
"$HERE/scripts/smoke.sh" verify "$BASE"
echo "  data kept across a restart"
code="$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:${HTTP_PORT}/")"
[ "$code" = 308 ] || [ "$code" = 301 ] || { echo "http is not redirected to https ($code)"; exit 1; }
echo "  http redirects to https"
echo "Compose + Caddy test passed."
