#!/bin/sh
# Open Brand Exchange brand portal: install or update on a Linux server.
#
#   curl -fsSL https://openbrandexchange.com/install.sh | sudo sh -s -- --domain portal.yourbrand.com
#
# Installs the newest release of the portal (node-go, or the node-ts standalone
# binary with --edition ts) as a systemd service with automatic HTTPS from
# Let's Encrypt. Running it again updates the portal in place and keeps its
# settings and data. Run with --help for every option, or with --dry-run to
# see what it would do without changing anything.
#
# Source: https://github.com/openbrandexchange/node-go/blob/main/install.sh
# Licence: AGPL-3.0-only (as node-go).
set -eu

usage() {
	cat <<'EOF'
Install or update the Open Brand Exchange brand portal on this Linux server.

  curl -fsSL https://openbrandexchange.com/install.sh | sudo sh -s -- --domain portal.yourbrand.com

Options:
  --domain NAME      the portal's address, e.g. portal.yourbrand.com (needed for the first install)
  --email ADDRESS    your e-mail address for Let's Encrypt (optional; certificate notices)
  --version X.Y.Z    install this release instead of the newest one
  --edition go|ts    go (default): the Go edition (node-go);
                     ts: the TypeScript edition's standalone binary (obx-portal), with Caddy for HTTPS
  --behind-proxy     no HTTPS of its own: serve plain http on port 8080 for your own proxy
  --force            continue even when a check (DNS, ports) fails
  --archive FILE     install from this release archive instead of downloading one
                     (for servers without access to GitHub; verify it yourself first)
  --dry-run          only show what would happen; change nothing
  --uninstall        remove the program and the service; settings and data stay
  --purge            with --uninstall: also delete the settings, the data and the obx user
  -h, --help         this text

Run the same command again to update: settings and data are kept.
EOF
}

# --- output ---------------------------------------------------------------

if [ -t 1 ]; then
	B="$(printf '\033[1m')"
	R="$(printf '\033[31m')"
	Y="$(printf '\033[33m')"
	G="$(printf '\033[32m')"
	N="$(printf '\033[0m')"
else
	B="" R="" Y="" G="" N=""
fi
say() { printf '%s\n' "$*"; }
step() { printf '\n%s==> %s%s\n' "$B" "$*" "$N"; }
ok() { printf '  %sok%s  %s\n' "$G" "$N" "$*"; }
warn() { printf '  %swarning:%s %s\n' "$Y" "$N" "$*" >&2; }
die() {
	printf '\n%serror:%s %s\n' "$R" "$N" "$*" >&2
	exit 1
}
# fail stops for a failed check, unless --force.
fail() {
	if [ "$FORCE" = 1 ]; then
		warn "$* (continuing because of --force)"
	else
		printf '\n%sstopped:%s %s\n\nFix this and run the same command again, or add --force to continue anyway.\n' "$R" "$N" "$*" >&2
		exit 1
	fi
}

# run executes a command, or prints it with --dry-run.
run() {
	if [ "$DRY_RUN" = 1 ]; then
		printf '  [dry-run] %s\n' "$*"
	else
		"$@"
	fi
}

# write_file PATH MODE writes stdin to PATH (mode MODE), or shows it with --dry-run.
write_file() {
	if [ "$DRY_RUN" = 1 ]; then
		printf '  [dry-run] write %s (mode %s):\n' "$1" "$2"
		sed -e 's/^\(OBX_SECRET_KEY=\).*/\1<generated>/' -e 's/^/      | /'
	else
		umask 077
		cat >"$1.tmp.$$"
		chmod "$2" "$1.tmp.$$"
		mv -f "$1.tmp.$$" "$1"
		umask 022
	fi
}

# --- arguments ------------------------------------------------------------

DOMAIN=""
EMAIL=""
VERSION=""
EDITION=""
ARCHIVE_FILE=""
BEHIND_PROXY=0
FORCE=0
DRY_RUN=0
UNINSTALL=0
PURGE=0
need_arg() { [ $# -ge 2 ] && [ -n "$2" ] || die "$1 needs a value (see --help)"; }
while [ $# -gt 0 ]; do
	case "$1" in
	--domain) need_arg "$@"; DOMAIN="$2"; shift 2 ;;
	--domain=*) DOMAIN="${1#*=}"; shift ;;
	--email) need_arg "$@"; EMAIL="$2"; shift 2 ;;
	--email=*) EMAIL="${1#*=}"; shift ;;
	--version) need_arg "$@"; VERSION="$2"; shift 2 ;;
	--version=*) VERSION="${1#*=}"; shift ;;
	--edition) need_arg "$@"; EDITION="$2"; shift 2 ;;
	--edition=*) EDITION="${1#*=}"; shift ;;
	--archive) need_arg "$@"; ARCHIVE_FILE="$2"; shift 2 ;;
	--archive=*) ARCHIVE_FILE="${1#*=}"; shift ;;
	--behind-proxy) BEHIND_PROXY=1; shift ;;
	--force) FORCE=1; shift ;;
	--dry-run) DRY_RUN=1; shift ;;
	--uninstall) UNINSTALL=1; shift ;;
	--purge) PURGE=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown option $1 (see --help)" ;;
	esac
