# Google Cloud Run

One command deploys the portal to Cloud Run with images in Cloud Storage and a continuously backed-up database:

```sh
git clone https://github.com/openbrandexchange/node-go && cd node-go
gcloud auth login && gcloud config set project YOUR-PROJECT
./deploy/cloudrun.sh
```

At the end it prints the address and the **setup code**. Open the address and complete the setup wizard.

## What the script creates

| Piece | Why |
|---|---|
| Cloud Storage bucket `<project>-brand-portal` | Images are stored under `images/` (mounted into the container with Cloud Storage FUSE); the database backup under `database/`. |
| SQLite + [Litestream](https://litestream.io) | The database lives in the container and is streamed to the bucket every second; on a new start it is restored first. This is why the service runs with **max-instances=1** (one writer). |
| Secret Manager: `brand-portal-secret-key`, `brand-portal-setup-code` | `OBX_SECRET_KEY` encrypts stored credentials and signs download links; the setup code protects the fresh portal. |
| Service account `brand-portal-runner` | May only use that bucket and those secrets. |
| Cloud Run service `brand-portal` | Built from this folder by Cloud Build (`gcloud run deploy --source`). |

Run the script again at any time to update to the current code; it keeps your data, key and setup code.

Options (environment variables): `REGION` (default `europe-west4`), `SERVICE`, `BUCKET`, `DOMAIN`, `ALWAYS_ON`.

## Always on, or scale to zero?

By default one instance keeps running with CPU always allocated (`ALWAYS_ON=1`), so scheduled imports, webhooks to retailers and large background downloads run on time. `ALWAYS_ON=0 ./deploy/cloudrun.sh` scales to zero when nobody uses the portal (cheaper; the first visit after a pause takes a few seconds, and scheduled imports wait until the next visit).

## Your own domain

```sh
DOMAIN=images.yourbrand.com ./deploy/cloudrun.sh
gcloud beta run domain-mappings create --service brand-portal --domain images.yourbrand.com --region europe-west4
```

Add the DNS records the second command prints. (Or put a load balancer in front; then set the same `DOMAIN`.)

## Cloud SQL (PostgreSQL) instead of SQLite

For a bigger catalog or when you prefer a managed database:

```sh
DATABASE_URL='postgres://portal:PASSWORD@/portal?host=/cloudsql/PROJECT:REGION:INSTANCE' \
CLOUDSQL_INSTANCE=PROJECT:REGION:INSTANCE ./deploy/cloudrun.sh
```

The URL is stored in Secret Manager. With PostgreSQL Litestream is not used.

## Limits to know

- Cloud Run's container disk is memory: uploads are buffered in `/tmp` before they are stored, so the service gets 2 GiB of memory. For very large packages (several GB) use a scheduled import from the bucket or SFTP instead of uploading through the browser.
- The request timeout is set to 60 minutes for large zip downloads.
