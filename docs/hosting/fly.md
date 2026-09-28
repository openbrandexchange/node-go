# Fly.io

```sh
git clone https://github.com/openbrandexchange/node-go && cd node-go
fly auth login
./deploy/fly.sh your-portal ams
```

The script creates the app and a 10 GB volume (`obx_data`, mounted at `/data`), stores a generated `OBX_SECRET_KEY` as a Fly secret (only the first time), deploys, and prints the setup code. The portal is then at `https://your-portal.fly.dev` (the public address is detected automatically; set `OBX_PUBLIC_URL` when you add your own domain with `fly certs add images.yourbrand.com`).

Keep one machine (`min_machines_running = 1` in `fly.toml`): the database is SQLite on the volume. Updates: `git pull && fly deploy` ([upgrading.md](../upgrading.md)); snapshots: `fly ssh console -C "node-go backup --to /data/backups/$(date +%F)"` and `fly ssh sftp get` ([operations.md](../operations.md#backups)). For backups, add Litestream to a bucket: `fly secrets set LITESTREAM_REPLICA_URL=s3://bucket/portal-db AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=…`, or use Fly Postgres with `OBX_DATABASE_URL`.