done
[ "$PURGE" = 0 ] || [ "$UNINSTALL" = 1 ] || die "--purge only goes with --uninstall"
VERSION="${VERSION#v}"
DOMAIN="$(printf '%s' "$DOMAIN" | tr '[:upper:]' '[:lower:]' | sed -e 's#^https\{0,1\}://##' -e 's#/.*$##' -e 's/\.$//')"

# Without --edition: the edition already installed here, else go.
if [ -z "$EDITION" ]; then
	if [ -f /etc/systemd/system/obx-portal.service ] && [ ! -f /etc/systemd/system/node-go.service ]; then
		EDITION=ts
	else
		EDITION=go
	fi
fi

case "$EDITION" in
go)
	REPO=openbrandexchange/node-go
	SERVICE=node-go
	BIN=/usr/local/bin/node-go
	DATA=/var/lib/node-go
	CONF_DIR=/etc/node-go
	;;
ts)
	REPO=openbrandexchange/node-ts
	SERVICE=obx-portal
	BIN=/opt/obx-portal/obx-portal
	DATA=/var/lib/obx-portal
	CONF_DIR=/etc/obx-portal
	;;
*) die "--edition must be go or ts" ;;
esac
ENV_FILE="$CONF_DIR/$SERVICE.env"
UNIT="/etc/systemd/system/$SERVICE.service"
CADDY_SITE=/etc/caddy/obx-portal.caddy
CADDY_MARK="# Managed by the Open Brand Exchange installer"
USER_NAME=obx

[ "$DRY_RUN" = 0 ] || say "${B}Dry run:${N} nothing is changed; the steps that would change something start with [dry-run]."

# --- the system -----------------------------------------------------------

step "Checking this server"
OS="$(uname -s)"
case "$(uname -m)" in
x86_64 | amd64) ARCH=amd64 TS_ARCH=x64 ;;
aarch64 | arm64) ARCH=arm64 TS_ARCH=arm64 ;;
*) die "this processor type ($(uname -m)) is not supported; the portal runs on 64-bit Intel/AMD (amd64) and ARM (arm64)" ;;
esac
if [ "$OS" != Linux ]; then
	[ "$DRY_RUN" = 1 ] || die "this installer is for Linux servers (this is $OS). On your own computer, try the portal with: go run . dev (see the README)"
	warn "this is $OS, not Linux: showing what would happen on a Linux $ARCH server"
else
	ok "Linux, $ARCH"
fi
if [ "$(id -u)" != 0 ]; then
	[ "$DRY_RUN" = 1 ] || die "run it as root: put sudo before sh (curl -fsSL https://openbrandexchange.com/install.sh | sudo sh -s -- ...)"
	warn "not running as root (fine for --dry-run)"
fi
if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
	ok "systemd"
elif [ "$DRY_RUN" = 1 ]; then
	warn "systemd is not running here (needed for a real install)"
else
	die "this server does not run systemd, which the installer uses to keep the portal running. Use a distribution with systemd (Ubuntu, Debian, Fedora, Rocky/Alma Linux, openSUSE) or install the binary yourself (see docs/hosting/linux.md)"
fi
if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL --retry 3 --connect-timeout 15 "$1"; }
	fetch_to() { curl -fsSL --retry 3 --connect-timeout 15 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -qO- --tries=3 --timeout=30 "$1"; }
	fetch_to() { wget -qO "$2" --tries=3 --timeout=30 "$1"; }
else
	die "curl or wget is needed (Debian/Ubuntu: apt-get install -y curl)"
fi
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	die "sha256sum is needed to verify the download (Debian/Ubuntu: apt-get install -y coreutils)"
fi
command -v tar >/dev/null 2>&1 || die "tar is needed (Debian/Ubuntu: apt-get install -y tar)"

# --- uninstall ------------------------------------------------------------

caddy_unconfigure() {
	[ -f "$CADDY_SITE" ] || return 0
	run rm -f "$CADDY_SITE"
	if grep -q "^$CADDY_MARK" /etc/caddy/Caddyfile 2>/dev/null && [ -f /etc/caddy/Caddyfile.before-obx ]; then
		run mv -f /etc/caddy/Caddyfile.before-obx /etc/caddy/Caddyfile
	elif [ -f /etc/caddy/Caddyfile ]; then
		run sed -i '\#^import /etc/caddy/obx-portal.caddy$#d' /etc/caddy/Caddyfile
	fi
	if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet caddy 2>/dev/null; then
		run systemctl reload caddy || true
	fi
}

