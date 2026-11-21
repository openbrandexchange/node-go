# Put your portal online (your own Linux server)

One command on a Linux server gives you the portal at your own address, with HTTPS:

```sh
curl -fsSL https://openbrandexchange.com/install.sh | sudo sh -s -- --domain portal.yourbrand.com
```

Count on 15 minutes the first time, most of it waiting for DNS. Running the same command again
later updates the portal; your settings and data stay.

## What you need

- **A Linux server (VPS)** with a public IPv4 address: Ubuntu 22.04 or 24.04, Debian 12 or 13, or
  another distribution with systemd (Fedora, Rocky/Alma Linux, openSUSE). Intel/AMD or ARM.
  1 GB memory is enough to start; plan disk space for about twice the size of your original images.
  Any provider works: [examples with Hetzner, DigitalOcean, TransIP and Scaleway](vps.md).
- **Ports 80 and 443 open** to the internet (in the provider's firewall too, if it has one).
- **An address** such as `portal.yourbrand.com`: a subdomain of a domain you own, so you can add a
  DNS record for it.

## 1. Point your address at the server (DNS)

Add one record at the company that manages your domain:

| Type | Name | Value |
|---|---|---|
| A | `portal` | your server's IPv4 address, e.g. `203.0.113.10` |
| AAAA (only if the server has IPv6) | `portal` | your server's IPv6 address |

`portal` makes the address `portal.yourbrand.com`; choose another word if you like. Leave the TTL at
the default (or 1 hour). Where to click (the labels can differ slightly):

| Provider | Where |
|---|---|
| TransIP | Control panel → Domains → your domain → DNS → add a record |
| Hostnet | Mijn Hostnet → Domeinnamen → your domain → DNS beheren → add a record |
| Strato | Domains → Manage domain → gear icon → DNS → A record → Manage |
| one.com | Control panel → DNS settings → DNS records → Create new record |
| Cloudflare | your domain → DNS → Records → Add record. Set **Proxy status** to **DNS only** (grey cloud), so the portal gets its own certificate. Want the orange cloud later? Set SSL/TLS to **Full (strict)** first. |
| GoDaddy | My Products → your domain → DNS → Manage DNS → Add New Record |

Check it from your computer:

```sh
dig +short portal.yourbrand.com
```

or with an online DNS checker (search for "DNS checker"). It must show the server's address. That
usually takes 5 to 60 minutes, sometimes a few hours. The installer checks it too, and tells you
exactly which record is missing or wrong.

## 2. Run the installer

Log in to the server (`ssh root@203.0.113.10`, or with your own user and `sudo`) and run:

```sh
curl -fsSL https://openbrandexchange.com/install.sh | sudo sh -s -- --domain portal.yourbrand.com --email you@yourbrand.com
```

`--email` is optional: Let's Encrypt writes to it if something is wrong with the certificate.

The installer first checks the server and stops with a plain explanation when something is not
ready: Linux with systemd, enough disk space, the DNS record, ports 80 and 443 free, and the
firewall (with `ufw` or `firewalld` active it prints the exact command to open the ports). Then it:

1. downloads the newest release from GitHub and verifies it against the release's `checksums.txt`;
2. installs the program as `/usr/local/bin/node-go` and creates the system user `obx`;
3. writes the settings to `/etc/node-go/node-go.env`, with a generated secret key and setup code;
4. installs the `node-go` systemd service, which starts on boot and restarts after a failure. It runs
   as the unprivileged `obx` user, may open ports 80 and 443 and nothing else, and sees a read-only
   system except its data folder `/var/lib/node-go`;
5. starts it and opens `https://portal.yourbrand.com` once, so the certificate is fetched right away.

It ends with something like:

```
Open https://portal.yourbrand.com and enter this one-time setup code in the setup wizard:

    7KQM-X4TD-9PWN
```

Open the address, enter the code, your company name and your own password. Done.

Want to see first what it would do? Add `--dry-run`: it runs the checks, downloads and verifies the
release, and prints every step without changing anything. Read the script itself:
[install.sh](../../install.sh).

### HTTPS

The portal gets a free certificate from Let's Encrypt itself and renews it automatically; there is
no separate web server to set up. Port 80 only answers Let's Encrypt and redirects visitors to
`https://`. Certificates are kept in `/var/lib/node-go/acme`. The settings behind it
(`OBX_DOMAIN`, `OBX_TLS=auto`, `OBX_ACME_EMAIL`) are in [configuration.md](../configuration.md#https).

## Afterwards

| To | Do |
|---|---|
| Update to the newest release | run the same install command again (the `--domain` may be left out) |
| Install a specific release | add `--version 0.4.0` |
| Change a setting (e-mail, storage, ...) | `sudo nano /etc/node-go/node-go.env`, then `sudo systemctl restart node-go` |
| See whether it runs | `sudo systemctl status node-go` |
| Read the log | `sudo journalctl -u node-go -f` |
| Find the setup code again | `sudo grep OBX_SETUP_CODE /etc/node-go/node-go.env` |
| Reset an admin password | `sudo sh -c 'set -a; . /etc/node-go/node-go.env; exec runuser -u obx -- node-go admin reset-password --email you@yourbrand.com'` |
| Remove the portal | `curl -fsSL https://openbrandexchange.com/install.sh \| sudo sh -s -- --uninstall` (keeps settings and data; add `--purge` to delete them too) |

Keep a copy of `OBX_SECRET_KEY` from the settings file in your password manager: it encrypts stored
import passwords and signs download links, and cannot be changed once there is data.

E-mail (invitations, "your download is ready"): add your mail provider's SMTP server to the settings
file, then restart:

```sh
OBX_SMTP_URL=smtp://user:password@smtp.example.com:587
OBX_MAIL_FROM=portal@yourbrand.com
```

Without it the portal works; the admin shows invitation links to copy.

## Backups

A nightly consistent copy while the portal runs, kept for 14 days:

```sh
# /etc/cron.d/node-go-backup
15 3 * * * root sh -c 'set -a; . /etc/node-go/node-go.env; exec runuser -u obx -- /usr/local/bin/node-go backup --to /var/lib/node-go/backups/'"$(date +\%F)" >/dev/null && find /var/lib/node-go/backups -mindepth 1 -maxdepth 1 -mtime +14 -exec rm -rf {} +
```

Copy the backups off the server now and then (`scp -r`, `rclone` to a bucket), and switch on your
provider's server backups if it has them. Restoring and testing a restore:
[operations.md](../operations.md#backups). Large catalogs can keep the images in a bucket instead of
on the disk (`OBX_BLOB_URL`, see [configuration.md](../configuration.md)); decide that before the
first import.

## Behind your own web server or proxy

Already running nginx, Apache, Caddy or a load balancer on the server, or HTTPS terminated elsewhere?
Install with `--behind-proxy`: the portal then serves plain http on port 8080 and trusts the proxy's
`X-Forwarded-For` (`OBX_TRUST_PROXY=true`).

```sh
curl -fsSL https://openbrandexchange.com/install.sh | sudo sh -s -- --domain portal.yourbrand.com --behind-proxy
```

Let the proxy forward `portal.yourbrand.com` to `http://127.0.0.1:8080`, allow request bodies up to
2 GB and long timeouts (30 minutes) for large uploads and zip downloads. For example with nginx:

```nginx
server {
    server_name portal.yourbrand.com;
    client_max_body_size 2g;
    proxy_read_timeout 30m;
    proxy_send_timeout 30m;
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
    # listen 443 ssl; plus your certificate, e.g. from certbot --nginx
}
```

Make sure port 8080 is not reachable from the internet (only the proxy should talk to it).

Installed earlier with `deploy/install-linux.sh` and Caddy? Running the installer keeps that setup
(it sees `OBX_TRUST_PROXY=true`). To switch to the built-in HTTPS: `sudo apt remove caddy`, set
`OBX_TLS=auto`, `OBX_DOMAIN=portal.yourbrand.com` and `OBX_TRUST_PROXY=false` in the settings file
(remove `PORT` and `OBX_PUBLIC_URL`), and run the installer again.

## Without the installer

The installer only does what is described above, so you can do it by hand: download
`node-go_<version>_linux_amd64.tar.gz` (or `_arm64`) and `checksums.txt` from the
[releases](https://github.com/openbrandexchange/node-go/releases), check with
`sha256sum -c --ignore-missing checksums.txt`, and run `node-go serve` with the settings from
[configuration.md](../configuration.md) under a service manager of your choice. A server without
access to GitHub can use `--archive node-go_<version>_linux_amd64.tar.gz` with a file you copied there.

## When something does not work

| You see | Do |
|---|---|
| "has no A record yet" / "points to ..., which is not this server" | Check the DNS record (step 1) and wait; `dig +short portal.yourbrand.com` must show this server. |
| "has an AAAA (IPv6) record but this server has no IPv6 address" | Delete the AAAA record, or put the server's IPv6 address in it. |
| "port 80 is in use by nginx" (or apache2) | Stop it (`sudo systemctl disable --now nginx`) if the server is only for the portal, or use `--behind-proxy`. |
| "https://... does not answer yet" at the end | Usually DNS not ready or ports closed in the provider's firewall. The portal retries by itself when someone opens it; `sudo journalctl -u node-go -n 50` shows what Let's Encrypt said. |
| The portal does not start | `sudo journalctl -u node-go -n 50`: a wrong setting is named there. `node-go check-config` checks the settings. |

More about running it: [operations.md](../operations.md), [upgrading.md](../upgrading.md),
[security.md](../security.md).
