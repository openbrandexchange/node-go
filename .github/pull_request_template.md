## What and why

<!-- One logical change. Link the issue if there is one. -->

## Checks

- [ ] `gofmt -l .` is empty, `go vet ./...` and `go test ./...` pass
- [ ] Tests cover the new behaviour (HTTP in, HTTP out, database state)
- [ ] Schema changes are a new migration at the end of `internal/db/migrate.go` (forward-only), tested on SQLite and PostgreSQL
- [ ] Docs and `CHANGELOG.md` ("Unreleased") updated for user-visible changes
- [ ] Every commit is signed off (`git commit -s`, Developer Certificate of Origin)
