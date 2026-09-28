# Docker (any server, automatic HTTPS)

What you need: a server with Docker and the Compose plugin (any Linux VPS works; 1 CPU and 1 GB memory are plenty to start), ports 80 and 443 reachable from the internet, and a domain name such as `images.yourbrand.com`.

## 1. Point your domain at the server

At your DNS provider, add an `A` record (and `AAAA` for IPv6) for `images.yourbrand.com` with the server's IP address. Check with `dig +short images.yourbrand.com` that it shows the server's address before you continue: the certificate can only be issued once it does.

## 2. Start

```sh
git clone https://github.com/openbrandexchange/node-go && cd node-go
DOMAIN=images.yourbrand.com ADMIN_EMAIL=you@yourbrand.com docker compose up -d
```

Those two settings are all it needs. Caddy obtains a Let's Encrypt certificate for the domain (`ADMIN_EMAIL` receives expiry notices, which you should never get because Caddy renews automatically) and redirects http to https. To keep the settings for later commands, put them in a `.env` file instead (`cp .env.example .env`); it also has the optional ones (e-mail, backups, metrics).

Then get the setup code:

```sh
docker compose logs node | grep "setup code"
```

Open `https://images.yourbrand.com`, enter the setup code, your company name and your password.

The node's secret key is generated into the data volume on first start. For a portal you keep, set it yourself so it lives with your other secrets: `OBX_SECRET_KEY=$(openssl rand -base64 48)` in `.env` **before the first start** (it can not be changed later).

## What runs

| Container | Does |
|---|---|
| `node` | the portal (image built from this folder); data in the volume `obx-data` (`/data`); limited to 1 GB memory and 2 CPUs; JSON logs rotated at 5 × 20 MB |
| `caddy` | HTTPS on 80/443 (HTTP/3 too), certificates in the volume `caddy-data`, forwards to the node, health checks `/readyz` |

`scripts/test-compose-https.sh` (run in CI) starts exactly this file with only `DOMAIN` and `ADMIN_EMAIL` set (with `DOMAIN=localhost`, for which Caddy uses its own local certificate authority instead of Let's Encrypt), then checks HTTPS, the security headers, the redirect from http, a complete setup and import through Caddy, and that a restart keeps the data.

## Updating

```sh
git pull && docker compose up -d --build
```

The database migrates itself on start. See [upgrading.md](../upgrading.md).

## Backups

Everything is in one volume: the SQLite database (`/data/obx.db`), the image files (`/data/blobs`) and the generated key (`/data/secret.key`).

- **Snapshot (simple):** `docker compose exec node node-go backup --to /data/backups/$(date +%F)` makes a consistent copy while the portal runs; copy it off the server with `docker compose cp node:/data/backups/$(date +%F) ./`. Put both lines in a nightly cron job.
- **Continuous:** set `LITESTREAM_REPLICA_URL=s3://your-bucket/portal-db` with `LITESTREAM_ACCESS_KEY_ID`/`LITESTREAM_SECRET_ACCESS_KEY` in `.env`, and keep the images in a bucket too (`OBX_BLOB_URL=s3://your-bucket/portal-images?region=eu-west-1` with `OBX_S3_ACCESS_KEY_ID`/`OBX_S3_SECRET_ACCESS_KEY`). The database is then restored automatically on a new server.

Restoring and testing a restore: [operations.md](../operations.md#backups).

## Without a domain (trying it out)

`docker compose -f docker-compose.local.yml up` serves on <http://localhost:8080> and writes e-mails to the output.

## Other ports or another proxy

`HTTP_PORT` and `HTTPS_PORT` change the published ports (Let's Encrypt still needs 80 or 443 to reach Caddy). Behind your own reverse proxy, run only the node (`docker compose up -d node`, publish port 8080), proxy to it with request bodies up to 2 GB and long timeouts, and set `OBX_PUBLIC_URL` and `OBX_TRUST_PROXY=true`.

## Using PostgreSQL

Set `OBX_DATABASE_URL=postgres://user:password@host:5432/portal?sslmode=require` in `.env`. The portal creates its tables on start.

## E-mail

Invitations, access-request notices and "your download is ready" messages need an SMTP server: set `OBX_SMTP_URL=smtp://user:password@smtp.example.com:587` and `OBX_MAIL_FROM=portal@yourbrand.com`. Without it the portal still works: the admin screens show invitation links to copy. Failed e-mails are retried for about a day.
