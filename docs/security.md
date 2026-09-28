# Security

How the portal protects the brand's content and the people who use it. It implements the rules of the spec's [security.md](https://github.com/openbrandexchange/spec/blob/main/docs/security.md) and passes the conformance suite's security profile. Report problems privately: [SECURITY.md](../SECURITY.md).

## Accounts and sessions

- **No default password.** A fresh portal can only be set up with its one-time setup code (printed in the log, or chosen with `OBX_SETUP_CODE`); the code stops working once setup is done.
- Passwords: at least 10 characters, checked against a list of common passwords, stored with argon2id. Sign-in answers the same for an unknown e-mail and a wrong password (and takes the same time).
- Failed sign-ins are limited to 10 per 15 minutes per address and per account (successful ones do not count, so a whole office behind one address can sign in). Password-reset and invitation links are single-use and expire (1 hour, 7 days).
- Sessions are `HttpOnly`, `SameSite=Lax` cookies (`Secure` behind https), every state-changing admin request needs the CSRF token, and blocking a person or a retailer ends their sessions and API tokens at once.
- Roles: brand administrators see the admin; retailers' staff see only the portal, only the brands their company was given, and only what those brands published.

## API access

- Retailers' software uses OAuth2 client credentials; secrets are shown once and stored as hashes, tokens are short-lived and bound to the client's current grant (revoking a key or a retailer revokes its tokens). Scopes: `catalog:read`, `media:read`, `media:original`, `usage:write`.
- Automatic-upload keys (`obxi_…`) have their own scopes (`ingest:read`, `ingest:write`, `ingest:delete`), can be restricted to brands and are stored as hashes.
- Image, export and download links are signed with the node key, carry the viewer and expire after one hour, so blocking a retailer stops its links on the next request. There are no permanent public URLs for content.

## Stored secrets

Import-source passwords, webhook secrets and pending e-mails with one-time links are encrypted with a key derived from `OBX_SECRET_KEY` (AES-GCM). Keep that key out of the repository and with your backups; rotating it is not supported yet (stored credentials would have to be entered again).

## Browser protections

Every response carries `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin`, `X-Frame-Options: DENY`, a `Permissions-Policy` and a `Content-Security-Policy`: for the web interface `default-src 'self'; script-src 'self'; … frame-ancestors 'none'` (no inline script), for API responses `default-src 'none'; frame-ancestors 'none'; sandbox`. Behind https (`OBX_PUBLIC_URL` starts with `https://`) the node adds `Strict-Transport-Security: max-age=31536000`. CORS (`Access-Control-Allow-Origin: *`) is sent only on the public discovery document `/.well-known/obx`.

## Outbound requests (SSRF)

Scheduled imports (web address, SFTP, S3) and webhooks go to addresses the brand or a retailer typed in. The node:

- allows only `https` (`sftp` for SFTP, and `http` only with `OBX_ALLOW_PRIVATE_URLS=true`);
- resolves the name and refuses loopback, private, CGNAT, link-local, multicast, reserved and documentation ranges in every spelling (IPv4-mapped IPv6, NAT64, `0.0.0.0`, `localhost`, numeric shorthands);
- checks the address in the dialer, i.e. the address actually connected to, so a name that changes its answer after the check (DNS rebinding) is still refused; redirects are checked again (webhooks follow none);
- **always** refuses link-local addresses (169.254.0.0/16 with the cloud metadata service 169.254.169.254, fe80::/10), the other cloud metadata addresses (100.100.100.200, fd00:ec2::254) and the metadata host names (`metadata.google.internal`, …), even with `OBX_ALLOW_PRIVATE_URLS=true`, which is meant only for local development against MinIO or a local SFTP server.

## Uploads and imports

Upload sizes are limited before anything is buffered; declared checksums are verified; file types are recognised by their content, SVG is never accepted; images are decoded with a 100-megapixel limit; zips are checked for path traversal, symlinks, entry count and total size and are never extracted to paths taken from entry names. Mirror imports refuse to unpublish more than half the catalog without an explicit override. CSV exports neutralise spreadsheet formulas.

## Operations

- The Docker image runs as an unprivileged user (uid 10001); the systemd unit uses `ProtectSystem=strict`, `NoNewPrivileges` and friends; base images are pinned by digest, Litestream by checksum.
- Logs never contain query strings, request bodies, passwords, tokens or client addresses ([operations.md](operations.md#logs)). The audit log records who changed what ([operations.md](operations.md#audit-log)).
- Dependencies are checked on every change: `govulncheck`, the licence check, Dependabot updates; CI runs the race detector and the conformance suite's security profile.
- `/metrics` is off unless `OBX_METRICS_TOKEN` is set.

## Known limits

- Rate limits are kept in memory: they reset on restart and are per instance (the portal runs as one instance).
- `OBX_TRUST_PROXY=true` takes the client address from `X-Forwarded-For`; set it only when every request comes through your proxy or platform (Caddy, Cloud Run, Fly.io, Railway, Render), otherwise a client could choose its own address for the rate limits.
- Renditions assume sRGB; embedded ICC profiles of originals are kept but not converted.
