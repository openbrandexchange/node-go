# Operations

Everything a brand (or whoever hosts the portal for them) needs to keep it running: health checks, logs, metrics, backups and restores, updates, limits and data retention. Settings are environment variables ([configuration.md](configuration.md)); hosting-specific steps are in [hosting/](hosting/).

## Health checks

| Endpoint | Answers | Use it for |
|---|---|---|
| `GET /healthz` | `200 ok` while the process runs and the database connection works; `503` otherwise | liveness: restart the container when it fails repeatedly |
| `GET /readyz` | `200 {"status":"ready","version":…,"schema_version":…,"checks":{…}}` when the database answers on the current schema and the file store is reachable; `503 {"status":"not_ready"}` otherwise, and from the moment the node is shutting down | readiness: send traffic only while it answers 200 |

`node-go healthcheck` calls `/healthz` on the local port (the Docker `HEALTHCHECK` uses it). The Cloud Run script configures `/readyz` as startup probe and `/healthz` as liveness probe; Fly.io, Railway and Render use `/healthz`; Caddy in `docker-compose.yml` checks `/readyz`.

## Logs

The node logs to standard error. `OBX_LOG_FORMAT=json` (the default in the Docker image) writes one JSON object per line for log collectors (Cloud Logging, Loki, Datadog, `docker logs`).

- **One line per request** (`"msg":"http"`) with `method`, `route` (the matched pattern, e.g. `/obx/v1/products/{id}`), `path`, `status`, `bytes`, `duration_ms` and `request_id`. Probes (`/healthz`, `/readyz`, `/metrics`) are logged only at `OBX_LOG_LEVEL=debug`. `OBX_ACCESS_LOG=false` logs only failures (status 500 and up).
- **Request ids.** Every response carries `X-Request-Id`. The node keeps an id sent by a proxy or load balancer (8–128 characters of `A-Z a-z 0-9 . _ : / = + -`), otherwise it makes one. Every log line written while handling that request carries the same `request_id`, as does the audit log entry, so one id finds everything about one request.
- **Never logged:** query strings (they hold link signatures), request bodies, passwords, tokens, cookies, client IP addresses (personal data). E-mail bodies with one-time links appear only with `OBX_MAIL_LOG_BODIES=true`, for local development.
- **Levels:** `info` (default) for requests and events, `warn` for things to look at (a failed import, an e-mail that will be retried), `error` for failures that need a person (an e-mail given up after all retries, a request that answered 500, a panic with its stack).
- **Retention** is up to the host: the Docker Compose file keeps 5 × 20 MB per container; Cloud Logging keeps 30 days by default; journald follows its own settings.

## Metrics

Set `OBX_METRICS_TOKEN` (16+ characters) to serve Prometheus metrics at `/metrics`; scrapers send `Authorization: Bearer <token>`. (`OBX_METRICS=true` without a token serves them to anyone who can reach the node: use that only on a private network.)

| Metric | Type | Meaning |
|---|---|---|
| `obx_http_requests_total{method,route,code}` | counter | requests per route pattern and status |
| `obx_http_request_duration_seconds{route}` | histogram | latency per route pattern |
| `obx_jobs{kind,status}` | gauge | imports, exports, downloads and batches that are queued, running or failed |
| `obx_webhook_deliveries_pending` | gauge | webhook deliveries waiting (to be retried) |
| `obx_mail_outbox_pending` | gauge | e-mails waiting for a retry |
| `obx_renditions_failed` | gauge | originals the portal could not make web versions of |
| `obx_catalog_visible{type}` | gauge | published brands, products, variants and images |
| `obx_changes_latest_seq` | gauge | position of the change feed |
| `obx_build_info{version,database,go}`, `obx_uptime_seconds`, `go_goroutines`, `go_memstats_*` | gauge | the running build and process |

Suggested alerts: `/readyz` failing for 5 minutes; `rate(obx_http_requests_total{code=~"5.."}[5m]) > 0.1`; `obx_mail_outbox_pending > 0` for an hour (the mail server rejects mail); `obx_jobs{status="failed"}` increasing; `obx_renditions_failed > 0`.

## Backups

What must be backed up, whatever the hosting:

1. **The database**: SQLite `obx.db` in `OBX_DATA_DIR`, or your PostgreSQL database.
2. **The files** (image originals and their web versions): `OBX_DATA_DIR/blobs`, or the bucket in `OBX_BLOB_URL`.
3. **The secret key**: `OBX_SECRET_KEY`, or `OBX_DATA_DIR/secret.key` when it is not set. Without it, stored import passwords and webhook secrets cannot be read and signed links change. Keep it apart from the other backups (a password manager is fine).

### Continuous: Litestream (SQLite)

