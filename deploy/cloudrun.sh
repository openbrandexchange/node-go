#!/usr/bin/env bash
# Deploy the brand portal to Google Cloud Run in one command:
#
#   ./deploy/cloudrun.sh
#
# Needs: the Google Cloud CLI, logged in (gcloud auth login), a project with
# billing enabled in which you are Owner (gcloud config set project <id>).
# In Cloud Shell all of that is already the case.
#
# What it sets up (idempotent: run it again to update to the current code):
#   - a Cloud Storage bucket for images (mounted into the container) and for
#     the continuous SQLite backup (Litestream);
#   - Secret Manager secrets for OBX_SECRET_KEY and the first-run setup code;
#   - a runtime service account that may use exactly that bucket and those
#     secrets, and a build service account for Cloud Build;
#   - the Cloud Run service itself (built from this folder by Cloud Build),
#     with one instance: SQLite needs a single writer.
#
# Settings (environment variables, all optional):
#   PROJECT    Google Cloud project   (default: gcloud config project)
#   REGION     region                 (default: europe-west4)
#   SERVICE    service name           (default: brand-portal)
#   BUCKET     bucket name            (default: <project>-<service>)
#   DOMAIN     custom domain, e.g. images.yourbrand.com (map it afterwards,
#              see docs/hosting/cloud-run.md)
#   MEMORY     instance memory        (default: 2Gi; the SQLite database lives in it)
#   DATABASE_URL  a Postgres URL (e.g. Cloud SQL) instead of SQLite+Litestream
#   CLOUDSQL_INSTANCE  project:region:instance to attach when using Cloud SQL
#   SMTP_URL, MAIL_FROM  e-mail (stored as a secret / plain setting)
#   ALWAYS_ON  1 (default) keeps one instance running so scheduled imports,
#              webhooks and background downloads run on time; 0 scales to zero.
set -euo pipefail

GCLOUD="${GCLOUD:-gcloud}"
command -v "$GCLOUD" >/dev/null || { echo "Install the Google Cloud CLI first: https://cloud.google.com/sdk/docs/install (or use Cloud Shell)" >&2; exit 1; }
g() { "$GCLOUD" "$@"; }

PROJECT="${PROJECT:-$(g config get-value project 2>/dev/null || true)}"
[ -n "$PROJECT" ] && [ "$PROJECT" != "(unset)" ] || { echo "Set PROJECT=<project-id> or run: gcloud config set project <project-id>" >&2; exit 1; }
REGION="${REGION:-europe-west4}"
SERVICE="${SERVICE:-brand-portal}"
BUCKET="${BUCKET:-${PROJECT}-${SERVICE}}"
MEMORY="${MEMORY:-2Gi}"
ALWAYS_ON="${ALWAYS_ON:-1}"
case "$SERVICE" in *[!a-z0-9-]*|-*|*-) echo "SERVICE may contain only lowercase letters, digits and dashes" >&2; exit 1 ;; esac
RUNTIME_SA="${SERVICE}-runner@${PROJECT}.iam.gserviceaccount.com"
BUILD_SA="${SERVICE}-builder@${PROJECT}.iam.gserviceaccount.com"
HERE="$(cd "$(dirname "$0")/.." && pwd)"

echo "==> Deploying ${SERVICE} to project ${PROJECT} in ${REGION}"
PROJECT_NUMBER="$(g projects describe "$PROJECT" --format 'value(projectNumber)')"
[ -n "$PROJECT_NUMBER" ] || { echo "Project ${PROJECT} not found or not accessible" >&2; exit 1; }
# Cloud Run's address is predictable, so the first revision already knows it.
RUN_URL="https://${SERVICE}-${PROJECT_NUMBER}.${REGION}.run.app"
PUBLIC_URL="${DOMAIN:+https://${DOMAIN}}"
PUBLIC_URL="${PUBLIC_URL:-$RUN_URL}"

echo "==> Enabling the APIs (fails here when billing is not enabled for the project)"
g services enable run.googleapis.com cloudbuild.googleapis.com artifactregistry.googleapis.com \
  secretmanager.googleapis.com storage.googleapis.com iam.googleapis.com --project "$PROJECT"

echo "==> Bucket gs://${BUCKET}"
if ! g storage buckets describe "gs://${BUCKET}" --project "$PROJECT" >/dev/null 2>&1; then
  g storage buckets create "gs://${BUCKET}" --project "$PROJECT" --location "$REGION" --uniform-bucket-level-access --public-access-prevention
fi

echo "==> Service accounts"
for sa in runner builder; do
  if ! g iam service-accounts describe "${SERVICE}-${sa}@${PROJECT}.iam.gserviceaccount.com" --project "$PROJECT" >/dev/null 2>&1; then
    g iam service-accounts create "${SERVICE}-${sa}" --project "$PROJECT" --display-name "Brand portal ${SERVICE} (${sa})"
  fi
done
g storage buckets add-iam-policy-binding "gs://${BUCKET}" --member "serviceAccount:${RUNTIME_SA}" --role roles/storage.objectAdmin >/dev/null
# Cloud Build builds the image from this folder with its own account.
g projects add-iam-policy-binding "$PROJECT" --member "serviceAccount:${BUILD_SA}" --role roles/run.builder --condition None >/dev/null

