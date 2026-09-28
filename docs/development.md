# Development

## Layout

| Path | What |
|---|---|
| `main.go` | the `node-go` command (serve, import, admin, backup, restore, check-config, healthcheck, version) |
| `internal/node` | the node: OBX protocol, ingest, admin + portal API, imports, jobs, exports, webhooks, statistics, e-mail, audit log, personal data, backups, metrics |
| `internal/db` | SQLite/PostgreSQL wrapper and the forward-only migrations |
| `internal/config` | settings from the environment, validated at start |
| `internal/importfmt`, `internal/imaging`, `internal/gtin` | canonical CSV and file names, pure-Go renditions, GTIN rules |
| `internal/safehttp` | outbound requests without SSRF |
| `internal/blobstore` | local disk and S3-compatible storage |
| `ui/dist` | the vendored admin-ui build (`scripts/update-ui.sh`) |
| `conformance/` | the OBX conformance suite against an in-process node (separate module; needs `../obx-go`) |
| `e2e/` | Playwright: every task in the browser, axe on every screen, keyboard-only tasks |
| `deploy/`, `docker-compose*.yml`, `Dockerfile`, `fly.toml`, `render.yaml`, `railway.json` | hosting |
| `scripts/` | checks and tests of the hosting (see below) |

## Checks

CI (`.github/workflows/ci.yml`) runs all of these on every push and pull request; run them locally before you push.

| Check | Command |
|---|---|
| format, vet, staticcheck | `test -z "$(gofmt -l .)" && go vet ./... && go run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./...` |
| shell scripts | `shellcheck deploy/*.sh scripts/*.sh e2e/run.sh` |
| tests (SQLite) | `go test ./...` |
| tests (PostgreSQL) | `OBX_TEST_DATABASE_URL=postgres://obx:obx@127.0.0.1:5432/obx?sslmode=disable go test ./internal/node` |
| race detector | `go test -race ./internal/...` (slow: the SQLite driver is pure Go) |
| 25 languages complete | `go test ./ui` |
| vulnerabilities | `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`; `cd e2e && npm audit --omit=dev` |
| dependency licences | `scripts/check-licenses.sh` |
| conformance | `(cd conformance && go test ./...)` in-process; CI also runs the `obx-conformance` binary against a running `node-go serve` (with `--rate-limit-test`) |
| browser tests with axe | `e2e/run.sh` |
| load test | `OBX_LOAD_TEST=1 go test ./internal/node -run TestLoad -v` ([performance.md](performance.md)) |
| Docker image, Litestream restore, backup/restore | `scripts/test-docker.sh` |
| Compose + Caddy over HTTPS | `scripts/test-compose-https.sh` |
| Cloud Run script against the real gcloud | `scripts/test-cloudrun.sh` |
| release build | `goreleaser release --snapshot --clean` (or `docker run --rm -v "$PWD":/src -w /src goreleaser/goreleaser:v2.18.2 release --snapshot --clean`) |

The conformance jobs need the private `obx-go` repository while the repositories are private: add a repository secret **`OBX_READ_TOKEN`** (a fine-grained token with read access to `openbrandexchange/obx-go` contents). Without it those jobs print a notice and are skipped.

## Releases

1. Move the "Unreleased" entries in `CHANGELOG.md` under a new version heading with the date.
2. Commit, then tag: `git tag -s v0.1.1 -m v0.1.1 && git push origin v0.1.1`.
3. `.github/workflows/release.yml` then builds the binaries (GoReleaser: Linux, macOS, Windows × amd64, arm64, with `checksums.txt`, an SBOM per archive and the dependency licences), a draft GitHub release with the changelog entry, and the multi-arch Docker image `ghcr.io/openbrandexchange/node-go:<version>` (and `:<major>.<minor>`, `:latest`) with SBOM and provenance attestations.
4. Check the draft release and publish it.

## Migrations

Append a new SQL string to `migrations` in `internal/db/migrate.go`; never edit one that was released. Use `{{SERIAL}}` for an auto-increment key and plain SQL that both SQLite and PostgreSQL accept. Test on both (`OBX_TEST_DATABASE_URL`). The schema version check and `node-go backup` use the number of migrations.