if [ "$UNINSTALL" = 1 ]; then
	step "Removing the portal ($SERVICE)"
	if [ -f "$UNIT" ]; then
		run systemctl disable --now "$SERVICE" || true
		run rm -f "$UNIT"
		run systemctl daemon-reload
	fi
	if [ "$EDITION" = ts ]; then
		run rm -rf /opt/obx-portal
		caddy_unconfigure
	else
		run rm -f "$BIN"
	fi
	if [ "$DRY_RUN" = 1 ]; then
		say "Dry run finished: nothing was removed."
	elif [ "$PURGE" = 1 ]; then
		run rm -rf "$DATA" "$CONF_DIR"
		if id "$USER_NAME" >/dev/null 2>&1 && [ ! -d /var/lib/node-go ] && [ ! -d /var/lib/obx-portal ]; then
			run userdel "$USER_NAME" || true
		fi
		say "Removed the portal, its settings and its data."
	else
		say "Removed the program and the service. Kept the settings ($ENV_FILE) and the data ($DATA):"
		say "running the installer again brings the portal back as it was; --uninstall --purge deletes them."
	fi
	exit 0
fi

# --- settings -------------------------------------------------------------

env_get() { [ -f "$ENV_FILE" ] && sed -n "s/^$1=//p" "$ENV_FILE" | tail -n 1 | tr -d '"'"'"; }
UPDATE=0
if [ -f "$ENV_FILE" ]; then
	UPDATE=1
	OLD_DOMAIN="$(env_get OBX_DOMAIN)"
	[ -n "$OLD_DOMAIN" ] || OLD_DOMAIN="$(env_get OBX_PUBLIC_URL | sed -e 's#^https\{0,1\}://##' -e 's#[:/].*$##')"
	[ -n "$DOMAIN" ] || DOMAIN="$OLD_DOMAIN"
	# An earlier install without built-in HTTPS (behind Caddy or another proxy) stays that way.
	if [ "$(env_get OBX_TLS)" != auto ] && [ "$EDITION" = go ] && [ "$BEHIND_PROXY" = 0 ]; then
		if grep -q '^OBX_TRUST_PROXY=true' "$ENV_FILE"; then
			BEHIND_PROXY=1
			say "  This portal runs behind a proxy (OBX_TRUST_PROXY=true in $ENV_FILE); keeping that."
			say "  To let the portal do HTTPS itself instead, stop the proxy and set OBX_TLS=auto and OBX_TRUST_PROXY=false there."
		fi
	fi
	if [ "$EDITION" = ts ] && [ "$BEHIND_PROXY" = 0 ] && [ ! -f "$CADDY_SITE" ]; then
		BEHIND_PROXY=1
	fi
fi
[ -n "$DOMAIN" ] || die "say which address the portal gets, for example: --domain portal.yourbrand.com (see --help)"
case "$DOMAIN" in
*.*) ;;
*) die "--domain must be a full domain name such as portal.yourbrand.com, got \"$DOMAIN\"" ;;
esac
if printf '%s' "$DOMAIN" | grep -Eq '[^a-z0-9.-]|^[0-9.]+$'; then
	die "--domain must be a domain name such as portal.yourbrand.com (not an IP address or a URL), got \"$DOMAIN\""
fi
if [ -n "$EMAIL" ] && ! printf '%s' "$EMAIL" | grep -Eq '^[^@ <>]+@[^@ <>]+\.[^@ <>]+$'; then
	die "--email must be a plain e-mail address such as you@yourbrand.com"
fi
if [ "$BEHIND_PROXY" = 1 ]; then
	ok "address: https://$DOMAIN (served by your own proxy, which forwards to port 8080)"
else
	ok "address: https://$DOMAIN"
fi

# --- disk -----------------------------------------------------------------

disk_dir=/var/lib
[ -d "$DATA" ] && disk_dir="$DATA"
free_kb="$(df -Pk "$disk_dir" 2>/dev/null | awk 'NR==2 {print $4}')"
if [ -n "$free_kb" ]; then
	free_gb=$((free_kb / 1048576))
	if [ "$free_kb" -lt 1048576 ]; then
		fail "less than 1 GB of free disk space in $disk_dir. The program needs about 100 MB; your images need room too. Free up space or pick a larger server"
	elif [ "$free_kb" -lt 5242880 ]; then
		warn "only $free_gb GB free in $disk_dir: enough to start, but images need room (count about 2x the size of your originals)"
	else
		ok "$free_gb GB free disk space"
	fi
fi

# --- DNS ------------------------------------------------------------------

# resolve TYPE NAME prints the A or AAAA addresses of NAME, one per line.
resolve() {
	if command -v dig >/dev/null 2>&1; then
		dig +short +time=5 +tries=2 "$2" "$1" 2>/dev/null | grep -E '^[0-9a-fA-F:.]+$' || true
	elif command -v getent >/dev/null 2>&1 && getent ahosts localhost >/dev/null 2>&1; then
		if [ "$1" = A ]; then
			getent ahostsv4 "$2" 2>/dev/null | awk '{print $1}' | sort -u || true
		else
			getent ahostsv6 "$2" 2>/dev/null | awk '$1 !~ /^::ffff:/ {print $1}' | sort -u || true
		fi
	elif command -v host >/dev/null 2>&1; then
		host -t "$1" "$2" 2>/dev/null | awk '/has (IPv6 )?address/ {print $NF}' || true
	fi
}
# public_ip 4|6 prints this server's public address as the internet sees it.
public_ip() {
	if command -v curl >/dev/null 2>&1; then
		curl -"$1" -fsS --max-time 8 "https://api$([ "$1" = 6 ] && echo 6).ipify.org" 2>/dev/null || true
	else
		wget -"$1" -qO- --timeout=8 "https://api$([ "$1" = 6 ] && echo 6).ipify.org" 2>/dev/null || true
	fi
}
# local_ips 4|6 prints the global addresses configured on this server.
local_ips() {
	command -v ip >/dev/null 2>&1 || return 0
	ip -o "-$1" addr show scope global 2>/dev/null | awk '{sub(/\/.*/, "", $4); print $4}' || true
}
in_list() { printf '%s\n' "$2" | grep -qxF "$1"; }

