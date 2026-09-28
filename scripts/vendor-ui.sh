#!/usr/bin/env sh
# Copies the built admin-ui (brand admin + retailer portal) into ui/dist so
# it is embedded in the node-go binary.
#
#   scripts/vendor-ui.sh [path-to-admin-ui-checkout]   (default: ../admin-ui)
#
# The admin-ui checkout must be built first (npm ci && npm run build).
set -eu
UI="${1:-../admin-ui}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
if [ ! -f "$UI/dist/index.html" ]; then
  echo "no build found at $UI/dist (run npm ci && npm run build in admin-ui)" >&2
  exit 1
fi
rm -rf "$HERE/ui/dist"
mkdir -p "$HERE/ui/dist"
cp -R "$UI/dist/." "$HERE/ui/dist/"
(cd "$UI" && git rev-parse HEAD 2>/dev/null || echo unknown) > "$HERE/ui/dist/ADMIN_UI_COMMIT"
echo "vendored admin-ui from $UI into ui/dist"
