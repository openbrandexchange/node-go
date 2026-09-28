#!/usr/bin/env bash
# Deploy the brand portal to Google Cloud Run in one command:
#
#   ./deploy/cloudrun.sh
#
# What it sets up (idempotent, safe to run again to update):
#   - a Cloud Storage bucket for images (mounted into the container) and for
#     the continuous SQLite backup (Litestream);
#   - Secret Manager secrets for OBX_SECRET_KEY and the first-run setup code;
#   - a service account that may use exactly that bucket and those secrets;
#   - the Cloud Run service itself (built from this folder by Cloud Build),
#     with one instance: SQLite needs a single writer.
#
# Settings (environment variables, all optional):
#   PROJECT   Google Cloud project   (default: gcloud config project)
#   REGION    region                 (default: europe-west4)
#   SERVICE   service name           (default: brand-portal)
#   BUCKET    bucket name            (default: <project>-<service>)
#   DOMAIN    custom domain, e.g. images.yourbrand.com (map it afterwards,
#             see docs/hosting/cloud-run.md)
#   DATABASE_URL  a Postgres URL (e.g. Cloud SQL) instead of SQLite+Litestream
#   CLOUDSQL_INSTANCE  project:region:instance to attach when using Cloud SQL
#   ALWAYS_ON  1 (default) keeps one instance running so scheduled imports,
#             webhooks and background downloads run on time; 0 scales to zero.
set -euo pipefail

command -v gcloud >/dev/null || { echo "Install the Google Cloud CLI first: https://cloud.google.com/sdk/docs/install" >&2; exit 1; }

PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null)}"
[ -n "$PROJECT" ] || { echo "Set PROJECT or run: gcloud config set project <id>" >&2; exit 1; }
REGION="${REGION:-europe-west4}"
SERVICE="${SERVICE:-brand-portal}"
BUCKET="${BUCKET:-${PROJECT}-${SERVICE}}"
ALWAYS_ON="${ALWAYS_ON:-1}"
SA="${SERVICE}-runner@${PROJECT}.iam.gserviceaccount.com"
HERE="$(cd "$(dirname "$0")/.." && pwd)"

echo "Deploying ${SERVICE} to project ${PROJECT} in ${REGION}"
gcloud services enable run.googleapis.com cloudbuild.googleapis.com artifactregistry.googleapis.com \
  secretmanager.googleapis.com storage.googleapis.com iam.googleapis.com --project "$PROJECT"

if ! gcloud storage buckets describe "gs://${BUCKET}" --project "$PROJECT" >/dev/null 2>&1; then
  gcloud storage buckets create "gs://${BUCKET}" --project "$PROJECT" --location "$REGION" --uniform-bucket-level-access
fi

if ! gcloud iam service-accounts describe "$SA" --project "$PROJECT" >/dev/null 2>&1; then
  gcloud iam service-accounts create "${SERVICE}-runner" --project "$PROJECT" --display-name "Brand portal (${SERVICE})"
fi
gcloud storage buckets add-iam-policy-binding "gs://${BUCKET}" --member "serviceAccount:${SA}" --role roles/storage.objectAdmin >/dev/null

secret() { # name value-generator
  if ! gcloud secrets describe "$1" --project "$PROJECT" >/dev/null 2>&1; then
    "$2" | gcloud secrets create "$1" --project "$PROJECT" --replication-policy automatic --data-file=-
  fi
  gcloud secrets add-iam-policy-binding "$1" --project "$PROJECT" --member "serviceAccount:${SA}" --role roles/secretmanager.secretAccessor >/dev/null
}
gen_key() { head -c 48 /dev/urandom | base64 | tr -d '\n'; }
gen_code() { LC_ALL=C tr -dc 'A-HJ-NP-Z2-9' </dev/urandom | head -c 12 | sed 's/\(....\)\(....\)\(....\)/\1-\2-\3/'; }
secret "${SERVICE}-secret-key" gen_key
secret "${SERVICE}-setup-code" gen_code

ENV_VARS="OBX_DATA_DIR=/data,OBX_BLOB_URL=file:///blobs/images,OBX_TRUST_PROXY=true,OBX_LOG_FORMAT=json"
EXTRA=()
if [ -n "${DATABASE_URL:-}" ]; then
  printf '%s' "$DATABASE_URL" | gcloud secrets create "${SERVICE}-database-url" --project "$PROJECT" --data-file=- 2>/dev/null \
    || printf '%s' "$DATABASE_URL" | gcloud secrets versions add "${SERVICE}-database-url" --project "$PROJECT" --data-file=-
  gcloud secrets add-iam-policy-binding "${SERVICE}-database-url" --project "$PROJECT" --member "serviceAccount:${SA}" --role roles/secretmanager.secretAccessor >/dev/null
  SECRETS="OBX_SECRET_KEY=${SERVICE}-secret-key:latest,OBX_SETUP_CODE=${SERVICE}-setup-code:latest,OBX_DATABASE_URL=${SERVICE}-database-url:latest"
  if [ -n "${CLOUDSQL_INSTANCE:-}" ]; then
    EXTRA+=(--add-cloudsql-instances "$CLOUDSQL_INSTANCE")
  fi
else
  ENV_VARS="${ENV_VARS},LITESTREAM_REPLICA_URL=gcs://${BUCKET}/database"
  SECRETS="OBX_SECRET_KEY=${SERVICE}-secret-key:latest,OBX_SETUP_CODE=${SERVICE}-setup-code:latest"
fi
if [ "$ALWAYS_ON" = "1" ]; then
  EXTRA+=(--min-instances 1 --no-cpu-throttling)
else
  EXTRA+=(--min-instances 0)
fi

gcloud run deploy "$SERVICE" --project "$PROJECT" --region "$REGION" --source "$HERE" \
  --service-account "$SA" --execution-environment gen2 --max-instances 1 \
  --cpu 1 --memory 2Gi --timeout 3600 --port 8080 --allow-unauthenticated \
  --add-volume "name=storage,type=cloud-storage,bucket=${BUCKET}" --add-volume-mount "volume=storage,mount-path=/blobs" \
  --set-env-vars "$ENV_VARS" --set-secrets "$SECRETS" "${EXTRA[@]}"

URL="$(gcloud run services describe "$SERVICE" --project "$PROJECT" --region "$REGION" --format 'value(status.url)')"
PUBLIC_URL="${DOMAIN:+https://${DOMAIN}}"
PUBLIC_URL="${PUBLIC_URL:-$URL}"
gcloud run services update "$SERVICE" --project "$PROJECT" --region "$REGION" --update-env-vars "OBX_PUBLIC_URL=${PUBLIC_URL}" >/dev/null

echo
echo "Your brand portal runs at: ${PUBLIC_URL}"
echo "First-run setup code:      $(gcloud secrets versions access latest --secret "${SERVICE}-setup-code" --project "$PROJECT")"
if [ -n "${DOMAIN:-}" ]; then
  echo "Map your domain: gcloud beta run domain-mappings create --service ${SERVICE} --domain ${DOMAIN} --region ${REGION}"
fi