The Docker image contains [Litestream](https://litestream.io). With `LITESTREAM_REPLICA_URL` set, every change to the database is streamed to a bucket within about a second, and a container that starts with an empty disk restores the database from there first. The Cloud Run script sets this up for you (`gcs://<bucket>/database`); for Docker Compose, Fly.io and others set:

```sh
LITESTREAM_REPLICA_URL=s3://your-bucket/portal-db      # or gcs://…, abs://…, file:///path
LITESTREAM_ACCESS_KEY_ID=…  LITESTREAM_SECRET_ACCESS_KEY=…   # for S3-compatible buckets
```

Keep the images in a bucket as well (`OBX_BLOB_URL=s3://your-bucket/portal-images?region=…`), or back up the `blobs` folder: Litestream covers the database only. Run **one** container per replica: two writers would overwrite each other's replica.

To restore to a point in time (for example just before a mistaken mass delete), stop the portal and run in a container with the same settings:

```sh
litestream restore -timestamp 2026-10-01T09:00:00Z -o /data/obx.db "$LITESTREAM_REPLICA_URL"
```

then start the portal again.

### Snapshots: `node-go backup`

`node-go backup --to DIR` writes a consistent copy of everything the node stores locally into a new directory, **while the node keeps running**: the SQLite database as a single file (`VACUUM INTO`), the files (when they are on local disk) and the secret key (when it is not set in the environment), with a `manifest.json`. Files in a bucket and a PostgreSQL database are not copied; the manifest says so.

```sh
# Docker Compose
docker compose exec node node-go backup --to /data/backups/$(date +%F)
docker compose cp node:/data/backups/$(date +%F) ./portal-backup-$(date +%F)
# Linux server (systemd)
sudo -u obx env $(sudo cat /etc/node-go/node-go.env | xargs) node-go backup --to /var/backups/node-go/$(date +%F)
```

Copy the directory off the server (object storage, another machine) and keep several generations. A nightly cron job is enough for most brands; with Litestream on top you lose at most a second.

### PostgreSQL

Back up with your provider's backups or `pg_dump --format=custom "$OBX_DATABASE_URL" > portal.dump`; restore with `pg_restore --clean --no-owner -d "$OBX_DATABASE_URL" portal.dump`. Files and the secret key are backed up as above.

### Restoring

With the portal stopped:

```sh
node-go restore --from ./portal-backup-2026-10-01          # refuses to overwrite existing data
node-go restore --from ./portal-backup-2026-10-01 --force  # replaces the database
node-go serve
```

Restore uses the same `OBX_DATA_DIR`, `OBX_DATABASE_URL` and `OBX_BLOB_URL` as the portal. It checks that the backup is not from a newer release, and that `OBX_SECRET_KEY` (when set) is the key the backup was made with; it restores `secret.key` into the data directory otherwise. For Docker, run it in a one-off container on the portal's volume: `docker compose run --rm node restore --from /data/backups/2026-10-01 --force`.

**Tested:** `scripts/test-docker.sh` (run in CI) sets up a portal in the Docker image, imports the example catalog, (1) streams the database with Litestream, throws the container and its disk away and starts a new one that restores from the replica, and (2) takes a `node-go backup` from the running node and restores it into a new, empty volume; after both, it logs in again and checks the catalog, an image download, the EAN lookup and the discovery document. `TestBackupAndRestore` does the same in the Go tests. Practise a restore yourself once after setting up a portal, and after every major upgrade.

## Updating

See [upgrading.md](upgrading.md). In short: take a backup, replace the image or binary, start it. Migrations run automatically on start, only forward; a database that a newer release already migrated is refused by an older one (restore the backup instead of downgrading).

## Stopping and restarting

On `SIGTERM` (Docker, Cloud Run, systemd) the node stops accepting new connections, reports not ready on `/readyz`, lets running requests finish for up to 25 seconds and waits for background jobs to finish their current step. `OBX_SHUTDOWN_DELAY=<seconds>` first keeps serving for that long while `/readyz` already answers 503, so a load balancer can take the node out of rotation (the Compose file uses 2 s). Jobs that were interrupted (an import, a zip being prepared) are resumed after the restart: every job kind can safely run again.

## Resources

| Catalog | Memory | CPU | Disk |
|---|---|---|---|
| up to 10 000 products | 512 MB–1 GB | 1 | database ≈ 30 KB per product (≈ 300 MB for 10 000 products with their change history) + your images and their web versions (≈ 1.3 × the originals) |
| larger | 2 GB+ | 2 | consider PostgreSQL and a bucket |

- Zip downloads stream from storage and use little memory; uploads are buffered on disk (`/tmp`) before they are stored. On Cloud Run the disk *is* memory, so the script gives the instance 2 GiB (`MEMORY=4Gi` for large catalogs).
- `GOMEMLIMIT` keeps the Go heap below a limit; the Compose file sets 768 MiB for its 1 GB container, the Cloud Run script half of the instance memory. The systemd unit caps memory at 75 % of the machine (`MemoryMax`).
- Image rendering is limited to 100 megapixels per image and two background workers (`OBX_WORKERS`).

## Timeouts, retries and limits

| What | Behaviour |
|---|---|
| Incoming requests | headers within 20 s, idle connections closed after 120 s; bodies up to the documented sizes ([configuration.md](configuration.md#limits)) |
| Outbound web requests (scheduled imports, webhooks, SFTP and S3 sources) | connect 15 s, TLS 15 s, first byte 60 s, total bounded per use; private, loopback, link-local and cloud-metadata addresses refused (the checked address is the one connected to) |
| SMTP | connect 20 s, whole conversation 60 s; failed e-mails are retried after 1, 5 and 15 minutes, then 1, 3, 6 and 12 hours, then given up with an `error` log line |
| Webhooks | signed, no redirects followed, retried with exponential backoff (30 s doubling, at most 6 h apart); a subscription that fails for 72 hours is disabled |
| Background jobs | leased for 10 minutes and renewed while they progress; a job interrupted three times is marked failed |
| Rate limits | per client, per user and per address; failed logins: 10 per 15 minutes per address and per account. Limits are kept in memory (one instance). |

## Audit log

Every change made in the admin (creating, changing and deleting retailers, people, keys, settings, catalog records, imports, sources, webhooks, approvals), every sign-in and failed sign-in, the setup and personal-data exports are recorded with time, who, what, the target, the outcome and the request id. Request bodies, passwords and secrets are never recorded.

- Admins read it with `GET /admin/api/audit-log?limit=50&cursor=…` (newest first; filters `action=`, `actor_id=`, `target_id=`, `outcome=succeeded|failed`) or `node-go admin audit-log --limit 100` on the server. Each entry has `at`, `actor` (`type`, `id`, `name`, `role`, `email`), `action` (for example `retailers.update`, `session.login`), `method`, `route`, `target_id`, `status`, `succeeded` and `request_id`; the shape is the same in every OBX portal. Command-line actions (`reset-password`, `create-ingest-token`, `personal-data`) are recorded as actor `cli`.
- Kept as long as the brand's retention setting `retention.audit_log_days` (default 365).

## Personal data (GDPR)

The portal stores personal data of retailers' staff (name, e-mail, language, sign-ins), of people who request access, and download statistics per person. The brand is the controller; see the [privacy-notice template](privacy-notice-template.md).

- **Access and portability:** a person downloads everything stored about them with "Download my data" (`GET /admin/api/me/export`); an admin exports a person with `GET /admin/api/users/{id}/export` and a whole retailer company with `GET /admin/api/retailers/{id}/export`, or on the server `node-go admin personal-data --email person@shop.example` / `--retailer rtl_…`. The document (`"format": "obx-personal-data-export/1"`) names the brand as controller and lists the retention settings.
- **Erasure:** a retailer's person deletes their own account (`DELETE /admin/api/me`); an admin deletes a person or a retailer. Deleting a person removes the account, sessions, prepared downloads and pending e-mails, and removes the name from the statistics and from the audit log. Deleting a retailer does that for all its people and also removes its API keys, webhooks and access requests. Statistics keep only the company name.
- **Retention** is a brand setting (admin API `PATCH /admin/api/settings` → `retention`, 30–3650 days): download statistics `download_events_days` (default 730; the person's name is removed after at most 400 days), audit log `audit_log_days` (365), decided access requests `access_requests_days` (365). Operational records follow environment variables: finished jobs and batch results `OBX_JOB_RETENTION_DAYS` (30), webhook delivery log `OBX_WEBHOOK_LOG_RETENTION_DAYS` (8); sent or failed e-mails are kept 7 days (the text itself is deleted when sent). Expired sessions are removed within minutes; invitation and reset links stop working when they expire (7 days and 1 hour).
- No IP addresses are stored or logged; the rate limiter keeps them in memory for at most two hours.

## Troubleshooting

| Symptom | Look at |
|---|---|
| The node does not start | the first log lines: every configuration problem is listed by variable name; `node-go check-config` prints the same without starting |
| "the database has schema version N but this node-go knows only up to M" | a newer release migrated the database; run that release, or restore the backup made before the upgrade |
| Setup code lost | `docker compose logs node | grep "setup code"`; or set `OBX_SETUP_CODE` and restart (only before setup is completed) |
| Admin password lost | `node-go admin reset-password --email you@yourbrand.com` prints a one-time link |
| E-mails do not arrive | `obx_mail_outbox_pending`, `warn` lines "e-mail could not be sent"; invitation links can always be copied in the admin |
| Images without web versions | `obx_renditions_failed`; the admin shows the image with its error (unsupported or damaged file) |
