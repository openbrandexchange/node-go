#!/usr/bin/env bash
# Checks deploy/cloudrun.sh against the real Google Cloud CLI without a
# Google Cloud account: the script runs with a stand-in gcloud that records
# every call, and each recorded call is then checked against the help text of
# the real gcloud (from the official Docker image): the command must exist and
# every flag must be one of its flags. Runs the script twice, for a first
# deploy (nothing exists yet) and for an update with every option set.
#
#   scripts/test-cloudrun.sh
#
# Needs Docker. GCLOUD_IMAGE overrides the CLI image.
set -euo pipefail
HERE="$(cd "$(dirname "$0")/.." && pwd)"
IMAGE="${GCLOUD_IMAGE:-gcr.io/google.com/cloudsdktool/google-cloud-cli:alpine}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

cat >"$WORK/gcloud" <<'SHIM'
#!/usr/bin/env bash
# Stand-in gcloud: records the call, answers what deploy/cloudrun.sh reads.
printf '%s\0' "$@" >"$(mktemp "$SHIM_DIR/call.XXXXXX")"
args=" $* "
case "$args" in
  *" config get-value project "*) echo demo-project ;;
  *" projects describe "*) echo 123456789012 ;;
  *" secrets versions access "*) echo "ABCD-EFGH-JKMN" ;;
  *" describe "*) [ "${SHIM_EXISTS:-0}" = 1 ] || exit 1 ;;
  *" secrets create "*|*" secrets versions add "*) cat >/dev/null ;;
esac
exit 0
SHIM
chmod +x "$WORK/gcloud"

run() { # name, env...
  local name="$1"; shift
  mkdir -p "$WORK/$name"
  env "$@" SHIM_DIR="$WORK/$name" GCLOUD="$WORK/gcloud" "$HERE/deploy/cloudrun.sh" >"$WORK/$name.out"
  cat "$WORK/$name"/call.* | tr '\0' ' ' >"$WORK/$name.log"
  grep -q "Your brand portal runs at: https://brand-portal-123456789012.europe-west4.run.app" "$WORK/$name.out" \
    || { echo "FAIL $name: unexpected output"; cat "$WORK/$name.out"; exit 1; }
}
# DRY_RUN=1 needs no gcloud at all, calls nothing, prints the deploy command
# and never prints a secret value.
dry="$(DRY_RUN=1 GCLOUD="$WORK/no-gcloud" PROJECT=demo-project SMTP_URL='smtp://u:very-secret@smtp.example:587' "$HERE/deploy/cloudrun.sh" 2>&1)" \
  || { echo "FAIL dry run: exit $?"; echo "$dry"; exit 1; }
echo "$dry" | grep -q "gcloud run deploy brand-portal --project demo-project" || { echo "FAIL dry run: no deploy command"; echo "$dry"; exit 1; }
echo "$dry" | grep -q "nothing was created or changed" || { echo "FAIL dry run: no closing line"; exit 1; }
if echo "$dry" | grep -q "very-secret"; then echo "FAIL dry run: printed a secret"; exit 1; fi

run first
run update SHIM_EXISTS=1 DOMAIN=images.kestrel.example DATABASE_URL='postgres://p:x@/db?host=/cloudsql/demo-project:europe-west4:db' \
  CLOUDSQL_INSTANCE=demo-project:europe-west4:db SMTP_URL='smtp://u:p@smtp.example:587' MAIL_FROM=portal@kestrel.example ALWAYS_ON=0 MEMORY=4Gi

# The first deploy must go out with the final public address and probes.
grep -q "OBX_PUBLIC_URL=https://brand-portal-123456789012.europe-west4.run.app" "$WORK/first.log" || { echo "FAIL: public URL not set on the first deploy"; exit 1; }
grep -q "startup-probe httpGet.path=/readyz" "$WORK/first.log" || { echo "FAIL: no readiness startup probe"; exit 1; }
grep -q "OBX_PUBLIC_URL=https://images.kestrel.example" "$WORK/update.log" || { echo "FAIL: DOMAIN not used"; exit 1; }
grep -q "GOMEMLIMIT=2048MiB" "$WORK/update.log" || { echo "FAIL: GOMEMLIMIT for 4Gi"; exit 1; }
if grep -q "LITESTREAM_REPLICA_URL" "$WORK/update.log"; then echo "FAIL: Litestream with PostgreSQL"; exit 1; fi

calls="$(find "$WORK/first" "$WORK/update" -name 'call.*' | wc -l | tr -d ' ')"
echo "Checking ${calls} gcloud calls against $(docker run --rm "$IMAGE" gcloud --version | head -1)"
mkdir -p "$WORK/help"
fail=0
for call in "$WORK"/first/call.* "$WORK"/update/call.*; do
  words=""
  flags=""
  inflags=0
  while IFS= read -r -d '' x; do
    case "$x" in --*) inflags=1; flags="${flags} ${x%%=*}" ;; esac
    [ "$inflags" = 1 ] || words="${words} ${x}"
  done <"$call"
  # The longest prefix of the words that is a gcloud command.
  cmd="$words"
  help=""
  while [ -n "$(echo "$cmd" | tr -d ' ')" ]; do
    key="$(echo "$cmd" | tr ' /:' '___')"
    if [ ! -f "$WORK/help/$key" ]; then
      # shellcheck disable=SC2086
      docker run --rm "$IMAGE" gcloud $cmd --help >"$WORK/help/$key" 2>/dev/null || true
    fi
    if grep -q "SYNOPSIS" "$WORK/help/$key"; then
      help="$WORK/help/$key"
      break
    fi
    cmd="${cmd% *}"
  done
  if [ -z "$help" ]; then echo "FAIL: not a gcloud command:${words}"; fail=1; continue; fi
  for flag in $flags; do
    base="${flag#--}"
    base="${base#no-}"
    if ! grep -qE -- "(${flag}|--\[no-\]${base})([^a-z-]|\$)" "$help"; then
      echo "FAIL: gcloud${cmd} has no flag ${flag}"; fail=1
    fi
  done
done
[ "$fail" = 0 ] || exit 1
echo "OK: deploy/cloudrun.sh uses only existing gcloud commands and flags (first deploy and update)."
