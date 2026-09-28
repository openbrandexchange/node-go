# Docker (any server, automatic HTTPS)

What you need: a server with Docker (any Linux VPS works; 1 CPU and 1 GB memory are plenty to start), ports 80 and 443 open, and a domain name such as `images.yourbrand.com`.

## 1. Point your domain at the server

At your DNS provider, add an `A` record (and `AAAA` for IPv6) for `images.yourbrand.com` with the server's IP address.

## 2. Start

```sh
git clone https://github.com/openbrandexchange/node-go && cd node-go
cp .env.example .env        # set OBX_DOMAIN, and OBX_SECRET_KEY (openssl rand -base64 48)
docker compose up -d
```

Caddy requests a Let's Encrypt certificate automatically. Then:

```sh
docker compose logs node | grep "setup code"
```

Open `https://images.yourbrand.com`, enter the setup code, your company name and your password.

## Updating

```sh
git pull && docker compose up -d --build
```

The database migrates itself on start. Your data lives in the Docker volume `obx-data`.

## Backups

Everything is in one volume: the SQLite database (`/data/obx.db`) and the image files (`/data/blobs`). Two good options:

- **Continuous (recommended):** set `LITESTREAM_REPLICA_URL=s3://your-bucket/portal-db` (plus `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` for Litestream) and keep images in the same bucket with `OBX_BLOB_URL=s3://your-bucket/portal-images?region=eu-west-1`. The database is then restored automatically on a new server.
- **Nightly copy:** `docker run --rm -v node-go_obx-data:/data -v $PWD:/backup alpine tar czf /backup/portal-$(date +%F).tgz /data` (stop the container first, or use Litestream, for a consistent database copy).

Keep `OBX_SECRET_KEY` with your backups: without it, stored passwords for scheduled imports and webhook secrets cannot be read.

## Without a domain (trying it out)

`docker compose -f docker-compose.local.yml up` serves on <http://localhost:8080>.

## Using PostgreSQL

Set `OBX_DATABASE_URL=postgres://user:password@host:5432/portal?sslmode=require` in `.env`. The portal creates its tables on start.

## E-mail

Invitations, access-request notices and "your download is ready" messages need an SMTP server: set `OBX_SMTP_URL=smtp://user:password@smtp.example.com:587` and `OBX_MAIL_FROM=portal@yourbrand.com`. Without it the portal still works: the admin screens show invitation links to copy.
