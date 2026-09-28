# Implementation notes (node-go)

Decisions taken where the [spec](https://github.com/openbrandexchange/spec) (v0.1.0-draft, fixtures synced from commit in `testdata/spec/SPEC_COMMIT`) left room, and extensions the shared admin-ui may rely on. Each is the simplest reading; challenge them with an issue.

## Status

- `go test ./...` covers the protocol against the Kestrel fixtures (JSON Schema validation of every response, expected products/assets/GTIN lookup), the ingest replay script (one operation at a time, as a JSON batch and as NDJSON), setup/sessions/CSRF, access requests, retailers, the portal, imports, exports and signed webhooks. The same tests run on PostgreSQL with `OBX_TEST_DATABASE_URL`, and against S3/SFTP test servers with `OBX_TEST_S3_ENDPOINT` / `OBX_TEST_SFTP`.
- `conformance/` runs the obx-go conformance suite (all profiles) against an in-process node: 47 of 47 tests pass (obx-go commit 00371c4).

## obx-go

The node does **not** use obx-go's types at runtime: it needs exact control over `null` vs absent fields and over the stored representations it hashes for the change feed, and obx-go was written in parallel. obx-go is used where it adds the most, in `conformance/` (a separate Go module with `replace` directives to `../` and `../../obx-go`, so the main module builds without it). Running it needs a sibling checkout of obx-go; CI does this when the `OBX_GO_TOKEN` secret can read the private repository.

## Protocol and feed

1. **Renditions are made before the write.** When an ingest/admin/import write attaches an original whose `web`/`square`/`thumb` do not exist yet, the node renders them first (pure Go, Catmull-Rom after box-halving, JPEG q85, EXIF orientation applied, transparency flattened on white) and then writes, so the asset upsert appears in operation order and `visibility` is `visible` immediately. A background queue and a periodic sweep only catch leftovers (restarts, failures). An original that cannot be rendered stays `pending_files`; the error is kept and not retried forever.
2. **Replacing an original keeps serving the previous files** until the new set is complete (`assets.live_files`), then swaps in one upsert, on server- and client-rendered paths alike.
3. **Feed entries store no data**; `data` is rendered at read time, and upserts of objects not visible to the reader at that moment are omitted (protocol §6.2). A consumer that reads after a whole batch therefore sees only the deletes of objects that are gone; the conformance suite reads per operation.
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
15. **Image-only imports** match style numbers case-insensitively against the catalog (`style_number` or product external id) and colours against the product's variants.
16. **Scheduled imports:** the remote file list (name, size, modification time, ETag or content hash) is fingerprinted; an unchanged source records a succeeded run with `"x-node-go-unchanged": true` and imports nothing (`force` overrides). SFTP host keys are pinned on first use (written into `config.host_key_fingerprint`). Runs are ordinary jobs with leases, so a crashed run is picked up again; `next_run_at` is stored, so schedules survive restarts. Only one run per source at a time.
17. **Unreferenced blobs are not garbage-collected** yet (the spec allows it after 24 h).
18. **Uploads are buffered in the system temp directory** before they are stored (hash and type checks). On Cloud Run that is memory.

## Admin + portal API

19. **Extensions** (not in `node-admin.yaml`, safe for other nodes to ignore):
    - `GET /admin/api/files/{sha256}?exp&v=a&sig` serves any stored file for signed admin previews (`AdminAsset.renditions` of unpublished assets).
    - `GET /admin/api/public/logo` serves the uploaded logo (`logo_url` points here).
    - `Settings.branding.portal_name` (and `PublicBranding.portal_name`): optional LocalizedText that overrides the local word in "<Brand> · <local word>" (admin-ui NOTES item 2). Without it the word is `glossary.portal` from the vendored locale file.
20. **Portal lists** are ordered by style number and paged with opaque offset cursors; facets count with every filter except their own dimension.
21. **Admins in the portal** see everything published, may download originals, and their downloads are not logged.
22. **Rendition URLs** in the portal and API are signed for one hour and carry the viewer (`v=`), so blocking a retailer stops its links on the next request.
23. **Statistics** count images for image/zip/API downloads (CSV excluded) and keep events one year; deleting a user or retailer anonymises the actor.

## E-mail and languages

24. E-mail texts come from the vendored admin-ui locale files `ui/dist/locales/<lang>.json` (keys `email.<kind>.subject|body`, `email.greeting`, `email.footer`; placeholders `{portal}`, `{brand}`, `{name}`, `{link}`, `{retailer}`, `{contact}`, `{reason}`, `{files}`, `{expires_days}`), with built-in English fallbacks. See [docs/languages-and-email.md](docs/languages-and-email.md). The local word for the portal name ("Beeldbank", "Händlerportal", "Espace revendeurs", …) is built in for the 25 languages until the glossary provides it.
25. Links in e-mails assume these admin-ui routes: `/invite/{token}`, `/reset-password/{token}`, `/login`, `/downloads` (background downloads) and `/admin/access-requests/{id}`.

## Hosting

26. SQLite is the default everywhere; it needs a single instance (Cloud Run `max-instances=1`, one Fly machine). Writers are serialised in-process, and with PostgreSQL additionally by an advisory lock.
27. Rate limits are kept in memory per instance.
28. The Docker image contains Litestream; it only runs with `LITESTREAM_REPLICA_URL`. The Cloud Run script stores images on a Cloud Storage FUSE mount (no HMAC keys needed) and replicates the database to the same bucket.
29. Verified locally: Docker build and container start, the local Compose stack, Litestream to a file replica, Caddyfile validation, shellcheck of all scripts, and S3 (SeaweedFS) and SFTP (atmoz/sftp) sources. Not executed: the Cloud Run, Fly.io, Railway and Render deploys and the systemd installer (no accounts/host in this environment).

## Known gaps

- Renditions assume sRGB: embedded ICC profiles are not converted (originals keep them untouched).
- Webhook deliveries use one attempt per 3 s tick; very large numbers of subscribers are not batched.
