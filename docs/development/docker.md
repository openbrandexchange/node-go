# Docker (local development only)

> **Not for production.** To put a portal online, use the installer on a Linux server:
> [docs/hosting/linux.md](../hosting/linux.md). The Docker files are kept for development and for
> the tests that CI runs.

The fastest local start needs no Docker at all: `go run . dev` (see the README). Docker is handy to
test the image, Litestream or the Caddy setup.

## Try the image locally

```sh
docker compose -f docker-compose.local.yml up
```

Serves <http://localhost:8080> with the published image (`ghcr.io/openbrandexchange/node-go`) and writes
e-mails to the output. Add `-f docker-compose.build.yml --build` to build the image from this folder.
Data lives in the volume `obx-local-data`.

## Compose with Caddy over HTTPS (as CI tests it)

`docker-compose.yml` runs the node behind Caddy. `scripts/test-compose-https.sh` starts it with
`DOMAIN=localhost` (Caddy's local certificate authority), then checks HTTPS, the security headers, the
redirect from http, a complete setup and import through Caddy, and that a restart keeps the data.

| Container | Does |
|---|---|
| `node` | the portal; data in the volume `obx-data` (`/data`); 1 GB memory and 2 CPUs; JSON logs rotated at 5 × 20 MB |
| `caddy` | HTTPS on 80/443, certificates in the volume `caddy-data`, forwards to the node, health checks `/readyz` |

Settings: `DOMAIN`, `ADMIN_EMAIL`, `HTTP_PORT`/`HTTPS_PORT`, `OBX_VERSION` (image tag) and the `OBX_…`
and `LITESTREAM_…` variables of [configuration.md](../configuration.md), in a `.env` file next to the
Compose file (`cp .env.example .env`).

## The image

- Multi-arch (linux/amd64, linux/arm64), built by CI (`docker` job) and pushed by the release workflow.
- `scripts/test-docker.sh`: a plain container, a Litestream replica restored into a new container
  with an empty disk, and `node-go backup` restored into a new volume, each followed by a smoke test.
- Litestream runs only when `LITESTREAM_REPLICA_URL` is set; the `HEALTHCHECK` uses
  `node-go healthcheck`.

Useful commands:

```sh
docker compose logs node | grep "setup code"
docker compose exec node node-go backup --to /data/backups/$(date +%F)
docker compose exec node node-go admin reset-password --email you@yourbrand.example
```
