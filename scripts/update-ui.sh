#!/usr/bin/env sh
# Re-copies the admin-ui build (brand admin + retailer portal) into ui/dist,
# where it is embedded in the node-go binary.
#
#   scripts/update-ui.sh [path-to-admin-ui-checkout]   (default: ../admin-ui)
#
# Set BUILD=1 to run "npm ci && npm run build" in the admin-ui checkout first;
# otherwise its committed dist/ is used as is.
set -eu
HERE="$(cd "$(dirname "$0")/.." && pwd)"
UI="${1:-$HERE/../admin-ui}"
if [ "${BUILD:-0}" = "1" ]; then
  (cd "$UI" && npm ci && npm run build)
fi
if [ ! -f "$UI/dist/index.html" ] || [ ! -f "$UI/dist/locales/en.json" ]; then
  echo "no admin-ui build found at $UI/dist (run with BUILD=1, or npm ci && npm run build there)" >&2
  exit 1
fi
rm -rf "$HERE/ui/dist"
mkdir -p "$HERE/ui/dist"
cp -R "$UI/dist/." "$HERE/ui/dist/"
(cd "$UI" && git rev-parse HEAD 2>/dev/null || echo unknown) > "$HERE/ui/dist/ADMIN_UI_COMMIT"
echo "vendored admin-ui $(cat "$HERE/ui/dist/ADMIN_UI_COMMIT") into ui/dist"
