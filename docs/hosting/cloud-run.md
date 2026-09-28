# Google Cloud Run

One command deploys the portal to Cloud Run, with the images in Cloud Storage and the database continuously backed up to the same bucket.

## Before you start (once)

1. A Google Cloud project **with billing enabled** in which you are **Owner** (create one at <https://console.cloud.google.com/projectcreate>, then link a billing account under *Billing*).
2. The Google Cloud CLI, logged in. Either open **Cloud Shell** in the console (everything is installed and logged in already), or on your computer:

```sh
# https://cloud.google.com/sdk/docs/install, then:
gcloud auth login
gcloud config set project YOUR-PROJECT-ID
```

## Deploy

```sh
git clone https://github.com/openbrandexchange/node-go && cd node-go
./deploy/cloudrun.sh
```

That is the whole deployment. It takes a few minutes the first time (Cloud Build builds the image) and ends with:

```
Your brand portal runs at: https://brand-portal-123456789012.europe-west4.run.app
First-run setup code:      K7QX-9M2P-VT4C
```

Open the address, enter the setup code, your company name and your password. Settings are environment variables in front of the command, for example `REGION=europe-west1 SERVICE=kestrel-portal ./deploy/cloudrun.sh`:

| Variable | Default | Meaning |
|---|---|---|
| `PROJECT` | `gcloud config get-value project` | project id |
| `REGION` | `europe-west4` (Netherlands) | [Cloud Run region](https://cloud.google.com/run/docs/locations) |
| `SERVICE` | `brand-portal` | service name (lowercase letters, digits, dashes) |
| `BUCKET` | `<project>-<service>` | bucket for images and the database backup |
| `DOMAIN` | not set | your own domain, see below |
| `MEMORY` | `2Gi` | instance memory; the SQLite database and uploads live in it (`4Gi` for large catalogs) |
| `SMTP_URL`, `MAIL_FROM` | not set | e-mail: `smtp://user:password@smtp.example.com:587` (stored in Secret Manager) and the sender address |
| `DATABASE_URL`, `CLOUDSQL_INSTANCE` | not set | PostgreSQL (Cloud SQL) instead of SQLite, see below |
| `ALWAYS_ON` | `1` | `0` scales to zero when nobody uses the portal |

Run the script again at any time, with the same settings, to update to the current code: it keeps your data, key and setup code.

## What the script does, step by step

These are the exact commands it runs (with the defaults), so you can also do it by hand or review it first:

```sh
PROJECT=$(gcloud config get-value project); REGION=europe-west4; SERVICE=brand-portal; BUCKET=$PROJECT-$SERVICE
NUMBER=$(gcloud projects describe $PROJECT --format 'value(projectNumber)')
# 1. APIs
gcloud services enable run.googleapis.com cloudbuild.googleapis.com artifactregistry.googleapis.com \
  secretmanager.googleapis.com storage.googleapis.com iam.googleapis.com --project $PROJECT
# 2. A private bucket for images (images/) and the database backup (database/)
gcloud storage buckets create gs://$BUCKET --project $PROJECT --location $REGION --uniform-bucket-level-access --public-access-prevention
# 3. A runtime account (only this bucket and these secrets) and a build account
gcloud iam service-accounts create $SERVICE-runner --project $PROJECT
gcloud iam service-accounts create $SERVICE-builder --project $PROJECT
gcloud storage buckets add-iam-policy-binding gs://$BUCKET --member serviceAccount:$SERVICE-runner@$PROJECT.iam.gserviceaccount.com --role roles/storage.objectAdmin
gcloud projects add-iam-policy-binding $PROJECT --member serviceAccount:$SERVICE-builder@$PROJECT.iam.gserviceaccount.com --role roles/run.builder --condition None
# 4. Secrets: the node key (never changes) and the first-run setup code
head -c 48 /dev/urandom | base64 | tr -d '\n' | gcloud secrets create $SERVICE-secret-key --project $PROJECT --replication-policy automatic --data-file=-
printf 'K7QX-9M2P-VT4C' | gcloud secrets create $SERVICE-setup-code --project $PROJECT --replication-policy automatic --data-file=-
gcloud secrets add-iam-policy-binding $SERVICE-secret-key --project $PROJECT --member serviceAccount:$SERVICE-runner@$PROJECT.iam.gserviceaccount.com --role roles/secretmanager.secretAccessor
gcloud secrets add-iam-policy-binding $SERVICE-setup-code --project $PROJECT --member serviceAccount:$SERVICE-runner@$PROJECT.iam.gserviceaccount.com --role roles/secretmanager.secretAccessor
# 5. Build this folder and deploy it: one instance, always on, probes on /readyz and /healthz
gcloud run deploy $SERVICE --project $PROJECT --region $REGION --source . --quiet \
  --build-service-account projects/$PROJECT/serviceAccounts/$SERVICE-builder@$PROJECT.iam.gserviceaccount.com \
  --service-account $SERVICE-runner@$PROJECT.iam.gserviceaccount.com --execution-environment gen2 --max-instances 1 \
  --cpu 1 --memory 2Gi --concurrency 80 --timeout 3600 --port 8080 --allow-unauthenticated \
  --startup-probe httpGet.path=/readyz,httpGet.port=8080,periodSeconds=5,timeoutSeconds=5,failureThreshold=60 \
  --liveness-probe httpGet.path=/healthz,httpGet.port=8080,periodSeconds=30,timeoutSeconds=5,failureThreshold=3 \
  --add-volume name=storage,type=cloud-storage,bucket=$BUCKET --add-volume-mount volume=storage,mount-path=/blobs \
  --set-env-vars OBX_PUBLIC_URL=https://$SERVICE-$NUMBER.$REGION.run.app,OBX_DATA_DIR=/data,OBX_BLOB_URL=file:///blobs/images,OBX_TRUST_PROXY=true,OBX_LOG_FORMAT=json,GOMEMLIMIT=1024MiB,LITESTREAM_REPLICA_URL=gcs://$BUCKET/database \
  --set-secrets OBX_SECRET_KEY=$SERVICE-secret-key:latest,OBX_SETUP_CODE=$SERVICE-setup-code:latest \
  --min-instances 1 --no-cpu-throttling
# 6. The setup code
gcloud secrets versions access latest --secret $SERVICE-setup-code --project $PROJECT
```

`scripts/test-cloudrun.sh` (run in CI) runs the script against a stand-in `gcloud` and checks every command and flag it uses against the help text of the real Google Cloud CLI.

| Piece | Why |
|---|---|
| Cloud Storage bucket | Images are stored under `images/`, mounted into the container with Cloud Storage FUSE (no keys needed); the database backup under `database/`. Private: no public access. |
| SQLite + [Litestream](https://litestream.io) | The database lives in the instance and is streamed to the bucket within a second; a new instance restores it first (the startup probe waits for that). This is why the service runs with **max-instances=1**: one writer. |
| Secret Manager | `OBX_SECRET_KEY` encrypts stored credentials and signs download links; the setup code protects the fresh portal. |
| Two service accounts | The runtime account may only use that bucket and those secrets; the build account only builds. |
| Probes | `/readyz` as startup probe (traffic only once the database is restored and the files are reachable), `/healthz` as liveness probe. |

## Costs

Always on (`ALWAYS_ON=1`, the default) means one instance with 1 vCPU and 2 GiB running all month, plus a little storage and egress: roughly **USD 45–65 per month** in europe-west4 at 2026 list prices, less with committed-use discounts. `ALWAYS_ON=0` scales to zero when nobody uses the portal (a few dollars a month for a small portal), but then scheduled imports, webhooks and background zips wait until the next visit, and the first visit after a pause takes a few seconds while the database is restored. Check <https://cloud.google.com/run/pricing> for current prices.

## Your own domain

```sh
DOMAIN=images.yourbrand.com ./deploy/cloudrun.sh
gcloud beta run domain-mappings create --service brand-portal --domain images.yourbrand.com --region europe-west4
```

Verify the domain when asked (`gcloud domains verify yourbrand.com`), add the DNS records the second command prints, and wait for the certificate (up to an hour). Domain mappings exist in [a subset of regions](https://cloud.google.com/run/docs/mapping-custom-domains) (europe-west1 and europe-west4 among them); elsewhere put a Google Cloud load balancer in front and use the same `DOMAIN`.

## Cloud SQL (PostgreSQL) instead of SQLite

For a very large catalog, zero-downtime updates or when you prefer a managed database:

```sh
DATABASE_URL='postgres://portal:PASSWORD@/portal?host=/cloudsql/PROJECT:REGION:INSTANCE' \
CLOUDSQL_INSTANCE=PROJECT:REGION:INSTANCE ./deploy/cloudrun.sh
```

The URL is stored in Secret Manager; give the runtime account the *Cloud SQL Client* role. With PostgreSQL Litestream is not used and several instances are possible (raise `--max-instances` by hand; rate limits are then per instance).

## Updates, backups, logs

- **Updates:** `git pull && ./deploy/cloudrun.sh` (same settings). During the switch-over the old and the new instance run for a few seconds; with SQLite, changes made on the old one in those seconds can be lost, so update when the portal is quiet ([upgrading.md](../upgrading.md)).
- **Backups:** the database is continuously in `gs://BUCKET/database` (Litestream keeps its history; point-in-time restore in [operations.md](../operations.md#continuous-litestream-sqlite)), the images in `gs://BUCKET/images`. Turn on [object versioning](https://cloud.google.com/storage/docs/object-versioning) and/or a bucket-to-bucket copy for extra safety: `gcloud storage buckets update gs://BUCKET --versioning`. Keep the secret `brand-portal-secret-key`.
- **Logs:** `gcloud run services logs read brand-portal --region europe-west4`, or Logs Explorer (JSON, with request ids).

## Limits to know

- Cloud Run's container disk is memory: uploads are buffered in `/tmp` before they are stored, and the SQLite database lives there too. For very large packages (several GB) use a scheduled import from the bucket or SFTP instead of uploading through the browser.
- The request timeout is 60 minutes, for large zip downloads.
