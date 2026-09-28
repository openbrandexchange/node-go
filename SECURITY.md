# Security policy

## Reporting a vulnerability

Please report security problems privately, never in a public issue or pull request:

- e-mail **security@openbrandexchange.com**, or
- open a private report under **Security → Report a vulnerability** on this GitHub repository.

Include what you found, the affected version (`node-go version`, `GET /readyz` or the
`obx_build_info` metric), the steps to reproduce it and the impact you expect. Please do not access
data that is not yours, and give us time to fix the problem before you disclose it.

## What happens next

| When | What we do |
|---|---|
| within 3 working days | confirm that we received the report |
| within 10 working days | tell you whether we can reproduce it and how serious we think it is |
| within 90 days (sooner for critical issues) | release a fix and publish an advisory, crediting you if you wish |

We coordinate the date of public disclosure with you. There is no bug bounty.

## Supported versions

Security fixes are made for the latest minor release (0.1.x today). Brands running their own portal
should update when a release says it contains a security fix ([docs/upgrading.md](docs/upgrading.md)).

## Scope

This repository: the portal code, the Docker image, the release archives and the deployment
templates (`deploy/`, `docker-compose.yml`, Cloud Run, Fly.io, Railway, Render). Problems in the
protocol itself go to the spec repository's private reporting in the same way; problems in the
admin screens to the admin-ui repository. When in doubt, e-mail us and we route it.

How the portal protects data is described in [docs/security.md](docs/security.md).
