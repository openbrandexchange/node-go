# Contributing

Thank you for helping. This is the Go edition of the Open Brand Exchange portal ("Portal for
Google Cloud & Docker"). Everyone taking part follows the [Code of Conduct](CODE_OF_CONDUCT.md).

## Before you start

- Bugs and ideas: open an issue with the templates. Security problems: see [SECURITY.md](SECURITY.md),
  never a public issue.
- Protocol changes start in [openbrandexchange/spec](https://github.com/openbrandexchange/spec); this
  repository follows the spec, and all three portal editions behave the same.
- Admin screens live in [admin-ui](https://github.com/openbrandexchange/admin-ui) and are vendored here
  with `scripts/update-ui.sh`.

## Development

```sh
go vet ./... && go test ./...                    # unit and HTTP integration tests (SQLite)
OBX_TEST_DATABASE_URL=postgres://… go test ./internal/node   # the same on PostgreSQL
(cd conformance && go test ./...)                # the OBX conformance suite (needs ../obx-go)
e2e/run.sh                                       # browser tests incl. axe (needs Node.js)
scripts/test-docker.sh                           # the image, Litestream restore, backup/restore
```

The full list, with what CI runs, is in [docs/development.md](docs/development.md).

- Tests test behaviour (HTTP in, HTTP out, database state), never source text.
- Schema changes are a new entry at the end of `internal/db/migrate.go` (forward-only: never edit a
  released migration). Both SQLite and PostgreSQL must run it.
- Code, comments, commit messages and docs are English; user-facing texts come from admin-ui in 25
  languages and follow the [glossary](https://github.com/openbrandexchange/spec/blob/main/docs/glossary.md).
- Keep dependencies few; every module compiled into the binary must pass `scripts/check-licenses.sh`.

## Developer Certificate of Origin

Contributions are accepted under the [Developer Certificate of Origin 1.1](https://developercertificate.org/).
Sign off every commit to say you wrote it or have the right to submit it under this repository's
licence (AGPL-3.0-only):

```sh
git commit -s -m "Explain the change"
```

This adds `Signed-off-by: Your Name <you@example.com>`. You keep the copyright in your contribution.
There is no contributor licence agreement.

## Pull requests

- One logical change per pull request, with tests and docs updated.
- CI must be green: format, vet, staticcheck, tests (SQLite and PostgreSQL), race detector,
  vulnerability and licence checks, conformance, browser tests with axe, Docker and deploy checks,
  load test.
- User-visible changes get a line in `CHANGELOG.md` under "Unreleased".
