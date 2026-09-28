#!/usr/bin/env sh
# Copies the JSON Schemas and conformance fixtures from a local checkout of the
# OBX spec repository (CC-BY-4.0) into this repository.
#
#   scripts/sync-spec.sh [path-to-spec-checkout]   (default: ../spec)
set -eu
SPEC="${1:-../spec}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
if [ ! -d "$SPEC/schemas" ]; then
  echo "spec checkout not found at $SPEC" >&2
  exit 1
fi
rm -rf "$HERE/internal/schemas/json" "$HERE/testdata/spec"
mkdir -p "$HERE/internal/schemas/json" "$HERE/testdata/spec"
cp "$SPEC"/schemas/*.json "$HERE/internal/schemas/json/"
cp -R "$SPEC/fixtures/kestrel" "$SPEC/fixtures/expected" "$SPEC/fixtures/ingest" "$HERE/testdata/spec/"
mkdir -p "$HERE/testdata/spec/examples"
cp "$SPEC"/fixtures/examples/*.json "$HERE/testdata/spec/examples/"
(cd "$SPEC" && git rev-parse HEAD 2>/dev/null || echo unknown) > "$HERE/testdata/spec/SPEC_COMMIT"
echo "synced schemas and fixtures from $SPEC"
