# Changelog

All notable changes to the Portal for Google Cloud & Docker (`node-go`) are listed here. The project
follows [Semantic Versioning](https://semver.org/): the major version changes when an upgrade needs
more than replacing the software (see [docs/upgrading.md](docs/upgrading.md)); database migrations
are always forward-only and applied automatically on start.

## [Unreleased]

### Added
- **Excel import** ([import-format.md §5](https://github.com/openbrandexchange/spec/blob/main/docs/import-format.md#excel)):
  `products.xlsx` and `assets.xlsx` are accepted wherever `products.csv` and `assets.csv` are
  (admin upload, Ingest API `csv` / `assets_csv` parts, package zips, scheduled pulls from a web
  address, SFTP folder or S3 prefix, `node-go import --csv`). The workbook is turned into exactly
  the rows of the CSV file and imported as such; errors carry Excel's row numbers and a
  `row 12, column ean (C): ` locator. The spec's templates import as they are. Standard library
  only, with the fixed limits of §5.6 (zip bombs, rows, columns), and macros, ActiveX, external
  links, data connections and `DOCTYPE`s refused.
- Error codes `workbook_invalid`, `workbook_unsafe`, `workbook_sheet_missing` and
  `workbook_too_large` (all `422`); `.xls`, `.xlsm`, `.xlsb`, `.ods` and `.numbers` uploads are
  `415 unsupported_media_type`.
- Aligned with node-ts and node-php (spec commit 9219d68): numbers outside Excel's range
  (1E-308 to below 1E+308) are `workbook_invalid`, a date after 9999-12-31 a conversion error; a
  column with an empty header gives a warning when it holds values (locator `row 3, column C: `);
  header names are normalised with each run of white space and `-` as one `_` (CSV too); a part
  read twice counts once toward the inflated limit.
- JSON Schemas and fixtures from spec commit 9219d68 (`testdata/spec/imports/xlsx`).

## [0.2.0] - 2026-11-12

Implements Open Brand Exchange protocol 0.2 (spec 0.2.0, [proposal 0002](https://github.com/openbrandexchange/spec/blob/main/docs/proposals/0002-product-data.md):
article data for fashion, shoes, bags and luggage) and passes `obx-conformance all` (65 of 65
tests, including P-CAT-5, P-CAT-6, I-IMP-7, I-IMP-8 and the rate-limit test) on SQLite and
PostgreSQL. Additive: retailers' software written for 0.1 keeps working.

### Added
- **Article data** stored and returned everywhere (protocol, ingest, admin, portal, exports,
  webhooks): product `item_subtype`, `contains_animal_origin`, `footwear_materials`,
  `certifications`, `features` (the typed feature registry: shell, wheels, lock, expandable,
  closure, laptop compartment, water resistance, detachable and adjustable parts, fit, sleeve
  length, neckline; other keys are kept as sent); variant `size_length`, `measurements` (cm, kg,
  l, inch) and `color.family`; the fibre flags `recycled` and `organic`. Products and variants
  always carry the new properties (`null` when not stated); measurements and features only
  their stated members.
- **Imports** (upload, zip, scheduled pull, `node-go import`) read the 0.2 columns:
  `item_subtype`, `contains_animal_origin`, `footwear_materials`, `certifications`
  (`scheme:licence (material)`), `feature:<key>`, `color_family` (`gray`/`multicolor`
  accepted), `size_length` and the measurement columns (decimal comma accepted,
  `strap_length_cm` as `min-max` or one number), and `(recycled)` / `(organic)` in materials.
  A column the file does not have keeps the stored value per feature and per measurement.
  A row with an invalid value is skipped as a whole.
- **Label completeness warnings** (`fibre_composition_missing`, `animal_origin_unknown`,
  `footwear_materials_missing`, `fibre_percentages_not_100`) in import results and in the
  Ingest API's `warning_details`; they never block publishing.
- The retailers' article data CSV (portal download, selection, `data.csv` in background zips)
  has the 0.2 columns in the order of the import format, a `feature:<key>` column per registry
  key, and imports again unchanged.
- The import template shows a feature, the colour family and a weight.

### Changed
- Admin screens: vendored admin-ui 0.2.0 (label information, measurements and features on the
  article pages, editing them in the brand admin, label warnings in import results).
- The 0.1 fibre keys `recycled_<fibre>` and `organic_<fibre>` are normalised to the fibre with
  its flag on every write path; a 0.2 node never returns them. Registry feature values outside
  their type, measurements that are not positive numbers, a strap whose minimum is larger than
  its maximum, and footwear materials or colour families outside their lists are
  `422 validation_failed`; empty `features` / `measurements` objects are stored as `null`.
- The percentages of a material part no longer give a generic import warning; for textile
  products they give `fibre_percentages_not_100`.
- `OBX-Version: 0.2`, discovery `obx_version` `0.2` and `spec_version` `0.2.0`; JSON Schemas and
  fixtures from spec 0.2.0 (`testdata/spec`, now with `fixtures/imports`);
  `testdata/kestrel-example.zip` has the 11 articles and 35 images of the 0.2 fixtures.

### Upgrading
- Database migration 3 (no column changes). On the first start the node rewrites the stored
  products and variants once in their 0.2 form: an object whose 0.1 fibre keys were normalised
  gets an upsert in `/changes`; every other object only gains the new `null` properties and
  writes no change entry. See [docs/upgrading.md](docs/upgrading.md#from-01-to-02).

## [0.1.0] - 2026-11-12

First release. Implements Open Brand Exchange protocol 0.1 (read API, ingest API, node admin and
portal API) and passes `obx-conformance all` (61 of 61 tests, including the rate-limit test) on
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

### Also in this release

#### Added

- `DRY_RUN=1 ./deploy/cloudrun.sh` prints the `gcloud` commands a real run executes and changes
  nothing; it works without the Google Cloud CLI.

#### Changed

- The web interface is admin-ui 0.1.1 (commit d3ee574): invitation language, clearer
  scheduled-import errors, websites typed without https://, "Show the key" after approving an API
  key request, and a "Get started" step to set up e-mail while none is configured.
- The web interface is served under admin-ui's strict Content-Security-Policy: no inline styles and
  no fonts from other hosts.

#### Fixed

- The setup code is accepted in lowercase, with spaces instead of dashes, or without dashes.
- E-mails no longer show a double full stop after a company name that ends in one ("B.V..").
- A successful connection test of a scheduled import no longer adds an English line under the
  translated result.
- A missing file under `/assets/` or `/locales/` is a 404, also when the browser accepts HTML, so a
  tab left open during an upgrade reloads once instead of reading the start page as code.
- An import no longer takes away what it does not state about an image (spec import rule 8): the
  images alone, or with an assets.csv that has no row for them, keep their embargo, credit, alt
  text and AI label, also when those were set in the admin. Before, every such import reset them
  to the defaults, which lifted embargoes.
- Every zip a retailer downloads (one product, the selection, a background download) holds an
  `assets.csv` with one row per image: its usage rights, embargo and AI provenance in the import
  format's columns (spec import-format.md §2.3). Before, a zip held only the files, so a shop could
  publish an embargoed image without seeing the embargo.
- A column that a products.csv does not have leaves that field of an existing product or variant as
  it is (spec import rule 9, per language for texts): a file with only `style_number` and `name:nl`
  changes only the Dutch names. Before, it removed the descriptions, materials, care codes and the
  other languages. An empty cell in a column the file has still removes the value.
- Image names: the rightmost role or view wins (`_back_front` is view front) and `_model_other` is
  role model with view other, as the spec says; before, such a file was refused.
- Discovery reports `spec_version` 0.1.0 instead of 0.1.0-draft.
- A new API key without chosen scopes gets `catalog:read` and `media:read`, plus `media:original`
  when the retailer may download originals, as the spec says; it got every scope, `usage:write`
  included.
- Renewing an API key that was made when approving an access request retires the secret still
  waiting for the requester's software, so the status address no longer hands out a secret that
  stopped working ("Show the key" in the admin renews it).

[0.1.0]: https://github.com/openbrandexchange/node-go/releases/tag/v0.1.0