step "Checking that $DOMAIN points to this server"
PUB4="$(public_ip 4)"
PUB6="$(public_ip 6)"
MINE4="$(printf '%s\n%s\n' "$PUB4" "$(local_ips 4)" | grep -v '^$' | sort -u || true)"
MINE6="$(printf '%s\n%s\n' "$PUB6" "$(local_ips 6)" | grep -v '^$' | sort -u || true)"
DNS4="$(resolve A "$DOMAIN")"
DNS6="$(resolve AAAA "$DOMAIN")"
say "  this server: ${PUB4:-no public IPv4 found}${PUB6:+ and $PUB6}"
say "  $DOMAIN: $(printf '%s' "$DNS4 $DNS6" | tr '\n' ' ' | sed 's/^ *$/no A or AAAA record yet/')"

dns_help() {
	rec="${DOMAIN%%.*}"
	note="(\"$rec\" is the part before your domain name; some providers want the full name $DOMAIN.)"
	case "$DOMAIN" in
	*.*.*) ;;
	*) rec="@" note="(\"@\" means the domain itself; some providers want $DOMAIN or leave the name empty.)" ;;
	esac
	cat <<EOF

  Add this record where your domain's DNS is managed (TransIP, Hostnet, Strato, one.com,
  Cloudflare, GoDaddy, ...; often under "DNS" or "DNS settings" of the domain):

EOF
	printf '      %-5s %-24s %s\n' Type Name Value A "$rec" "${PUB4:-<the IPv4 address of this server>}"
	if [ -n "$PUB6" ]; then
		printf '      %-5s %-24s %s\n' AAAA "$rec" "$PUB6"
	fi
	cat <<EOF

  $note
  On Cloudflare, set the proxy status to "DNS only" (grey cloud).
  It usually works within 5 to 60 minutes. Check with: dig +short $DOMAIN
  (or an online DNS checker), then run this installer again.
EOF
}

if [ "$BEHIND_PROXY" = 1 ]; then
	if [ -z "$DNS4$DNS6" ]; then
		warn "$DOMAIN has no DNS record yet; point it at your proxy or load balancer"
	else
		ok "$DOMAIN has a DNS record (pointing at your proxy is fine with --behind-proxy)"
	fi
elif [ -z "$DNS4$DNS6" ]; then
	dns_help >&2
	fail "$DOMAIN has no A record yet, so Let's Encrypt cannot check it and the portal gets no certificate"
else
	bad4=""
	good=0
	for a in $DNS4; do
		if in_list "$a" "$MINE4"; then good=1; else bad4="$bad4 $a"; fi
	done
	bad6=""
	for a in $DNS6; do
		if in_list "$a" "$MINE6"; then good=1; else bad6="$bad6 $a"; fi
	done
	if [ -z "$MINE4$MINE6" ]; then
		warn "could not find this server's public address (no internet connection to api.ipify.org?); not checking DNS"
	elif [ "$good" = 0 ]; then
		dns_help >&2
		fail "$DOMAIN points to$bad4$bad6, which is not this server. If you just changed the record, wait a little (up to an hour) and try again"
	elif [ -n "$bad4$bad6" ]; then
		if [ -n "$bad6" ] && [ -z "$PUB6" ]; then
			fail "$DOMAIN has an AAAA (IPv6) record ($bad6) but this server has no IPv6 address. Let's Encrypt tries IPv6 first and would fail: delete the AAAA record, or give the server an IPv6 address and use that"
		else
			fail "$DOMAIN also points to$bad4$bad6, which is not this server. Remove or correct that record: Let's Encrypt may check that address and fail"
		fi
	else
		ok "$DOMAIN points to this server"
	fi
fi

# --- ports and firewall ---------------------------------------------------

