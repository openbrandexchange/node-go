#!/usr/bin/env bash
# Runs install.sh for real on a disposable Linux machine with systemd (the CI
# runner): dry runs against the published releases (checksums verified), then
# a real install of node-go built from this checkout with built-in HTTPS, an
# update in place, uninstall (data kept), a behind-proxy install and a purge.
# The test domain does not resolve, so no certificate is ever requested; the
# DNS check is passed with --force.
#
#   scripts/test-install.sh        (needs root via sudo, systemd, Go; changes the machine)
set -euo pipefail
HERE="$(cd "$(dirname "$0")/.." && pwd)"
[ "$(uname -s)" = Linux ] && [ -d /run/systemd/system ] || { echo "needs Linux with systemd (a throwaway machine)" >&2; exit 1; }
[ -n "${CI:-}" ] || [ "${I_KNOW_THIS_CHANGES_THE_MACHINE:-}" = 1 ] || { echo "changes this machine; set I_KNOW_THIS_CHANGES_THE_MACHINE=1" >&2; exit 1; }
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
DOMAIN=portal.obx-install-test.invalid
fail() { echo "FAIL: $*" >&2; sudo journalctl -u node-go -n 40 --no-pager >&2 || true; exit 1; }
envval() { sudo sed -n "s/^$1=//p" /etc/node-go/node-go.env; }

echo "== dry runs against the published releases"
sh "$HERE/install.sh" --dry-run --behind-proxy --domain "$DOMAIN" --version 0.3.0 | tee "$WORK/dry.txt"
grep -q 'sha256 verified' "$WORK/dry.txt" || fail "go archive not verified"
sh "$HERE/install.sh" --dry-run --force --edition ts --domain "$DOMAIN" --version 0.3.0 | tee "$WORK/dry-ts.txt"
grep -q 'sha256 verified' "$WORK/dry-ts.txt" || fail "ts archive not verified"
grep -q 'obx-portal.caddy' "$WORK/dry-ts.txt" || fail "no Caddy site in the ts dry run"
if sh "$HERE/install.sh" --dry-run --force --domain "$DOMAIN" --version 0.3.0 >/dev/null 2>&1; then
	fail "0.3.0 (no built-in HTTPS) accepted without --behind-proxy"
fi
if sh "$HERE/install.sh" --dry-run --domain "$DOMAIN" --version 0.3.0 --behind-proxy --bogus >/dev/null 2>&1; then
	fail "unknown option accepted"
fi
test ! -e /etc/node-go || fail "a dry run changed the machine"

echo "== archive from this checkout"
arch=amd64
[ "$(uname -m)" = aarch64 ] && arch=arm64
(cd "$HERE" && CGO_ENABLED=0 go build -trimpath -o "$WORK/pkg/node-go" .)
tar -C "$WORK/pkg" -czf "$WORK/node-go_0.0.0-test_linux_${arch}.tar.gz" node-go
ARCHIVE="$WORK/node-go_0.0.0-test_linux_${arch}.tar.gz"

echo "== fresh install with built-in HTTPS"
sudo sh "$HERE/install.sh" --domain "$DOMAIN" --email it@obx-install-test.invalid --archive "$ARCHIVE" --force | tee "$WORK/install.txt"
systemctl is-active --quiet node-go || fail "service not running"
[ "$(stat -c %U /var/lib/node-go)" = obx ] || fail "data dir not owned by obx"
[ "$(sudo stat -c %a /etc/node-go/node-go.env)" = 600 ] || fail "settings file readable by others"
pgrep -u obx -x node-go >/dev/null || fail "node-go does not run as obx"
curl -fsS http://127.0.0.1/healthz >/dev/null || fail "no /healthz on port 80"
loc="$(curl -sS -o /dev/null -w '%{redirect_url}' "http://127.0.0.1/portal?x=1" -H "Host: $DOMAIN")"
[ "$loc" = "https://$DOMAIN/portal?x=1" ] || fail "http does not redirect to https (got $loc)"
sudo ss -Hltn 'sport = :443' | grep -q . || fail "nothing listens on 443"
code="$(envval OBX_SETUP_CODE)"
secret="$(envval OBX_SECRET_KEY)"
[ -n "$code" ] && [ ${#secret} -ge 32 ] || fail "setup code or secret missing"
grep -q "$code" "$WORK/install.txt" || fail "setup code not printed"
[ "$(envval OBX_TLS)" = auto ] && [ "$(envval OBX_DOMAIN)" = "$DOMAIN" ] || fail "settings"
sudo sh -c 'set -a; . /etc/node-go/node-go.env; exec runuser -u obx -- /usr/local/bin/node-go check-config' || fail "check-config"
systemctl show node-go -p AmbientCapabilities | grep -q cap_net_bind_service || fail "capability"

echo "== update in place (no options: domain and edition from the settings)"
sudo sh "$HERE/install.sh" --archive "$ARCHIVE" --force | tee "$WORK/update.txt"
grep -q 'kept the settings' "$WORK/update.txt" || fail "update did not keep the settings"
[ "$(envval OBX_SECRET_KEY)" = "$secret" ] || fail "secret changed on update"
systemctl is-active --quiet node-go || fail "service not running after update"

echo "== uninstall keeps settings and data"
sudo sh "$HERE/install.sh" --uninstall
test ! -e /usr/local/bin/node-go && test ! -e /etc/systemd/system/node-go.service || fail "program or unit left"
if ! sudo test -f /etc/node-go/node-go.env || ! sudo test -f /var/lib/node-go/obx.db; then fail "data or settings removed"; fi

echo "== reinstall brings it back as it was"
sudo sh "$HERE/install.sh" --archive "$ARCHIVE" --force >/dev/null
[ "$(envval OBX_SECRET_KEY)" = "$secret" ] || fail "secret changed on reinstall"
curl -fsS http://127.0.0.1/healthz >/dev/null || fail "not back"

echo "== purge"
sudo sh "$HERE/install.sh" --uninstall --purge
test ! -e /var/lib/node-go && test ! -e /etc/node-go || fail "purge left data"
! id obx >/dev/null 2>&1 || fail "obx user left"

echo "== behind a proxy"
sudo sh "$HERE/install.sh" --behind-proxy --domain "$DOMAIN" --archive "$ARCHIVE" | tee "$WORK/proxy.txt"
curl -fsS http://127.0.0.1:8080/healthz >/dev/null || fail "no /healthz on 8080"
[ "$(envval OBX_TRUST_PROXY)" = true ] && [ "$(envval OBX_PUBLIC_URL)" = "https://$DOMAIN" ] || fail "proxy settings"
! sudo ss -Hltn 'sport = :443' | grep -q . || fail "443 used behind a proxy"
sudo sh "$HERE/install.sh" --uninstall --purge

echo "install.sh: all checks passed"
