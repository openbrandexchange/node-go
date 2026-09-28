#!/usr/bin/env bash
# Tests the Docker image the way it is run in production:
#   1. a plain container: setup, import, portal, image download;
#   2. Litestream: the database streams to a replica; a brand-new container
#      with an empty disk restores it on start and serves the same portal
#      (the Cloud Run set-up: database replica + images on a bucket mount);
#   3. node-go backup / restore: a backup taken from the running node is
#      restored into a new, empty data volume and serves the same portal.
#
#   scripts/test-docker.sh            (needs Docker, curl and jq)
#
# Every container and volume it creates is named obx-dtest-*, and only those
# are removed afterwards.
set -euo pipefail
HERE="$(cd "$(dirname "$0")/.." && pwd)"
IMAGE="${IMAGE:-obx-node-go:test}"
P="obx-dtest-$$"
PORT="${PORT:-18480}"
KEY="docker-test-secret-key-0123456789abcdefghij"
CODE="DTST-SETU-PCOD"
cleanup() {
  for c in "$P-plain" "$P-a" "$P-b" "$P-c"; do docker rm -f "$c" >/dev/null 2>&1 || true; done
  for v in "$P-replica" "$P-blobs" "$P-data-a" "$P-data-b" "$P-data-c"; do docker volume rm "$v" >/dev/null 2>&1 || true; done
}
trap cleanup EXIT
[ -n "${SKIP_BUILD:-}" ] || docker build -q -t "$IMAGE" --build-arg VERSION=test "$HERE" >/dev/null
echo "== image $IMAGE: $(docker run --rm "$IMAGE" version)"

# Named volumes start root-owned; the image runs as uid 10001.
vol() { docker volume create "$1" >/dev/null; docker run --rm -v "$1:/v" --user 0 --entrypoint chown "$IMAGE" 10001 /v; }
wait_ready() { for _ in $(seq 1 90); do curl -fs "http://127.0.0.1:$PORT/readyz" >/dev/null 2>&1 && return 0; sleep 1; done; docker logs "$1" | tail -30; return 1; }
run() { # name data-volume extra-args...
  local name="$1" data="$2"; shift 2
  docker run -d --name "$name" -p "127.0.0.1:$PORT:8080" -v "$data:/data" \
    -e OBX_PUBLIC_URL="http://127.0.0.1:$PORT" -e OBX_SECRET_KEY="$KEY" -e OBX_SETUP_CODE="$CODE" "$@" "$IMAGE" >/dev/null
  wait_ready "$name"
}

echo "== 1. plain container"
vol "$P-data-a"
run "$P-plain" "$P-data-a"
"$HERE/scripts/smoke.sh" setup "http://127.0.0.1:$PORT" "$CODE"
curl -fsS "http://127.0.0.1:$PORT/readyz" | jq -e '.checks.database == "ok"' >/dev/null
docker stop -t 30 "$P-plain" >/dev/null
docker logs "$P-plain" 2>&1 | grep -q '"msg":"stopped"' || { echo "no graceful shutdown in the log"; exit 1; }
echo "  graceful shutdown logged"
docker rm "$P-plain" >/dev/null
docker volume rm "$P-data-a" >/dev/null

echo "== 2. Litestream: replicate, lose the disk, restore on start"
vol "$P-replica"; vol "$P-blobs"; vol "$P-data-a"; vol "$P-data-b"
LS=(-v "$P-replica:/replica" -v "$P-blobs:/blobs" -e LITESTREAM_REPLICA_URL=file:///replica/db -e OBX_BLOB_URL=file:///blobs/images)
run "$P-a" "$P-data-a" "${LS[@]}"
"$HERE/scripts/smoke.sh" setup "http://127.0.0.1:$PORT" "$CODE"
docker stop -t 30 "$P-a" >/dev/null   # Litestream syncs the last changes on SIGTERM
docker rm "$P-a" >/dev/null
docker run --rm -v "$P-replica:/replica" --entrypoint sh "$IMAGE" -c 'test -n "$(ls -A /replica/db)"' || { echo "replica is empty"; exit 1; }
run "$P-b" "$P-data-b" "${LS[@]}"   # empty /data: restored from the replica
docker logs "$P-b" 2>&1 | grep -qi "restor" || echo "  (restore message not in the log; checking the data)"
"$HERE/scripts/smoke.sh" verify "http://127.0.0.1:$PORT"
echo "  restored from the Litestream replica"

echo "== 3. node-go backup from the running node, restore into a new volume"
docker exec "$P-b" node-go backup --to /data/backup-test >/dev/null
docker exec "$P-b" cat /data/backup-test/manifest.json | jq -e '.database == "obx.db"' >/dev/null
docker stop -t 30 "$P-b" >/dev/null
vol "$P-data-c"
# Copy the backup from the old data volume into the new one, then restore.
docker run --rm -v "$P-data-b:/old" -v "$P-data-c:/data" --user 10001 --entrypoint sh "$IMAGE" -c 'cp -r /old/backup-test /data/backup-test'
docker run --rm -v "$P-data-c:/data" -v "$P-blobs:/blobs" -e OBX_SECRET_KEY="$KEY" -e OBX_BLOB_URL=file:///blobs/images "$IMAGE" restore --from /data/backup-test
run "$P-c" "$P-data-c" -v "$P-blobs:/blobs" -e OBX_BLOB_URL=file:///blobs/images
"$HERE/scripts/smoke.sh" verify "http://127.0.0.1:$PORT"
echo "  restored from a node-go backup"
echo "Docker tests passed."