# listener PORT prints the program listening on TCP PORT ("" when free).
listener() {
	if command -v ss >/dev/null 2>&1; then
		line="$(ss -Hltnp "sport = :$1" 2>/dev/null | head -n 1)"
		[ -n "$line" ] || return 0
		name="$(printf '%s' "$line" | sed -n 's/.*users:(("\([^"]*\)".*/\1/p')"
		printf '%s\n' "${name:-another program}"
	elif command -v lsof >/dev/null 2>&1; then
		lsof -nP -iTCP:"$1" -sTCP:LISTEN 2>/dev/null | awk 'NR==2 {print $1}'
	fi
	return 0
}

step "Checking ports"
if [ "$BEHIND_PROXY" = 1 ]; then
	PORTS=8080
	if [ "$EDITION" = go ]; then OWN="node-go"; else OWN="obx-portal"; fi
else
	PORTS="80 443"
	if [ "$EDITION" = go ]; then OWN="node-go"; else OWN="caddy"; fi
fi
if ! command -v ss >/dev/null 2>&1 && ! command -v lsof >/dev/null 2>&1; then
	warn "cannot check which ports are in use (ss is missing)"
fi
for p in $PORTS; do
	who="$(listener "$p" | head -n 1)"
	if [ -z "$who" ]; then
		ok "port $p is free"
	elif [ "$who" = "$OWN" ]; then
		ok "port $p is used by $who (this portal)"
	else
		case "$who" in
		nginx | apache2 | httpd | caddy | traefik | haproxy | lighttpd)
			fail "port $p is in use by $who. The portal needs ports 80 and 443 for HTTPS. Stop it (sudo systemctl disable --now $who) if this server is only for the portal, or keep $who and run the installer with --behind-proxy, then let $who forward $DOMAIN to http://127.0.0.1:8080"
			;;
		*)
			fail "port $p is in use by $who. Stop that program, or run the installer with --behind-proxy"
			;;
		esac
	fi
done

if [ "$BEHIND_PROXY" = 0 ]; then
	if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
		rules="$(ufw status 2>/dev/null)"
		if printf '%s\n' "$rules" | grep -Eq '^(80(/tcp)?|80,443/tcp|Nginx Full|Apache Full|WWW Full)[[:space:]].*ALLOW' &&
			printf '%s\n' "$rules" | grep -Eq '^(443(/tcp)?|80,443/tcp|Nginx Full|Apache Full|WWW Full)[[:space:]].*ALLOW'; then
			ok "the firewall (ufw) allows ports 80 and 443"
		else
			warn "the firewall (ufw) does not allow ports 80 and 443 yet. Open them with:"
			say "      sudo ufw allow 80/tcp && sudo ufw allow 443/tcp"
		fi
	elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
		svcs="$(firewall-cmd --list-services 2>/dev/null) $(firewall-cmd --list-ports 2>/dev/null)"
		case " $svcs " in
		*" http "*" https "* | *" https "*" http "* | *" 80/tcp "*" 443/tcp "* | *" 443/tcp "*" 80/tcp "*)
			ok "the firewall (firewalld) allows ports 80 and 443"
			;;
		*)
			warn "the firewall (firewalld) does not allow ports 80 and 443 yet. Open them with:"
			say "      sudo firewall-cmd --permanent --add-service=http --add-service=https && sudo firewall-cmd --reload"
			;;
		esac
	else
		ok "no local firewall (ufw/firewalld) is blocking"
	fi
	say "  Your hosting company may have a firewall of its own (Hetzner Cloud Firewall, DigitalOcean"
	say "  Cloud Firewall, Scaleway security group, ...): it must allow TCP ports 80 and 443 too."
fi

# --- download -------------------------------------------------------------

TMP="$(mktemp -d "${TMPDIR:-/tmp}/obx-install.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
trap 'exit 130' INT TERM
mkdir "$TMP/x"
if [ -n "$ARCHIVE_FILE" ]; then
	step "Unpacking $ARCHIVE_FILE"
	[ -f "$ARCHIVE_FILE" ] || die "$ARCHIVE_FILE does not exist"
	ARCHIVE="$(basename "$ARCHIVE_FILE")"
	if [ -z "$VERSION" ]; then
		VERSION="$(printf '%s' "$ARCHIVE" | sed -n 's/^[a-z-]*[_-]\([0-9][0-9.]*[0-9a-z.-]*\)[_-]linux.*/\1/p')"
		[ -n "$VERSION" ] || VERSION=local
	fi
	tar -xzf "$ARCHIVE_FILE" -C "$TMP/x" || die "$ARCHIVE_FILE is not a release archive (.tar.gz)"
	ok "$ARCHIVE ($VERSION; not verified: you supplied the file)"
else
	step "Downloading the portal"
	if [ -z "$VERSION" ]; then
		VERSION="$(fetch "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null |
			sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"v\{0,1\}\([^"]*\)".*/\1/p' | head -n 1)"
		[ -n "$VERSION" ] || die "could not find the newest release on github.com/$REPO/releases (no internet connection, or GitHub's rate limit; pass --version X.Y.Z)"
	fi
	if [ "$EDITION" = go ]; then
		ARCHIVE="node-go_${VERSION}_linux_${ARCH}.tar.gz"
		SUMS=checksums.txt
	else
		ARCHIVE="obx-portal-${VERSION}-linux-${TS_ARCH}.tar.gz"
		SUMS=SHA256SUMS
	fi
	BASE="https://github.com/$REPO/releases/download/v$VERSION"
	fetch_to "$BASE/$ARCHIVE" "$TMP/$ARCHIVE" || die "could not download $BASE/$ARCHIVE (does release $VERSION exist? see https://github.com/$REPO/releases)"
	fetch_to "$BASE/$SUMS" "$TMP/$SUMS" || die "could not download $BASE/$SUMS to verify the download"
	want="$(awk -v f="$ARCHIVE" '$2 == f || $2 == "*" f {print $1}' "$TMP/$SUMS")"
	got="$(sha256 "$TMP/$ARCHIVE")"
	[ -n "$want" ] || die "$SUMS of release $VERSION does not list $ARCHIVE"
	[ "$want" = "$got" ] || die "the download of $ARCHIVE is damaged or was changed (sha256 $got, expected $want); nothing was installed"
	ok "$ARCHIVE ($VERSION), sha256 verified"
	tar -xzf "$TMP/$ARCHIVE" -C "$TMP/x"