echo "==> Secrets"
secret() { # name value-generator
  if ! g secrets describe "$1" --project "$PROJECT" >/dev/null 2>&1; then
    "$2" | g secrets create "$1" --project "$PROJECT" --replication-policy automatic --data-file=-
  fi
  g secrets add-iam-policy-binding "$1" --project "$PROJECT" --member "serviceAccount:${RUNTIME_SA}" --role roles/secretmanager.secretAccessor >/dev/null
}
secret_value() { # name value: create or add a version when it changed
  if ! g secrets describe "$1" --project "$PROJECT" >/dev/null 2>&1; then
    printf '%s' "$2" | g secrets create "$1" --project "$PROJECT" --replication-policy automatic --data-file=-
  elif [ "$(g secrets versions access latest --secret "$1" --project "$PROJECT")" != "$2" ]; then
    printf '%s' "$2" | g secrets versions add "$1" --project "$PROJECT" --data-file=-
  fi
  g secrets add-iam-policy-binding "$1" --project "$PROJECT" --member "serviceAccount:${RUNTIME_SA}" --role roles/secretmanager.secretAccessor >/dev/null
}
gen_key() { head -c 48 /dev/urandom | base64 | tr -d '\n'; }
# (no "tr </dev/urandom | head": with pipefail the killed tr fails the script)
gen_code() { head -c 4096 /dev/urandom | LC_ALL=C tr -dc 'A-HJ-NP-Z2-9' | cut -c1-12 | sed 's/\(....\)\(....\)\(....\)/\1-\2-\3/'; }
secret "${SERVICE}-secret-key" gen_key
secret "${SERVICE}-setup-code" gen_code
SECRETS="OBX_SECRET_KEY=${SERVICE}-secret-key:latest,OBX_SETUP_CODE=${SERVICE}-setup-code:latest"

# GOMEMLIMIT keeps the Go heap below the instance memory (the database and
# uploads in /tmp live in the same memory on Cloud Run).
mem_mib() { case "$1" in *Gi) echo $(( ${1%Gi} * 1024 ));; *Mi) echo "${1%Mi}";; *) echo 2048;; esac; }
GOMEMLIMIT="$(( $(mem_mib "$MEMORY") / 2 ))MiB"
ENV_VARS="OBX_PUBLIC_URL=${PUBLIC_URL},OBX_DATA_DIR=/data,OBX_BLOB_URL=file:///blobs/images,OBX_TRUST_PROXY=true,OBX_LOG_FORMAT=json,GOMEMLIMIT=${GOMEMLIMIT}"
EXTRA=()
if [ -n "${DATABASE_URL:-}" ]; then
  secret_value "${SERVICE}-database-url" "$DATABASE_URL"
  SECRETS="${SECRETS},OBX_DATABASE_URL=${SERVICE}-database-url:latest"
  if [ -n "${CLOUDSQL_INSTANCE:-}" ]; then
    EXTRA+=(--add-cloudsql-instances "$CLOUDSQL_INSTANCE")
  fi
else
  ENV_VARS="${ENV_VARS},LITESTREAM_REPLICA_URL=gcs://${BUCKET}/database"
fi
if [ -n "${SMTP_URL:-}" ]; then
  secret_value "${SERVICE}-smtp-url" "$SMTP_URL"
  SECRETS="${SECRETS},OBX_SMTP_URL=${SERVICE}-smtp-url:latest"
fi
if [ -n "${MAIL_FROM:-}" ]; then
  ENV_VARS="${ENV_VARS},OBX_MAIL_FROM=${MAIL_FROM}"
fi
if [ "$ALWAYS_ON" = "1" ]; then
  EXTRA+=(--min-instances 1 --no-cpu-throttling)
else
  EXTRA+=(--min-instances 0 --cpu-throttling)
fi

echo "==> Building and deploying (Cloud Build, a few minutes the first time)"
g run deploy "$SERVICE" --project "$PROJECT" --region "$REGION" --source "$HERE" --quiet \
  --build-service-account "projects/${PROJECT}/serviceAccounts/${BUILD_SA}" \
  --service-account "$RUNTIME_SA" --execution-environment gen2 --max-instances 1 \
  --cpu 1 --memory "$MEMORY" --concurrency 80 --timeout 3600 --port 8080 --allow-unauthenticated \
  --startup-probe "httpGet.path=/readyz,httpGet.port=8080,periodSeconds=5,timeoutSeconds=5,failureThreshold=60" \
  --liveness-probe "httpGet.path=/healthz,httpGet.port=8080,periodSeconds=30,timeoutSeconds=5,failureThreshold=3" \
  --add-volume "name=storage,type=cloud-storage,bucket=${BUCKET}" --add-volume-mount "volume=storage,mount-path=/blobs" \
  --set-env-vars "$ENV_VARS" --set-secrets "$SECRETS" "${EXTRA[@]}"

echo
echo "Your brand portal runs at: ${RUN_URL}"
[ "$PUBLIC_URL" = "$RUN_URL" ] || echo "Public address (after the domain mapping below): ${PUBLIC_URL}"
echo "First-run setup code:      $(g secrets versions access latest --secret "${SERVICE}-setup-code" --project "$PROJECT")"
if [ -n "${DOMAIN:-}" ]; then
  echo
  echo "Map your domain once (then add the DNS records it prints):"
  echo "  gcloud beta run domain-mappings create --service ${SERVICE} --domain ${DOMAIN} --region ${REGION} --project ${PROJECT}"
fi
