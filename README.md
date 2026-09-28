# Portal for Google Cloud & Docker

**Your own brand portal for retailers, in one program.** Retailers log in, find your products, paste a list of EANs, and download images and article data. Their shop software can fetch the same content automatically. You publish by uploading a spreadsheet and photos, or let your own systems do it for you.

This is `node-go`: the Go edition of an [Open Brand Exchange](https://openbrandexchange.com) node. It runs as a single program with no other services required, on Google Cloud Run, any Docker host, Fly.io, Railway, Render or a plain Linux server.

| For | What they get |
|---|---|
| **You, the brand** | A setup wizard, your catalog, uploads and scheduled imports, retailers and access requests, invitations, API keys for retailers, automatic upload keys for your own systems, download statistics, your logo and colours. |
| **Retailers' staff** | The portal: request access, search and filter, product pages with images and article data, paste EANs, select, download one image, a product, a selection or everything as a zip, and article data as CSV. |
| **Retailers' software** | The standard OBX API: changes since last time, products, EAN lookup, images, bulk exports, webhooks. |
| **Your automation** | The standard upload API, scheduled imports from a web address, SFTP or S3 bucket, and a command-line tool. |

The interface speaks 25 European languages. Images keep their original bytes (including embedded Content Credentials); the portal makes web, square and thumbnail versions itself.

## Try it in 60 seconds

With Docker:

```sh
git clone https://github.com/openbrandexchange/node-go && cd node-go
docker compose -f docker-compose.local.yml up
```

Or with Go 1.26 or newer (it fetches the exact toolchain of `go.mod` itself):

```sh
go run . serve
```

Open <http://localhost:8080>. The output shows a line like `First-run setup: open http://localhost:8080 and enter this setup code: 55GX-FKYX-E5HK`; enter that code in the setup wizard together with your company name and your own password (there is never a default password).

Then add the example catalog of the fictional brand Kestrel Outfitters. In the admin, go to **Imports → Choose files**, pick `testdata/kestrel-example.zip` (the spreadsheet and 27 images in one zip) and click **Start import**. Or, when you started with Go:

```sh
go run . import --csv testdata/spec/kestrel/products.csv \
  --assets testdata/spec/kestrel/assets.csv --images testdata/spec/kestrel/images
```

To see the retailer side, click **View as retailer**, or invite yourself under **Retailers**. Without an e-mail server nothing is sent: the admin shows the invitation link to copy, and the Docker try-out also writes the whole e-mail to the output (`OBX_MAIL_LOG_BODIES=true`).

Everything is stored in `./data` with Go, or in the Docker volume `obx-local-data`.

A scheduled import only fetches from `https://` addresses outside your own network. To try one on your own computer, against a file served from it or a local SFTP or MinIO server, start the portal with `OBX_ALLOW_PRIVATE_URLS=true` (for example under `environment:` in `docker-compose.local.yml`) and use `http://host.docker.internal:<port>/…` as the address. Never set it on a real server.

## Put it online

Each option is one command; the guides explain domain names, e-mail and backups.

| Where | Command | Guide |
|---|---|---|
| Google Cloud Run | `./deploy/cloudrun.sh` (`DRY_RUN=1` first to only see the commands) | [docs/hosting/cloud-run.md](docs/hosting/cloud-run.md) |
| Your own server with Docker (automatic HTTPS) | `DOMAIN=images.yourbrand.com ADMIN_EMAIL=you@yourbrand.com docker compose up -d` | [docs/hosting/docker.md](docs/hosting/docker.md) |
| Fly.io | `./deploy/fly.sh your-portal` | [docs/hosting/fly.md](docs/hosting/fly.md) |
| Railway or Render | deploy this repository (the config files are included) | [docs/hosting/railway-render.md](docs/hosting/railway-render.md) |
| A Linux server without Docker | `sudo ./deploy/install-linux.sh --domain images.yourbrand.com --with-caddy` | [docs/hosting/linux.md](docs/hosting/linux.md) |

Settings are environment variables with sensible defaults: see [docs/configuration.md](docs/configuration.md). Storage can be local disk (default), any S3-compatible bucket (AWS S3, Cloudflare R2, MinIO) or Google Cloud Storage; the database SQLite (default, continuously backed up with Litestream if you like) or PostgreSQL.

Running it for real: [docs/operations.md](docs/operations.md) (health checks, logs, metrics, backups and restores, retention, the audit log, personal data), [docs/upgrading.md](docs/upgrading.md), [docs/security.md](docs/security.md), [docs/performance.md](docs/performance.md) (tested with 10 000 products), and a [privacy-notice template](docs/privacy-notice-template.md) for your retailers.

## Publish automatically

Your photos and product data probably live in a PIM, ERP, DAM or a shared folder already. Pick what fits, from no code at all to a small connector: [docs/automation.md](docs/automation.md).

- **Scheduled import:** in the admin, point the portal at a web address, SFTP folder or bucket with your `products.csv` and images; it picks up changes hourly, daily or weekly.
- **Command line:** `obx push --csv products.csv --images ./images` from any computer, cron job or CI pipeline (tool from [obx-go](https://github.com/openbrandexchange/obx-go)), or `node-go import …` on the server itself.
- **n8n, Make, Zapier or your own code:** plain HTTPS calls; repeating the same data changes nothing.

The spreadsheet format is one row per size, and images are named `{style}_{colour}_{position}.jpg`: see the [import format](https://github.com/openbrandexchange/spec/blob/main/docs/import-format.md).

## For developers

```sh
go test ./...                      # unit and HTTP integration tests (SQLite), incl. the 25-language check
e2e/run.sh                         # the admin and portal in a headless browser, axe on every screen
(cd conformance && go test ./...)  # the OBX conformance suite from obx-go (needs ../obx-go)
scripts/test-docker.sh             # the Docker image, a Litestream restore and a backup restore
```

Everything CI runs, how to release and how to add a migration: [docs/development.md](docs/development.md). Contributions are welcome under the Developer Certificate of Origin: [CONTRIBUTING.md](CONTRIBUTING.md), [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

- `internal/node`: the node (protocol, ingest, admin + portal API, imports, jobs, exports, webhooks, statistics, e-mail, audit log, backups).
- `internal/importfmt`: the canonical CSV and file-name parser; `internal/imaging`: pure-Go renditions.
- `ui/dist`: the web interface, vendored from [admin-ui](https://github.com/openbrandexchange/admin-ui) with `scripts/update-ui.sh` (`BUILD=1` rebuilds admin-ui first).
- API contracts: the [OBX spec](https://github.com/openbrandexchange/spec) (`openapi/*.yaml`). Implementation decisions: [NOTES.md](NOTES.md). Changes: [CHANGELOG.md](CHANGELOG.md).

The command line:

```
node-go serve                                   start the portal
node-go import --csv FILE [--assets FILE] [--images DIR|ZIP] [--package ZIP] [--mode mirror] [--dry-run]
node-go admin reset-password --email ADDRESS    prints a one-time link (or --password-stdin)
node-go admin create-ingest-token --name NAME   prints an automatic upload key
node-go admin audit-log [--limit N]             the latest admin actions
node-go admin personal-data --email ADDRESS     everything stored about a person (or --retailer ID)
node-go backup --to DIR | restore --from DIR    consistent backups (see docs/operations.md)
node-go check-config | healthcheck | version
```

Security problems: please report them privately to **security@openbrandexchange.com** or through a GitHub security advisory on this repository ([SECURITY.md](SECURITY.md)). Other questions: hello@openbrandexchange.com.

## License

[GNU Affero General Public License v3.0](LICENSE) (AGPL-3.0-only). You may run, change and host it freely; if you offer a changed version to others over a network, share your changes under the same licence. The embedded admin-ui is Apache-2.0; the spec fixtures in `testdata/spec` are CC-BY-4.0. See [NOTICE](NOTICE).

An initiative of Richard Lindhout · Open Brand Exchange Foundation (in formation). The project will be transferred to the foundation, a Dutch stichting, once it is set up ([governance](https://github.com/openbrandexchange/spec/blob/main/docs/governance.md)).

<sub>Works with Open Brand Exchange</sub>
