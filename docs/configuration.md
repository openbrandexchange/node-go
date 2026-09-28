# Configuration

All settings are environment variables. None is required to try the portal locally; for a public portal set at least `OBX_PUBLIC_URL` and `OBX_SECRET_KEY`. Every value is checked when the node starts: a wrong one stops it with a message naming the variable (all problems at once), and an unknown `OBX_…` variable is reported as a probable typo. `node-go check-config` runs the same check without starting. Everything a brand configures itself (company, logo, colours, languages, portal options, default image rights) is done in the admin, not here.

| Variable | Default | Meaning |
|---|---|---|
| `OBX_PUBLIC_URL` | `http://localhost:8080` (or the address Render, Railway or Fly.io announce) | The address people and software use, without a path. Links in e-mails and the discovery document are built from it; with `https://` cookies are `Secure` and responses carry HSTS. |
| `PORT` | `8080` | Port to listen on. |
| `OBX_SECRET_KEY` | generated once into `$OBX_DATA_DIR/secret.key` | At least 32 characters (`openssl rand -base64 48`). Encrypts stored import credentials and webhook secrets and signs download links. Back it up; never change it after data exists. |
| `OBX_SETUP_CODE` | generated and printed in the log | One-time code for the first-run setup wizard. |
| `OBX_DATA_DIR` | `./data` | Where the SQLite database, the generated secret key and (by default) images are stored. |
| `OBX_DATABASE_URL` | `sqlite:$OBX_DATA_DIR/obx.db` | `sqlite:/path/file.db` or `postgres://user:pass@host:5432/db?sslmode=require`. `DATABASE_URL` is used when this is not set. |
| `OBX_BLOB_URL` | `file://$OBX_DATA_DIR/blobs` | Where images and zips are stored: `file:///path`, `s3://bucket/prefix?region=eu-west-1`, `s3://bucket/prefix?endpoint=https://minio.example.com&path_style=true`, `r2://bucket/prefix?endpoint=https://<account>.r2.cloudflarestorage.com`, `gs://bucket/prefix` (Cloud Storage with HMAC keys). |
| `OBX_S3_ACCESS_KEY_ID`, `OBX_S3_SECRET_ACCESS_KEY` | the `AWS_*` variables or the instance role | Keys for an S3-compatible `OBX_BLOB_URL`. |
| `OBX_SMTP_URL` | not set: e-mails are only logged | `smtp://user:password@host:587` (STARTTLS) or `smtps://user:password@host:465`. |
| `OBX_MAIL_FROM` | `no-reply@<public host>` | Sender address. |
| `OBX_MAIL_LOG_BODIES` | `false` | Development only: write full e-mails (including one-time links) to the log. |
| `OBX_ALLOW_PRIVATE_URLS` | `false` | Development only: allow scheduled imports and webhooks to reach private and loopback addresses and plain `http`. Link-local addresses (cloud metadata) stay blocked. |
| `OBX_TRUST_PROXY` | `false` | Take the client address from `X-Forwarded-For` (set when behind Caddy, a load balancer, Cloud Run, Fly, Railway or Render) for rate limits. Only when every request comes through that proxy. |
| `OBX_WORKERS` | `2` | Background workers for imports, exports and zip downloads. |
| `OBX_LOG_FORMAT`, `OBX_LOG_LEVEL` | `text` (`json` in the Docker image), `info` | `json` for log collectors; `debug` also logs health-check requests. Paths only, never query strings, headers or client addresses ([operations.md](operations.md#logs)). |
| `OBX_ACCESS_LOG` | `true` | One `info` line per request with route, status, duration and request id; `false` logs only failures. |
| `OBX_METRICS_TOKEN` | not set | Serve Prometheus metrics at `/metrics` for scrapers that send `Authorization: Bearer <token>` (16+ characters). |
| `OBX_METRICS` | `false` (`true` when a token is set) | `true` without a token serves `/metrics` to anyone: only on a private network. |
| `OBX_SHUTDOWN_DELAY` | `0` | Seconds to keep serving after `SIGTERM` while `/readyz` already answers 503 (for load balancers). |
| `OBX_CHANGE_RETENTION_DAYS` | `400` (90–36500) | How long the change history for retailers' software is kept. |
| `OBX_JOB_RETENTION_DAYS` | `30` (8–36500) | How long finished imports, exports, prepared downloads and batch results are kept. |
| `OBX_WEBHOOK_LOG_RETENTION_DAYS` | `8` (1–365) | How long the webhook delivery log is kept. |
| `GOMEMLIMIT` | not set | Go runtime memory limit (for example `768MiB` in a 1 GB container); set by the Compose file and the Cloud Run script. |
| `OBX_CUSTOM_DOMAIN_HELP` | a generic text | Markdown shown in the admin next to the domain settings (hosting-specific instructions). |
| `LITESTREAM_REPLICA_URL` | not set | Docker image only: restore the SQLite database from this replica on start and replicate it continuously (`gcs://bucket/path`, `s3://bucket/path`, `file:///path`). Credentials: `LITESTREAM_ACCESS_KEY_ID`/`LITESTREAM_SECRET_ACCESS_KEY` (S3) or the platform's identity (Cloud Run). |
| `OBX_TEST_RATE_LIMIT` | not set | Conformance testing only: lower the per-minute API limit. |

Retention of personal data (download statistics, the audit log, access requests) is a setting of the brand in the admin API (`retention` in the settings), not an environment variable: see [operations.md](operations.md#personal-data-gdpr).

### Docker Compose (`docker-compose.yml`)

| Variable | Meaning |
|---|---|
| `DOMAIN` | required: the portal's domain, e.g. `images.yourbrand.com` (sets `OBX_PUBLIC_URL=https://$DOMAIN` and Caddy's certificate) |
| `ADMIN_EMAIL` | required: your address for Let's Encrypt (certificate expiry notices) |
| `HTTP_PORT`, `HTTPS_PORT` | `80`, `443`; change only when something else uses them (Let's Encrypt needs 80 or 443 reachable) |
| the `OBX_…` and `LITESTREAM_…` variables above | passed on to the node when set (see `.env.example`) |

## Limits

| | |
|---|---|
| Single upload (image, blob) | 100 MiB; larger originals use resumable uploads (up to 2 GiB) |
| Import upload | 2 GiB (zip contents up to 3 GiB, 100 000 entries) |
| Batch | 1000 operations |
| Images | JPEG, PNG, WebP, TIFF; up to 100 megapixels for rendering |
| Selection / quick zip | 1000 products / 400 images; larger downloads are prepared in the background and kept 7 days |
