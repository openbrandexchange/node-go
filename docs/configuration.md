# Configuration

All settings are environment variables. None is required to try the portal locally; for a public portal set at least `OBX_PUBLIC_URL` and `OBX_SECRET_KEY`. Everything a brand configures itself (company, logo, colours, languages, portal options, default image rights) is done in the admin, not here.

| Variable | Default | Meaning |
|---|---|---|
| `OBX_PUBLIC_URL` | `http://localhost:8080` (or the address Render, Railway or Fly.io announce) | The address people and software use. Links in e-mails and the discovery document are built from it. |
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
| `OBX_TRUST_PROXY` | `false` | Take the client address from `X-Forwarded-For` (set when behind Caddy, a load balancer, Cloud Run, Fly, Railway or Render) for rate limits. |
| `OBX_WORKERS` | `2` | Background workers for imports, exports and zip downloads. |
| `OBX_LOG_FORMAT`, `OBX_LOG_LEVEL` | `text`, `info` | `json` for log collectors; `debug` logs every request (paths only, never query strings or headers). |
| `OBX_CHANGE_RETENTION_DAYS` | `400` (minimum 90) | How long the change history for retailers' software is kept. |
| `OBX_CUSTOM_DOMAIN_HELP` | a generic text | Markdown shown in the admin next to the domain settings (hosting-specific instructions). |
| `LITESTREAM_REPLICA_URL` | not set | Docker image only: restore the SQLite database from this replica on start and replicate it continuously (`gcs://bucket/path`, `s3://bucket/path`, `file:///path`). |
| `OBX_TEST_RATE_LIMIT` | not set | Conformance testing only: lower the per-minute API limit. |

## Limits

| | |
|---|---|
| Single upload (image, blob) | 100 MiB; larger originals use resumable uploads (up to 2 GiB) |
| Import upload | 2 GiB (zip contents up to 3 GiB, 100 000 entries) |
| Batch | 1000 operations |
| Images | JPEG, PNG, WebP, TIFF; up to 100 megapixels for rendering |
| Selection / quick zip | 1000 products / 400 images; larger downloads are prepared in the background and kept 7 days |
