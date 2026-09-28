#!/bin/sh
# Starts node-go. With LITESTREAM_REPLICA_URL set (for example
# gcs://my-bucket/obx-db or s3://my-bucket/obx-db) the SQLite database is
# restored from the replica on start and continuously replicated to it.
set -eu

if [ "${1:-serve}" = "serve" ] && [ -n "${LITESTREAM_REPLICA_URL:-}" ]; then
  db="${OBX_DATA_DIR:-/data}/obx.db"
  mkdir -p "$(dirname "$db")"
  litestream restore -if-db-not-exists -if-replica-exists -o "$db" "$LITESTREAM_REPLICA_URL"
  exec litestream replicate -exec "node-go serve" "$db" "$LITESTREAM_REPLICA_URL"
fi

exec node-go "$@"
