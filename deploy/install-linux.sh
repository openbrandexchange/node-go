#!/usr/bin/env bash
# Install the brand portal as a systemd service on a Linux server:
#
#   sudo ./deploy/install-linux.sh --domain images.yourbrand.com [--binary ./node-go] [--with-caddy]
#
# Without --binary the script builds node-go with Go (1.26+) from this folder.
# --with-caddy installs Caddy (Debian/Ubuntu) for automatic HTTPS.
set -euo pipefail

DOMAIN=""
BINARY=""
WITH_CADDY=0
while [ $# -gt 0 ]; do
  case "$1" in
    --domain) DOMAIN="$2"; shift 2 ;;
    --binary) BINARY="$2"; shift 2 ;;
    --with-caddy) WITH_CADDY=1; shift ;;
    *) echo "unknown option $1" >&2; exit 2 ;;
  esac
done
[ "$(id -u)" = "0" ] || { echo "Run as root (sudo)." >&2; exit 1; }
[ -n "$DOMAIN" ] || { echo "Pass --domain images.yourbrand.com" >&2; exit 1; }
HERE="$(cd "$(dirname "$0")/.." && pwd)"

if [ -z "$BINARY" ]; then
  command -v go >/dev/null || { echo "Go is not installed; pass --binary with a built node-go." >&2; exit 1; }
  (cd "$HERE" && CGO_ENABLED=0 go build -trimpath -o /tmp/node-go .)
  BINARY=/tmp/node-go
fi
install -m 0755 "$BINARY" /usr/local/bin/node-go

id obx >/dev/null 2>&1 || useradd --system --home /var/lib/node-go --shell /usr/sbin/nologin obx
install -d -o obx -g obx -m 0700 /var/lib/node-go
install -d -m 0755 /etc/node-go
if [ ! -f /etc/node-go/node-go.env ]; then
  umask 077
  cat >/etc/node-go/node-go.env <<ENV
OBX_PUBLIC_URL=https://${DOMAIN}
OBX_DATA_DIR=/var/lib/node-go
OBX_SECRET_KEY=$(head -c 48 /dev/urandom | base64 | tr -d '\n')
OBX_TRUST_PROXY=true
PORT=8080
# OBX_SMTP_URL=smtp://user:password@smtp.example.com:587
# OBX_MAIL_FROM=portal@${DOMAIN}
ENV
  chmod 0600 /etc/node-go/node-go.env
fi
install -m 0644 "$HERE/deploy/systemd/node-go.service" /etc/systemd/system/node-go.service
systemctl daemon-reload
systemctl enable --now node-go

if [ "$WITH_CADDY" = "1" ]; then
  if ! command -v caddy >/dev/null; then
    apt-get update -y && apt-get install -y caddy
  fi
  cat >/etc/caddy/Caddyfile <<CADDY
${DOMAIN} {
	encode zstd gzip
	request_body {
		max_size 2GB
	}
	reverse_proxy 127.0.0.1:8080
}
CADDY
  systemctl reload caddy || systemctl restart caddy
fi

sleep 2
echo
echo "The portal runs. Open https://${DOMAIN} and enter the setup code:"
journalctl -u node-go --no-pager | grep -o 'setup code: [A-Z0-9-]*' | tail -1 || true
