# Upgrading

Releases follow [Semantic Versioning](https://semver.org/). Within a major version (0.x.y → 0.x.z and, while the version starts with 0, minor updates too unless the [changelog](../CHANGELOG.md) says otherwise) an upgrade is: back up, replace the software, start it. Read the changelog entry of every release you skip.

## What happens on start

- Database migrations run automatically, each once and in its own transaction, **only forward**. There are no down-migrations.
- The node checks the schema version first: a database already migrated by a **newer** release is refused with a clear message instead of being used with the wrong schema. To go back to an older release, restore the backup you made before the upgrade.
- Settings are validated; a setting that a new release no longer accepts stops the start with a message naming it.

## Steps

1. **Back up** ([operations.md](operations.md#backups)): `node-go backup --to …` (or rely on Litestream / your PostgreSQL backups), and keep the secret key.
2. **Replace the software:**

   On a server set up with the installer, run the install command again; it downloads the newest
   release, verifies it, keeps the settings and data and restarts the portal:

   ```sh
   curl -fsSL https://openbrandexchange.com/install.sh | sudo sh -s -- --domain portal.yourbrand.com
   ```

   Add `--version X.Y.Z` for a specific release. Installed by hand: replace `/usr/local/bin/node-go`
   with the binary from the new release archive and `sudo systemctl restart node-go`.

3. **Check:** `curl https://your-portal/readyz` shows the new `version` and `schema_version`; sign in once; look at the log for `warn` lines.

The portal is unavailable for the few seconds of the restart; running requests get 25 seconds to finish and interrupted jobs resume afterwards. Update when the portal is quiet (evening) if you like.

## From 0.2 to 0.3

0.3 implements spec 0.3 ([proposal 0004](https://github.com/openbrandexchange/spec/blob/main/docs/proposals/0004-terms-with-names.md)): materials and care are terms with names. **This is a breaking change for retailers' software**: `materials[].part`, `fibers[].material` are objects `{code, name}` instead of strings, `fibers[].qualifiers` is a list of such objects instead of the flags `recycled`/`organic` (`[]` when none), and care entries gain `category`, `symbol` and `name`. Tell your retailers before you upgrade; `OBX-Version` becomes `0.3`.

It adds migrations 4 and 5 (5 adds the table `product_qualifiers` for the portal's qualifier filter). On the first start after the upgrade the node rewrites every stored product once, in one transaction, before it serves requests: `part` and `fiber` become terms with the names of the specification's vocabularies, `recycled: true` / `organic: true` become qualifiers (`[recycled, organic]` when a fibre had both), and care entries get their category, symbol and name. Every product whose materials or care changed gets an `upsert` in `/changes`. Every `/changes` cursor issued before the upgrade (including the `changes_cursor` of earlier exports) answers `410 cursor_expired` ([protocol.md §6.4](https://github.com/openbrandexchange/spec/blob/main/docs/protocol.md#64-retention-and-cursors)): retailers' software restarts the feed without a cursor and gets a snapshot in the new shape. A 0.1 database is upgraded in the same start (migrations 3 to 5). A database that ran a pre-release build of 0.3 (one `qualifier` per fibre) is rewritten by migration 5 the same way: the `qualifier` becomes a list of one, and those products get an `upsert`; its cursors stay valid.

Brands' files and connectors keep working: the import format only gained qualifiers in brackets, one code per bracket, e.g. `(regenerative)` or `(recycled) (post_consumer)`, and the Ingest API accepts terms with just a `code`. Ingest clients that still send the 0.2 shape (`"part": "outer"`, `"fiber": …`) get `422 validation_failed` and must move to `{"code": …}`. Also new in 0.3, without any action for existing data: **variants made by an import keep their external ids** (the GTIN, which was the default before 0.3); a file row without `variant_id` now finds its variant by GTIN first, and new variants get the default id `<style>-<color>-<size>[-<size_length>]`. A fibre's `percentage` may be `null`; `x-<vendor>-` members are now stored and returned; tokens issued before a secret rotation get `invalid_token`; an expired or tampered signed URL is `403`; a `limit` above the maximum is clamped. Rolling back to 0.2 needs the backup made before the upgrade.

## From 0.1 to 0.2

0.2 implements spec 0.2 (article data). It adds migration 3, which changes no columns: products and variants keep their write model as JSON. On the first start after the upgrade the node rewrites every stored product and variant once, in one transaction, before it serves requests:

- the 0.1 fibre keys `recycled_<fibre>` / `organic_<fibre>` become the fibre with `recycled` / `organic` (a 0.2 node never returns the 0.1 keys); those products get an `upsert` in `/changes`, so retailers' software picks up the new form;
- every other product and variant only gains the new properties (`null`), without a change entry, so the upgrade does not make every retailer fetch the whole catalog again; their ETags in the Ingest API change once.

Nothing else is needed: imports, `obx push` and the Ingest API keep working, and re-importing the same files afterwards changes nothing. Rolling back to 0.1 needs the backup made before the upgrade (migration 3).

## Rolling back

- A release that did **not** add a migration (the changelog says so; `schema_version` in `/readyz` stays the same): start the previous version.
- A release that **did**: stop the portal, `node-go restore --from <backup made before the upgrade> --force` with the previous version, start it. Changes made after the backup are lost; with Litestream restore to a timestamp instead ([operations.md](operations.md#continuous-litestream-sqlite)).

## Version numbers

`node-go version` prints the version; the discovery document (`/.well-known/obx` → `node.version`), `/readyz` and the `obx_build_info` metric show it too. The OBX protocol version a release implements is listed in the changelog; a new protocol major version is always a new major version of node-go.
