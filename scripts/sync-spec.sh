#!/usr/bin/env sh
# Copies the JSON Schemas, the vocabularies (term names, embedded in the
# binary) and the conformance fixtures from a local checkout of the OBX spec
# repository (CC-BY-4.0) into this repository.
#
#   scripts/sync-spec.sh [path-to-spec-checkout]   (default: ../spec)
set -eu
SPEC="${1:-../spec}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
if [ ! -d "$SPEC/schemas" ]; then
  echo "spec checkout not found at $SPEC" >&2
  exit 1
fi
rm -rf "$HERE/internal/schemas/json" "$HERE/internal/vocab/json" "$HERE/testdata/spec"
mkdir -p "$HERE/internal/schemas/json" "$HERE/internal/vocab/json" "$HERE/testdata/spec"
cp "$SPEC"/schemas/*.json "$HERE/internal/schemas/json/"
cp "$SPEC"/vocabularies/*.json "$HERE/internal/vocab/json/"
cp -R "$SPEC/fixtures/kestrel" "$SPEC/fixtures/expected" "$SPEC/fixtures/ingest" "$SPEC/fixtures/imports" "$HERE/testdata/spec/"
mkdir -p "$HERE/testdata/spec/examples"
cp "$SPEC"/fixtures/examples/*.json "$HERE/testdata/spec/examples/"
(cd "$SPEC" && git rev-parse HEAD 2>/dev/null || echo unknown) > "$HERE/testdata/spec/SPEC_COMMIT"
echo "synced schemas, vocabularies and fixtures from $SPEC"
