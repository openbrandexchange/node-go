# Implementation notes (node-go)

Decisions taken where the [spec](https://github.com/openbrandexchange/spec) (v0.2.0, fixtures synced from commit in `testdata/spec/SPEC_COMMIT`) left room, and extensions the shared admin-ui may rely on. Each is the simplest reading; challenge them with an issue.

## Status

- `go test ./...` covers the protocol against the Kestrel fixtures (JSON Schema validation of every response, expected products/assets/GTIN lookup), the ingest replay script (one operation at a time, as a JSON batch and as NDJSON), setup/sessions/CSRF, access requests, retailers, the portal, imports, exports and signed webhooks. The same tests run on PostgreSQL with `OBX_TEST_DATABASE_URL`, and against S3/SFTP test servers with `OBX_TEST_S3_ENDPOINT` / `OBX_TEST_SFTP`.
- `conformance/` runs the obx-go conformance suite (all profiles, including security) against an in-process node on SQLite and PostgreSQL; CI also runs the `obx-conformance` binary against a running `node-go serve` with `--rate-limit-test`: 65 of 65 pass on SQLite and PostgreSQL (obx-go 626b3a7, spec 43fa091).
- Operations, security, performance and hosting are tested too: see [docs/development.md](docs/development.md#checks) for the full list CI runs.

## obx-go

The node does **not** use obx-go's types at runtime: it needs exact control over `null` vs absent fields and over the stored representations it hashes for the change feed, and obx-go was written in parallel. obx-go is used where it adds the most, in `conformance/` (a separate Go module with `replace` directives to `../` and `../../obx-go`, so the main module builds without it). Running it needs a sibling checkout of obx-go; CI does this when the `OBX_READ_TOKEN` secret can read the private repository (the owner adds it while the repositories are private).

## Protocol and feed

1. **Renditions are made before the write.** When an ingest/admin/import write attaches an original whose `web`/`square`/`thumb` do not exist yet, the node renders them first (pure Go, Catmull-Rom after box-halving, JPEG q85, EXIF orientation applied, transparency flattened on white) and then writes, so the asset upsert appears in operation order and `visibility` is `visible` immediately. A background queue and a periodic sweep only catch leftovers (restarts, failures). An original that cannot be rendered stays `pending_files`; the error is kept and not retried forever.
2. **Replacing an original keeps serving the previous files** until the new set is complete (`assets.live_files`), then swaps in one upsert, on server- and client-rendered paths alike.
3. **Feed entries store no data**; `data` is rendered at read time, and upserts of objects not visible to the reader at that moment are omitted (protocol §6.2). A consumer that reads after a whole batch therefore sees only the deletes of objects that are gone. Cross-repo decision (2026-09-28): conformance test I-REPLAY-2 compares the batch replay against `expected-changes.json` **minus the upserts of objects no longer visible when the feed is read**, which is what §6.2 prescribes; the spec text and the fixture's README are aligned to that.
4. **Empty snapshot:** the first page is a snapshot page with no entries and `has_more: true`; the next page is live. Snapshot cursors expire after 24 h, feed cursors are signed (a foreign or tampered cursor is `cursor_invalid`).
5. **`updated_at`** is set to the write time whenever an upsert entry is written (ingest §8 rule 3), also for children republished by a cascade.
6. **Grants:** changing a retailer's brands or originals, unblocking it, or toggling "allow original download" writes a `grant` entry with `resync: true`; blocking writes one with `resync: false` (its clients get `access_revoked` anyway). A retailer's `allow_original: null` follows the portal setting.
7. **Access tokens** are stateless signed tokens (1 h) bound to the client and its secret version: rotating a secret or revoking a client ends its tokens immediately.
8. **Before setup** every `/obx/` endpoint answers `503 setup_required`, not only discovery.
9. **Change history** is kept 400 days by default (`OBX_CHANGE_RETENTION_DAYS`, minimum 90); older cursors get `cursor_expired` via a pruning watermark. Superseded entries are not compacted.
10. **Asset `variant_ids`** are resolved from `variant_external_ids` when the asset is written; unknown ids are dropped.

## Ingest and imports

11. **Rights and provenance are stored as differences from the defaults.** A field equal to the node default (or the provenance default) is not stored, so reading a record (defaults applied) and writing it back is `unchanged`, and an explicit value equal to today's default follows future default changes. File references drop `width`/`height` when the node reads them from the file.
12. **Batches of more than 200 operations** (or with `Prefer: respond-async`) run as a job (`202`); smaller ones answer `200` directly.
13. **Mirror imports** compare against the *published* products of the brands present in the file; more than half → `mass_unpublish_refused`, checked before anything is written.
14. **Dry runs** execute the whole import in one transaction that is rolled back; images are hashed but not stored or rendered.
15. **Image-only imports** match style numbers case-insensitively against the catalog (`style_number` or product external id) and colours against the product's variants. **Colourless images** are named `{style}_ALL_{position}.jpg` (`ALL`, any case, is reserved in file names; a colour really coded `ALL` is only reachable through `assets.csv`): they belong to every colour, their `color_code` is null and their external id is `{style}_ALL_{position}` (cross-repo decision 2026-09-28, the same id the admin-ui and node-ts use; the spec's import-format.md documents it).
16. **Scheduled imports:** the remote file list (name, size, modification time, ETag or content hash) is fingerprinted; an unchanged source records a succeeded run with `"x-node-go-unchanged": true` and imports nothing (`force` overrides). SFTP host keys are pinned on first use (written into `config.host_key_fingerprint`). Runs are ordinary jobs with leases, so a crashed run is picked up again; `next_run_at` is stored, so schedules survive restarts. Only one run per source at a time.
17. **Unreferenced blobs are not garbage-collected** yet (the spec allows it after 24 h).
18. **Uploads are buffered in the system temp directory** before they are stored (hash and type checks). On Cloud Run that is memory.

## Article data (spec 0.2)

18a. **Stored as part of the write model.** The 0.2 fields live in the JSON `data` of products and variants like the 0.1 fields; migration 3 only marks the database for a one-time rewrite on start (`upgradeArticleData`): normalised fibre keys give an upsert, objects that only gain the new `null` properties are stored silently (no change entry, `updated_at` kept).
18b. **Normalisation on write** (Ingest API, admin, imports): `recycled_<fibre>` / `organic_<fibre>` (any fibre, prefixes combinable) become the fibre with its flag; `false` flags are not stored; members of `features` / `measurements` that are `null` count as not stated, and an object left empty is `null`; an empty `certifications` list is `null`. A `footwear_materials` object is kept with all three parts (missing parts `null`).
18c. **Label completeness warnings** follow obx-go: textile item types (rule 10's list, `bag`, `backpack`, `luggage`) get `fibre_composition_missing`, `animal_origin_unknown` (any fibre ending in `_wool` counts as wool) and `fibre_percentages_not_100` (once per part, tolerance 0.01); footwear item types get `footwear_materials_missing`; other types none. They are reported on `unchanged` writes too, in `warnings` as `"<code>: <detail>"` and in `warning_details` with a JSON Pointer. In imports they are ImportRowErrors at the product's first row with the column of rule 10.
18d. **Import rows are all or nothing**: a row with an invalid product or variant value is skipped as a whole, so a product that appears only on rows with errors is not written (and not counted). Values in `color_family`, `size_length` or measurement cells of a row without variant columns are ignored with a warning; a different `color_family` on a later row of the same colour is a warning (the first wins). A `feature:<key>` column outside the registry gives one warning per column; a feature key that is not a valid key ignores the column with a warning.
18e. **CSV export** writes every registry `feature:` column, then the other keys of the exported products (sorted); booleans as `yes`/`no`, lists with `|`, numbers with `.` and without trailing zeros, `strap_length_cm` as `min-max` (one number when equal), certifications with ` (material)` for material scope.

## Admin + portal API

19. **Extensions** (not in `node-admin.yaml`, safe for other nodes to ignore):
    - `GET /admin/api/files/{sha256}?exp&v=a&sig` serves any stored file for signed admin previews (`AdminAsset.renditions` of unpublished assets).
    - `GET /admin/api/public/logo` serves the uploaded logo (`logo_url` points here).
    - `Settings.branding.portal_name` (and `PublicBranding.portal_name`): optional LocalizedText that overrides the local word in "<Brand> · <local word>" (admin-ui NOTES item 2). Without it the word is `glossary.portal` from the vendored locale file.
20. **Portal lists** are ordered by style number and paged with opaque offset cursors; facets count with every filter except their own dimension.
21. **Admins in the portal** see everything published, may download originals, and their downloads are not logged.
22. **Rendition URLs** in the portal and API are signed for one hour and carry the viewer (`v=`), so blocking a retailer stops its links on the next request.
23. **Statistics** count images for image/zip/API downloads (CSV excluded) and keep events as long as `Settings.retention.download_events_days` (default 730; the person's name at most 400 days); deleting a user or retailer anonymises the actor.
23a. **Operations extensions, aligned with node-ts** (not yet in `node-admin.yaml`; proposed for the spec): `GET /admin/api/audit-log` (filters `action`, `actor_id`, `target_id`, `outcome`; entry shape `{id, at, actor{type,id,name,role,email}, action, method, route, target_id, status, succeeded, request_id, ip: null, details}`), `GET /admin/api/retailers/{id}/export`, `GET /admin/api/users/{id}/export`, `GET /admin/api/me/export` (`format: obx-personal-data-export/1`), `DELETE /admin/api/me` (retailer staff only), and `Settings.retention` `{download_events_days, audit_log_days, access_requests_days}` (30–3650). node-go stores no IP addresses, so `ip` is always null. The admin-ui has no screens for these yet.
23b. **Failed sign-ins only** count toward the login rate limit (10 per 15 minutes per address and per account), so many people behind one office address are never locked out by signing in.

## E-mail and languages

24. E-mail texts are the `email.*` groups of the vendored admin-ui locale files `ui/dist/locales/<lang>.json` (invite, accessRequestReceived, accessRequestNew, accessApproved, accessRejected, zipReady, passwordReset; `{{placeholders}}`), composed into plain text in a fixed order, with built-in English copies as fallback. There is one invite text for buyers and colleagues. See [docs/languages-and-email.md](docs/languages-and-email.md). The local word of the portal name comes from `glossary.portal` in the same files (built-in list as fallback).
25. Links in e-mails assume these admin-ui routes: `/invite/{token}`, `/reset-password/{token}`, `/login`, `/downloads` (background downloads) and `/admin/requests` (access requests).
25a. **E-mails are retried**: a failed send is stored encrypted in `mail_outbox` and retried after 1, 5 and 15 minutes, then 1, 3, 6 and 12 hours; the text is deleted once sent or given up.

## Hosting

26. SQLite is the default everywhere; it needs a single instance (Cloud Run `max-instances=1`, one Fly machine). Writers are serialised in-process, and with PostgreSQL additionally by an advisory lock.
27. Rate limits are kept in memory per instance.
28. The Docker image contains Litestream; it only runs with `LITESTREAM_REPLICA_URL`. The Cloud Run script stores images on a Cloud Storage FUSE mount (no HMAC keys needed) and replicates the database to the same bucket.
29. Verified locally and in CI: the multi-arch Docker build; `scripts/test-docker.sh` (a plain container; a Litestream replica restored into a new container with an empty disk; `node-go backup` from the running node restored into a new volume; each followed by a full smoke test); `scripts/test-compose-https.sh` (the production Compose file with only `DOMAIN` and `ADMIN_EMAIL`, HTTPS through Caddy with its local CA for `localhost`, headers, redirect, restart); `scripts/test-cloudrun.sh` (every gcloud command and flag of `deploy/cloudrun.sh` checked against the real Google Cloud CLI 586); the Caddyfile adapted for a real domain (Let's Encrypt with the ACME e-mail, ZeroSSL fallback); GoReleaser snapshot builds; S3 (SeaweedFS) and SFTP (atmoz/sftp) sources. Not executed for real (no accounts, DNS or hosts here): a Cloud Run deployment, a Let's Encrypt certificate on a public domain, Fly.io, Railway, Render and the systemd installer.
30. **SSRF:** the checked address is the one connected to (dialer control hook); link-local and cloud-metadata addresses and host names are refused even with `OBX_ALLOW_PRIVATE_URLS=true` (cross-repo rule, the same in node-php and node-ts).
31. **CORS** only on `/.well-known/obx` (cross-repo rule).

## Excel import (spec import-format.md §5)

18f. **Standard library only**: `archive/zip` and `encoding/xml` (`internal/importfmt/xlsx.go`), so every limit of §5.6 is checked where we can see it: the entry count, declared sizes and ratio from the central directory before anything is inflated, the inflated bytes counted per part (an entry that inflates beyond its declared size is `workbook_too_large`), `DOCTYPE`s refused, only the five XML entities. Only `[Content_Types].xml`, the relationships, the workbook, styles, shared strings and worksheets are inflated; nothing is written to disk under an entry name.
18g. **Unsafe entry names are checked before `[Content_Types].xml` is parsed** (§5.6 step 5 before step 6), so a workbook with `vbaProject.bin` and a broken content-types part is `workbook_unsafe`. The rows go to the CSV import directly (§5.3 allows it), not through CSV text, so a header such as `a;b` cannot change the delimiter. Each part is inflated at most once (a sheet read for the sheet choice and again for the rows counts once toward 256 MiB). As the spec now says (§5.4): a `<v>` other than 0 outside 1E-308 ≤ |x| < 1E+308 after rounding to 15 digits is `workbook_invalid`, also in a date cell (checked as a number first); a date after 9999-12-31 is a conversion error. CDATA is text (`encoding/xml` returns it as character data).
18h. **A workbook that cannot be imported** gives its ImportRowError (`row` and `column` null) and fails the import with a problem of the same code (`422`), as an unreadable `products.csv` fails it (`import_invalid`); nothing of the import is written; the run is `failed`, its results and `errors_preview` hold that one ImportRowError (`counts.errors` 1), with `dry_run` too. A workbook above `MaxImportBytes` (the CSV limit) is refused the same way with `payload_too_large`. The upload is not checked while it is sent; the error comes with the import run. `.xls`, `.xlsm`, `.xlsb`, `.ods`, `.numbers` (and `.xltx`, `.xltm`, `.xlam`) in a `csv` or `assets_csv` part are `415 unsupported_media_type` at once.
18j. **Headers** (§1.1, CSV and the sheet choice alike): trimmed of space, tab, CR and LF, lowercase, each run of white space and `-` one `_`. An empty header name is no column: the CSV import does not warn about it; a workbook warns once per such column that holds values (`column C: `), and a conversion error under it has the locator `row <n>, column C: ` with `column` null.
18i. A package with two `products.xlsx` (in different folders) is `import_invalid`, as is a package or pull folder with both `products.csv` and `products.xlsx`. A scheduled web address whose path ends in `.xlsx` is read as `products.xlsx`. The "only `*.csv`" fallback applies only without `products.csv` and `products.xlsx`: a folder or package with `products.xlsx` and one other CSV reads the workbook and ignores the CSV.

## Tracked issues

Things a user could run into, with the reason they are not done yet. Each becomes a GitHub issue once the repository is public.

| # | Issue | Why not now |
|---|---|---|
| G1 | Renditions assume sRGB: embedded ICC profiles are not converted (originals keep them untouched). | Needs a colour-management library; pure Go has none that is maintained. Most product photos are sRGB. |
| G2 | Webhook deliveries use one attempt per 3 s tick; very large numbers of subscribers are not batched. | Fine for the expected tens of subscribers per brand. |
| G3 | Unreferenced blobs are not garbage-collected (spec: MAY after 24 h). | Needs a mark-and-sweep that is safe with concurrent uploads; storage cost is small. |
| G4 | Rate limits live in memory per instance. | The portal runs as one instance; a shared store is only needed for PostgreSQL with several instances. |
| G5 | `OBX_SECRET_KEY` cannot be rotated. | Needs re-encryption of stored credentials with two keys during the switch. |
| G6 | With SQLite + Litestream on Cloud Run, changes made on the old instance during the few seconds of a revision switch can be lost. | Inherent to one-writer SQLite replication; documented in docs/upgrading.md; PostgreSQL avoids it. |
| G7 | The admin-ui has no screens yet for the audit log, the personal-data exports, "delete my account" and the retention settings; they are API and command line only. | Screens belong in admin-ui (shared by all portals). |
| G8 | E-mail dates are written `2026-10-05` in every language. | Go has no locale date formatting built in; a month table per language is planned. |
| G9 | Importing the same file again counts brands it only refers to as nothing (75 unchanged after 76 created). | Waiting for the spec to say whether referenced brands count as unchanged. |
