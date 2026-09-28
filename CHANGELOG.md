# Changelog

All notable changes to the Portal for Google Cloud & Docker (`node-go`) are listed here. The project
follows [Semantic Versioning](https://semver.org/): the major version changes when an upgrade needs
more than replacing the software (see [docs/upgrading.md](docs/upgrading.md)); database migrations
are always forward-only and applied automatically on start.

## [Unreleased]

## [0.1.0] - 2026-09-28

First release. Implements Open Brand Exchange protocol 0.1 (read API, ingest API, node admin and
portal API) and passes `obx-conformance all` (58 of 58 tests, including the rate-limit test) on
SQLite and PostgreSQL.

### For brands
- A portal for retailers ("<Brand> · <portal word>" in 25 languages): request access, sign in,
  search and filter, product pages, paste EANs, a selection, downloads of one image, a product, a
  selection or everything as a zip (large zips prepared in the background), article data as CSV.
- Admin screens (vendored admin-ui 0.1.1): setup wizard, catalog, imports (CSV + images or one
  zip, dry run, mirror mode with a mass-unpublish guard), scheduled imports from a web address,
  SFTP or an S3 bucket, retailers and their API keys, access requests, invitations, automatic
  upload keys, webhooks, statistics, branding and languages.
- Images keep their original bytes (including Content Credentials); web, square and thumbnail
  versions are made by the portal itself (pure Go, no libraries to install).
- Images named `{style}_ALL_{position}.jpg` belong to every colour of the style.
- E-mails in the recipient's language over SMTP, retried with backoff when the mail server is
  unavailable.
- Audit log of admin actions and sign-ins (`GET /admin/api/audit-log`, `node-go admin audit-log`).
- Personal data: export per retailer, per person ("my data"), erasure of retailers and people,
  retention settings for download statistics, the audit log, rejected access requests, jobs and
  webhook logs; a privacy-notice template ([docs/privacy-notice-template.md](docs/privacy-notice-template.md)).

### For retailers' software
- OAuth2 client credentials, the `/changes` feed (snapshot and live), products, EAN lookup (GET and
  POST up to 2000), signed image links, bulk exports, webhooks, usage events.

### For automation
- The OBX Ingest API (idempotent PUT/PATCH/DELETE, blobs, resumable uploads, batch, imports) for
  `obx push`, the GitHub Action and n8n/Make/Zapier; `node-go import` and
  `node-go admin create-ingest-token` on the server.

### Hosting
- One Docker image (linux/amd64 and linux/arm64) for Docker Compose with Caddy (automatic HTTPS
  with only `DOMAIN` and `ADMIN_EMAIL`), Google Cloud Run (`deploy/cloudrun.sh`, one command),
  Fly.io, Railway and Render; a systemd installer for Linux servers; binaries for Linux, macOS
  and Windows.
- SQLite (default, continuous backup with Litestream in the image) or PostgreSQL; files on local
  disk, S3-compatible buckets (AWS S3, Cloudflare R2, MinIO) or Google Cloud Storage.

### Operations
- `/healthz` (liveness) and `/readyz` (readiness, 503 while shutting down), optional Prometheus
  `/metrics` behind a token, JSON logs with request ids (`X-Request-Id`), graceful shutdown that
  resumes interrupted jobs, all settings checked at start with one clear message per problem.
- `node-go backup` / `node-go restore` (consistent SQLite copy, files and key), a schema version
  check that refuses a database written by a newer release.
- Security headers (CSP, HSTS behind TLS, frame-ancestors, referrer policy), rate limits (failed
  logins counted per address and per account), SSRF protection that pins the checked address and
  never reaches link-local or cloud metadata addresses.
- Tested at 10 000 products, 60 000 variants and 40 000 images: portal pages, `/changes` pages and
  EAN lookups stay well below 100 ms at the median ([docs/performance.md](docs/performance.md)).

[Unreleased]: https://github.com/openbrandexchange/node-go/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/openbrandexchange/node-go/releases/tag/v0.1.0