fi
if [ "$EDITION" = go ]; then
	NEW_BIN="$TMP/x/node-go"
	[ -f "$NEW_BIN" ] || die "the archive has no node-go program"
	if [ "$BEHIND_PROXY" = 0 ] && ! grep -q OBX_TLS "$NEW_BIN"; then
		die "release $VERSION has no built-in HTTPS yet (added after 0.3.0). Install a newer release with --version, or use --behind-proxy with your own HTTPS proxy"
	fi
else
	NEW_DIR=""
	for d in "$TMP"/x/obx-portal-*; do [ -f "$d/obx-portal" ] && NEW_DIR="$d"; done
	[ -n "$NEW_DIR" ] || die "the archive has no obx-portal program"
fi

# --- install --------------------------------------------------------------

random_string() {
	if command -v openssl >/dev/null 2>&1; then
		openssl rand -base64 48 | tr -d '\n'
	else
		head -c 48 /dev/urandom | base64 | tr -d '\n'
	fi
}
setup_code() {
	c="$(LC_ALL=C tr -dc 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789' </dev/urandom | head -c 12)"
	printf '%s-%s-%s' "$(printf '%s' "$c" | cut -c1-4)" "$(printf '%s' "$c" | cut -c5-8)" "$(printf '%s' "$c" | cut -c9-12)"
}
# set_env KEY VALUE changes or adds KEY in the settings file.
set_env() {
	if grep -q "^$1=" "$ENV_FILE" 2>/dev/null; then
		run sed -i "s|^$1=.*|$1=$2|" "$ENV_FILE"
	else
		if [ "$DRY_RUN" = 1 ]; then printf '  [dry-run] add %s=%s to %s\n' "$1" "$2" "$ENV_FILE"; else printf '%s=%s\n' "$1" "$2" >>"$ENV_FILE"; fi
	fi
}

if [ "$UPDATE" = 1 ]; then
	step "Updating the portal to $VERSION (settings and data are kept)"
else
	step "Installing the portal $VERSION"
fi

if ! id "$USER_NAME" >/dev/null 2>&1; then
	nologin=/usr/sbin/nologin
	[ -x "$nologin" ] || nologin=/sbin/nologin
	run useradd --system --home-dir "$DATA" --no-create-home --shell "$nologin" --user-group "$USER_NAME"
fi
run install -d -o "$USER_NAME" -g "$USER_NAME" -m 0750 "$DATA"
run install -d -m 0755 "$CONF_DIR"

if [ "$EDITION" = go ]; then
	run install -m 0755 "$NEW_BIN" "$BIN"
else
	run rm -rf /opt/obx-portal.new
	run cp -R "$NEW_DIR" /opt/obx-portal.new
	run chown -R root:root /opt/obx-portal.new
	if [ -d /opt/obx-portal ]; then run mv /opt/obx-portal /opt/obx-portal.old; fi
	run mv /opt/obx-portal.new /opt/obx-portal
	run rm -rf /opt/obx-portal.old
fi
ok "program in $BIN"

SETUP_CODE=""
if [ "$UPDATE" = 0 ]; then
	SETUP_CODE="$(setup_code)"
	SECRET="$(random_string)"
	if [ "$EDITION" = go ]; then
		if [ "$BEHIND_PROXY" = 1 ]; then
			addr_lines="OBX_PUBLIC_URL=https://$DOMAIN
PORT=8080
# Every request comes through your proxy, which sets X-Forwarded-For:
OBX_TRUST_PROXY=true"
		else
			addr_lines="OBX_DOMAIN=$DOMAIN
# HTTPS with a Let's Encrypt certificate on ports 80 and 443, renewed automatically:
OBX_TLS=auto
OBX_ACME_EMAIL=$EMAIL"
		fi
		write_file "$ENV_FILE" 0600 <<EOF
# Settings of the brand portal (node-go). After a change: sudo systemctl restart node-go
# All settings: https://github.com/openbrandexchange/node-go/blob/main/docs/configuration.md
$addr_lines
OBX_DATA_DIR=$DATA
# Keep a copy of this key in your password manager: it encrypts stored passwords and cannot be
# changed once there is data.
OBX_SECRET_KEY=$SECRET
# One-time code for the setup wizard (only needed until the setup is done).
OBX_SETUP_CODE=$SETUP_CODE
# E-mail (invitations, "your download is ready"):
# OBX_SMTP_URL=smtp://user:password@smtp.example.com:587
# OBX_MAIL_FROM=portal@${DOMAIN#*.}
EOF
	else
		write_file "$ENV_FILE" 0600 <<EOF
