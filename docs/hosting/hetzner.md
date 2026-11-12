# Your portal on Hetzner (Germany or Finland)

A brand portal on a Hetzner Cloud server in the EU, with Docker, automatic HTTPS and backups. About
30 minutes. This guide uses only what is in this repository: `docker-compose.yml` (the portal and
Caddy for HTTPS), `.env.example` and `node-go backup`. The same compose file is started and checked
end to end by `scripts/test-compose-https.sh` in CI.

What you need: a Hetzner account, a domain name you can add DNS records to (for example
`images.yourbrand.com`), and an SSH key on your computer.

## 1. Create the server

In the [Hetzner Cloud Console](https://console.hetzner.cloud): **Add Server**.

| Choice | Pick |
|---|---|
| Location | Nuremberg or Falkenstein (Germany), or Helsinki (Finland). All three are in the EU. |
| Image | Ubuntu (the latest LTS), or the **Docker CE** app image, which has Docker already installed |
| Type | the smallest shared-vCPU type with at least 2 GB memory, x86 (CX) or Arm (CAX); both work, the portal image is built for amd64 and arm64 |
| Networking | public IPv4 and IPv6 |
| SSH key | add yours |
| Firewall | create one that allows incoming TCP 22, 80 and 443 (and UDP 443 for HTTP/3) |
| Backups | switch on: Hetzner then keeps daily copies of the whole server |

Prices change; check them on hetzner.com when you choose. One brand with a few thousand articles
runs comfortably on the smallest type; move up a size later in the console if needed.

## 2. Point your domain at the server

At your DNS provider, add an `A` record for `images.yourbrand.com` with the server's IPv4 address
and an `AAAA` record with its IPv6 address. Check from your computer:

```sh
dig +short images.yourbrand.com
```

It must show the server's address before you go on: the HTTPS certificate is issued only then.

## 3. Install Docker (skip with the Docker CE image)

```sh
ssh root@<server-ip>
curl -fsSL https://get.docker.com | sh
```

## 4. Start the portal

```sh
git clone --branch v0.2.1 https://github.com/openbrandexchange/node-go && cd node-go
cp .env.example .env
sed -i "s|^OBX_SECRET_KEY=.*|OBX_SECRET_KEY=$(openssl rand -base64 48 | tr -d '\n')|" .env
nano .env        # set DOMAIN=images.yourbrand.com and ADMIN_EMAIL=you@yourbrand.com
docker compose up -d
```

The first start builds the portal from source; on the smallest server that takes a few minutes.
Caddy then gets a Let's Encrypt certificate for your domain and redirects http to https.

Keep a copy of `OBX_SECRET_KEY` from `.env` in your password manager: it encrypts stored
import passwords and signs download links, and cannot be changed once there is data.

Get the one-time setup code and open the portal:

```sh
docker compose logs node | grep "setup code"
```

Open `https://images.yourbrand.com`, enter the code, your company name and your password.

## 5. Images in Hetzner Object Storage (optional)

By default the database and the images are on the server's disk, which is fine to start. For a
large catalog, keep the images in a bucket instead. Decide **before you import**: images already
stored on disk are not moved.

1. In the console: **Object Storage → Create bucket**, in the same location as the server
   (`fsn1` Falkenstein, `nbg1` Nuremberg or `hel1` Helsinki). Keep it private.
2. Under **Security → S3 credentials**, create an access key and secret.
3. Add to `.env`, with your bucket name and location:

   ```sh
   OBX_BLOB_URL=s3://your-bucket/portal-images?endpoint=https://fsn1.your-objectstorage.com&region=fsn1
   OBX_S3_ACCESS_KEY_ID=<access key>
   OBX_S3_SECRET_ACCESS_KEY=<secret>
   ```

4. `docker compose up -d` to apply.

## 6. E-mail (optional)

For invitations and "your download is ready" messages, set an SMTP server in `.env`, for example
your own mail provider's:

```sh
OBX_SMTP_URL=smtp://user:password@smtp.example.com:587
OBX_MAIL_FROM=portal@yourbrand.com
```

Without it the portal works; the admin shows invitation links to copy.

## 7. Backups

Two layers:

- **Hetzner Backups** (step 1) copy the whole server daily. Restore from the console.
- **A nightly portal backup** makes a consistent copy while the portal runs, which you can restore
  on any server. Add it to root's crontab (`crontab -e`):

  ```cron
  30 2 * * * cd /root/node-go && docker compose exec -T node node-go backup --to /data/backups/$(date +\%F) && docker compose exec -T node find /data/backups -mindepth 1 -maxdepth 1 -mtime +14 -exec rm -rf {} +
  ```

  The backup is inside the data volume; copy it off the server now and then:
  `docker compose cp node:/data/backups/<date> ./` and then `scp` it to your computer or a bucket.
  Images in Object Storage (step 5) are not in this backup; the bucket keeps them.

How to restore, and how to test a restore: [operations.md](../operations.md#backups).

## 8. Updates

```sh
cd /root/node-go
git fetch --tags && git checkout v0.2.1     # the release you want; see the releases page
docker compose up -d --build
```

The database migrates itself on start. Read the release notes first: [upgrading.md](../upgrading.md).

## Where your data is

With the server and the bucket in Nuremberg, Falkenstein or Helsinki, the catalog, the images and
the database are stored in the EU, with Hetzner Online GmbH (Germany) as the hosting provider.
Parts you add yourself decide the rest: the e-mail provider you set in step 6 handles the e-mails,
and Let's Encrypt (the certificate authority) only sees your domain name. For your GDPR records, conclude Hetzner's data processing agreement in your account.
