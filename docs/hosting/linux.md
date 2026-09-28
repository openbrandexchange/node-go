# A Linux server without Docker (systemd)

On a Debian or Ubuntu server with a domain pointing at it:

```sh
git clone https://github.com/openbrandexchange/node-go && cd node-go
sudo ./deploy/install-linux.sh --domain images.yourbrand.com --with-caddy
```

The script:

- builds `node-go` (needs Go 1.26; or pass `--binary /path/to/node-go` with a binary built elsewhere: `CGO_ENABLED=0 GOOS=linux go build -o node-go .`),
- creates the system user `obx` and the data folder `/var/lib/node-go`,
- writes `/etc/node-go/node-go.env` with a generated `OBX_SECRET_KEY` (edit it for e-mail or PostgreSQL),
- installs and starts the hardened `node-go` systemd service (`deploy/systemd/node-go.service`),
- with `--with-caddy`: installs Caddy and configures automatic HTTPS for your domain.

It prints the setup code at the end (also visible with `journalctl -u node-go | grep "setup code"`).

Useful commands:

```sh
sudo systemctl status node-go
sudo journalctl -u node-go -f
sudo -u obx env $(sudo cat /etc/node-go/node-go.env | xargs) node-go admin reset-password --email you@yourbrand.com
```

Updating: `git pull && sudo ./deploy/install-linux.sh --domain images.yourbrand.com` (keeps the env file and data).

Behind another reverse proxy (nginx, Apache, a load balancer): proxy to `127.0.0.1:8080`, allow request bodies up to 2 GB and long timeouts for zip downloads, and keep `OBX_TRUST_PROXY=true`.
