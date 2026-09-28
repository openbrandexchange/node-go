#!/usr/bin/env bash
# One-command Fly.io deploy: ./deploy/fly.sh <app-name> [region]
set -euo pipefail
command -v fly >/dev/null || { echo "Install flyctl: https://fly.io/docs/flyctl/install/" >&2; exit 1; }
APP="${1:?usage: deploy/fly.sh <app-name> [region]}"
REGION="${2:-ams}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
cd "$HERE"
fly apps create "$APP" 2>/dev/null || true
fly volumes list -a "$APP" | grep -q obx_data || fly volumes create obx_data -a "$APP" --region "$REGION" --size 10 --yes
# The key encrypts stored credentials: create it once, never replace it.
if ! fly secrets list -a "$APP" | grep -q OBX_SECRET_KEY; then
  fly secrets set -a "$APP" --stage OBX_SECRET_KEY="$(head -c 48 /dev/urandom | base64 | tr -d '\n')" >/dev/null
fi
fly deploy -a "$APP" --primary-region "$REGION" --ha=false
echo "Setup code:"; fly logs -a "$APP" --no-tail | grep -o 'setup code: [A-Z0-9-]*' | tail -1 || true