# Settings of the brand portal (node-ts standalone). After a change: sudo systemctl restart obx-portal
# All settings: https://github.com/openbrandexchange/node-ts/blob/main/.env.example
OBX_PUBLIC_URL=https://$DOMAIN
OBX_DATA_DIR=$DATA
HOST=127.0.0.1
PORT=8080
# Every request comes through the proxy (Caddy) on this server:
OBX_TRUST_PROXY=true
# Keep a copy of this key in your password manager: it encrypts stored passwords and cannot be
# changed once there is data.
OBX_SECRET_KEY=$SECRET
# One-time code for the setup wizard (only needed until the setup is done).
OBX_SETUP_CODE=$SETUP_CODE
# E-mail: OBX_MAIL_PROVIDER=resend|postmark|sendgrid|mailchannels|webhook, OBX_MAIL_API_KEY, OBX_MAIL_FROM
EOF
	fi
	ok "settings in $ENV_FILE"
else
	if [ "$EDITION" = go ]; then
		if [ -n "$(env_get OBX_DOMAIN)" ]; then
			if [ "$(env_get OBX_DOMAIN)" != "$DOMAIN" ]; then
				say "  The address changes from $(env_get OBX_DOMAIN) to $DOMAIN."
				set_env OBX_DOMAIN "$DOMAIN"
			fi
		elif [ "$(env_get OBX_PUBLIC_URL)" != "https://$DOMAIN" ]; then
			say "  The address changes to https://$DOMAIN."
			set_env OBX_PUBLIC_URL "https://$DOMAIN"
		fi
		if [ -n "$EMAIL" ] && [ "$(env_get OBX_ACME_EMAIL)" != "$EMAIL" ]; then set_env OBX_ACME_EMAIL "$EMAIL"; fi
	elif [ "$(env_get OBX_PUBLIC_URL)" != "https://$DOMAIN" ]; then
		say "  The address changes to https://$DOMAIN."
		set_env OBX_PUBLIC_URL "https://$DOMAIN"
	fi
	ok "kept the settings in $ENV_FILE"
fi

# The service. Written on every run, so updates bring the newest hardening.
if [ "$EDITION" = go ]; then
	exec_line="ExecStart=$BIN serve"
	extra="# Lets the unprivileged obx user open ports 80 and 443, nothing else.
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
MemoryDenyWriteExecute=true"
	desc="Brand portal (Open Brand Exchange, node-go)"
else
	exec_line="WorkingDirectory=/opt/obx-portal
ExecStart=$BIN"
	extra="CapabilityBoundingSet="
	desc="Brand portal (Open Brand Exchange, node-ts standalone)"
fi
write_file "$UNIT" 0644 <<EOF
# Written by the Open Brand Exchange installer (install.sh); running it again overwrites this file.
# Own settings belong in $ENV_FILE, or in: systemctl edit $SERVICE
[Unit]
Description=$desc
After=network-online.target
Wants=network-online.target

[Service]
User=$USER_NAME
Group=$USER_NAME
EnvironmentFile=$ENV_FILE
$exec_line
Restart=on-failure
RestartSec=3
# Running requests get 25 s to finish on stop; jobs resume after a restart.
TimeoutStopSec=40
LimitNOFILE=65536
MemoryMax=75%
$extra
# Hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ReadWritePaths=$DATA
UMask=0027
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectControlGroups=true
ProtectClock=true
ProtectHostname=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
LockPersonality=true
SystemCallArchitectures=native

[Install]
WantedBy=multi-user.target
EOF
ok "service $SERVICE ($UNIT)"

# Caddy for HTTPS in front of the TypeScript edition.
if [ "$EDITION" = ts ] && [ "$BEHIND_PROXY" = 0 ]; then
	if ! command -v caddy >/dev/null 2>&1; then
		say "  Installing Caddy for HTTPS (https://caddyserver.com)"
		if [ "$DRY_RUN" = 1 ] && ! command -v apt-get >/dev/null 2>&1 && ! command -v dnf >/dev/null 2>&1; then
			say "  [dry-run] apt-get install -y caddy (Debian/Ubuntu) or dnf install -y caddy (Fedora/RHEL)"
		elif command -v apt-get >/dev/null 2>&1; then
			run apt-get update -q
			run env DEBIAN_FRONTEND=noninteractive apt-get install -y -q caddy ||
				die "could not install Caddy from the package sources. Install it as described on https://caddyserver.com/docs/install and run this installer again, or use --behind-proxy"
		elif command -v dnf >/dev/null 2>&1; then
			run dnf install -y caddy ||
				die "could not install Caddy (on RHEL/Rocky/Alma enable EPEL or Caddy's COPR first, see https://caddyserver.com/docs/install), then run this installer again; or use --behind-proxy"
		else
			die "install Caddy as described on https://caddyserver.com/docs/install and run this installer again, or use --behind-proxy"
		fi
	fi
	tls_line=""
	[ -z "$EMAIL" ] || tls_line="	tls $EMAIL"
	write_file "$CADDY_SITE" 0644 <<EOF
