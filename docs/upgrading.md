# Upgrading

Releases follow [Semantic Versioning](https://semver.org/). Within a major version (0.x.y → 0.x.z and, while the version starts with 0, minor updates too unless the [changelog](../CHANGELOG.md) says otherwise) an upgrade is: back up, replace the software, start it. Read the changelog entry of every release you skip.

## What happens on start

- Database migrations run automatically, each once and in its own transaction, **only forward**. There are no down-migrations.
- The node checks the schema version first: a database already migrated by a **newer** release is refused with a clear message instead of being used with the wrong schema. To go back to an older release, restore the backup you made before the upgrade.
- Settings are validated; a setting that a new release no longer accepts stops the start with a message naming it.

## Steps

1. **Back up** ([operations.md](operations.md#backups)): `node-go backup --to …` (or rely on Litestream / your PostgreSQL backups), and keep the secret key.
2. **Replace the software:**

| Hosting | Command |
|---|---|
| Docker Compose (this repository) | `git pull && docker compose up -d --build` |
| Docker with a published image | change the tag (`ghcr.io/openbrandexchange/node-go:0.3.0`) and `docker compose up -d` |
| Google Cloud Run | `git pull && ./deploy/cloudrun.sh` (same settings as the first time) |
| Fly.io | `git pull && fly deploy` |
| Railway / Render | push the new commit to your fork; the platform rebuilds |
| Linux server (systemd) | `git pull && sudo ./deploy/install-linux.sh --domain …`, or copy the new binary to `/usr/local/bin/node-go` and `sudo systemctl restart node-go` |

3. **Check:** `curl https://your-portal/readyz` shows the new `version` and `schema_version`; sign in once; look at the log for `warn` lines.

With SQLite and Litestream the old and the new instance must not run at the same time for long (both would write the replica): Cloud Run and Fly.io switch over within seconds. Changes made in those seconds on the old instance can be lost; update when the portal is quiet (evening), or use PostgreSQL for zero-downtime updates.

## From 0.2 to 0.3

0.3 implements spec 0.3 ([proposal 0004](https://github.com/openbrandexchange/spec/blob/main/docs/proposals/0004-terms-with-names.md)): materials and care are terms with names. **This is a breaking change for retailers' software**: `materials[].part`, `fibers[].material` and `fibers[].qualifier` are objects `{code, name}` instead of strings and flags, and care entries gain `category`, `symbol` and `name`. Tell your retailers before you upgrade; `OBX-Version` becomes `0.3`.

It adds migration 4, which changes no columns. On the first start after the upgrade the node rewrites every stored product once, in one transaction, before it serves requests: `part` and `fiber` become terms with the names of the specification's vocabularies, `recycled: true` / `organic: true` become the qualifier (`recycled` when a fibre had both), and care entries get their category, symbol and name. Every product whose materials or care changed gets an `upsert` in `/changes`, so retailers' software fetches the new shape. A 0.1 database is upgraded in the same start (migrations 3 and 4).

Brands' files and connectors keep working: the import format only gained `(<qualifier>)`, e.g. `(regenerative)`, and the Ingest API accepts terms with just a `code`. Ingest clients that still send the 0.2 shape (`"part": "outer"`, `"fiber": …`) get `422 validation_failed` and must move to `{"code": …}`. Rolling back to 0.2 needs the backup made before the upgrade.

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
