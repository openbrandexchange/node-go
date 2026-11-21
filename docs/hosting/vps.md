# A server for your portal: examples

Any Linux server (VPS) with a public IPv4 address works; the portal installs the same way on all of
them. Below is what to pick at four common providers. Prices and menus change: check them when you
choose. One brand with a few thousand articles runs comfortably on the smallest plan with 1–2 GB
memory; you can move up a size later.

On every provider:

1. Create the server with **Ubuntu 24.04 LTS** (or Debian 12/13), your SSH key, and a public IPv4
   address (IPv6 too if offered).
2. Allow incoming **TCP 22, 80 and 443** in the provider's firewall, if it has one.
3. Switch on the provider's **server backups** if offered.
4. Add the DNS record and run the installer, as in [linux.md](linux.md):

   ```sh
   ssh root@<server-ip>
   curl -fsSL https://openbrandexchange.com/install.sh | sudo sh -s -- --domain portal.yourbrand.com
   ```

## Hetzner Cloud (Germany, Finland)

[console.hetzner.cloud](https://console.hetzner.cloud) → your project → **Add Server**.

| Choice | Pick |
|---|---|
| Location | Nuremberg or Falkenstein (Germany), or Helsinki (Finland): all in the EU |
| Image | Ubuntu 24.04 |
| Type | the smallest shared-vCPU type with 2 GB memory, x86 (CX) or Arm (CAX); both work |
| Networking | public IPv4 and IPv6 |
| Firewalls | create one allowing TCP 22, 80 and 443 |
| Backups | on |

Large catalogs can keep the images in **Hetzner Object Storage** (S3-compatible) in the same
location: create a bucket and S3 credentials, then add to `/etc/node-go/node-go.env` **before the
first import** and restart:

```sh
OBX_BLOB_URL=s3://your-bucket/portal-images?endpoint=https://fsn1.your-objectstorage.com&region=fsn1
OBX_S3_ACCESS_KEY_ID=<access key>
OBX_S3_SECRET_ACCESS_KEY=<secret>
```

## DigitalOcean

[cloud.digitalocean.com](https://cloud.digitalocean.com) → **Create → Droplets**.

| Choice | Pick |
|---|---|
| Region | Amsterdam or Frankfurt for the EU |
| Image | Ubuntu 24.04 (LTS) x64 |
| Size | Basic, Regular, 1 or 2 GB memory |
| Authentication | SSH key |
| Advanced | enable IPv6 if you want it |
| Backups | on |

Firewall: **Networking → Firewalls → Create Firewall**, inbound SSH, HTTP and HTTPS, and apply it to
the droplet. Images in a bucket: DigitalOcean Spaces works as `s3://` with its endpoint.

## TransIP VPS (Netherlands)

[transip.nl](https://www.transip.nl) control panel → **VPS → Order VPS**.

| Choice | Pick |
|---|---|
| Location | Amsterdam (Netherlands) |
| Plan | the smallest BladeVPS / VPS with 2 GB memory |
| Operating system | Ubuntu 24.04 (install it from the control panel; add your SSH key) |

The VPS firewall (VPS → your VPS → **Firewall**) is off by default; when you switch it on, allow SSH,
HTTP and HTTPS. When the domain is at TransIP too, add the A record under **Domains → your domain →
DNS**.

## Scaleway (France, Netherlands, Poland)

[console.scaleway.com](https://console.scaleway.com) → **Instances → Create Instance**.

| Choice | Pick |
|---|---|
| Availability zone | Paris, Amsterdam or Warsaw |
| Type | a small shared instance (e.g. the "Development" or "Cost-Optimized" range) with 2 GB memory |
| Image | Ubuntu 24.04 |
| IP | a flexible public IPv4 (and IPv6) |

Security group: allow inbound TCP 22, 80 and 443 (the default group allows everything inbound; tighten
it if you like). Images in a bucket: Scaleway Object Storage works as `s3://` with its endpoint.

## Where your data is

With the server (and a bucket, if you use one) in an EU location, the catalog, the images and the
database are stored in the EU with that provider as hosting company. The e-mail provider you set
handles the e-mails, and Let's Encrypt only sees your domain name. For your GDPR records, conclude
the provider's data processing agreement in your account.