$CADDY_MARK (install.sh); running it again overwrites this file.
$DOMAIN {
$tls_line
	encode zstd gzip
	request_body {
		max_size 2GB
	}
	reverse_proxy 127.0.0.1:8080 {
		transport http {
			read_timeout 30m
			write_timeout 30m
		}
	}
}
EOF
	if [ ! -f /etc/caddy/Caddyfile ] || grep -q '^[[:space:]]*root \* /usr/share/caddy' /etc/caddy/Caddyfile; then
		# The package's example site (or none): replace it, keep the original.
		if [ -f /etc/caddy/Caddyfile ]; then run cp /etc/caddy/Caddyfile /etc/caddy/Caddyfile.before-obx; fi
		printf '%s; the original is in Caddyfile.before-obx.\nimport /etc/caddy/obx-portal.caddy\n' "$CADDY_MARK" | write_file /etc/caddy/Caddyfile 0644
	elif ! grep -q '^import /etc/caddy/obx-portal.caddy$' /etc/caddy/Caddyfile; then
		say "  Adding 'import /etc/caddy/obx-portal.caddy' to your existing /etc/caddy/Caddyfile"
		if [ "$DRY_RUN" = 1 ]; then say "  [dry-run] append import /etc/caddy/obx-portal.caddy"; else printf '\nimport /etc/caddy/obx-portal.caddy\n' >>/etc/caddy/Caddyfile; fi
	fi
	ok "Caddy serves https://$DOMAIN ($CADDY_SITE)"
fi

step "Starting the portal"
run systemctl daemon-reload
run systemctl enable --quiet "$SERVICE"
run systemctl restart "$SERVICE"
if [ "$EDITION" = ts ] && [ "$BEHIND_PROXY" = 0 ]; then
	run systemctl enable --quiet caddy
	run systemctl reload-or-restart caddy
fi

if [ "$DRY_RUN" = 1 ]; then
	say ""
	say "Dry run finished: nothing was changed."
	exit 0
fi

# Wait until the portal answers locally.
if [ "$EDITION" = go ] && [ "$BEHIND_PROXY" = 0 ]; then
	health="http://127.0.0.1:80/healthz"
else
	health="http://127.0.0.1:8080/healthz"
fi
i=0
until fetch "$health" >/dev/null 2>&1; do
	i=$((i + 1))
	if [ "$i" -ge 30 ] || ! systemctl is-active --quiet "$SERVICE"; then
		journalctl -u "$SERVICE" -n 20 --no-pager >&2 || true
		die "the portal did not start; the last log lines are above (all of them: sudo journalctl -u $SERVICE)"
	fi
	sleep 1
done
ok "the portal runs"

# The first https request makes the portal (or Caddy) get its certificate,
# which also proves that Let's Encrypt can reach this server from outside.
if [ "$BEHIND_PROXY" = 0 ]; then
	say "  Getting the HTTPS certificate from Let's Encrypt (up to a minute)..."
	i=0
	cert_ok=0
	while [ "$i" -lt 6 ]; do
		if fetch "https://$DOMAIN/healthz" >/dev/null 2>&1; then
			cert_ok=1
			break
		fi
		i=$((i + 1))
		sleep 10
	done
	if [ "$cert_ok" = 1 ]; then
		ok "https://$DOMAIN works, with a valid certificate"
	else
		warn "https://$DOMAIN does not answer yet. The portal keeps trying when someone opens it. Usual causes:"
		say "      - DNS does not point here yet (wait up to an hour; check with: dig +short $DOMAIN)"
		say "      - ports 80 and 443 are closed in a firewall (this server's, or your hosting company's)"
		say "      - see the log: sudo journalctl -u $SERVICE -n 50"
	fi
fi

step "Done"
if [ "$UPDATE" = 1 ]; then
	say "The portal runs version $VERSION at ${B}https://$DOMAIN${N}"
else
	say "Open ${B}https://$DOMAIN${N} and enter this one-time setup code in the setup wizard:"
	say ""
	say "    ${B}$SETUP_CODE${N}"
	say ""
	say "Then choose your company name and your own password. The code is also in $ENV_FILE"
	say "(OBX_SETUP_CODE) and in the log: sudo journalctl -u $SERVICE | grep 'setup code'"
fi
say ""
say "Settings:  $ENV_FILE   (then: sudo systemctl restart $SERVICE)"
say "Data:      $DATA   (back it up; see the hosting guide)"
say "Log:       sudo journalctl -u $SERVICE -f"
say "Update:    run the same install command again"
if [ "$EDITION" = go ]; then
	say "Guide:     https://github.com/openbrandexchange/node-go/blob/main/docs/hosting/linux.md"
else
	say "Guide:     https://github.com/openbrandexchange/node-ts/blob/main/docs/hosting-standalone.md"
fi
