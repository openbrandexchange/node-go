# Automatic publishing

The portal accepts the standard OBX upload API, so every recipe in the [OBX automation guide](https://github.com/openbrandexchange/spec/blob/main/docs/automation.md) works here unchanged: scheduled imports, `obx push` and `obx watch`, cron and the Windows Task Scheduler, GitHub Actions, n8n, Make and Zapier, and your own code.

What you need from the admin: **Automation → Automatic upload keys → New key**. Copy the key (it starts with `obxi_` and is shown once) and your portal address:

```sh
export OBX_URL=https://images.yourbrand.com
export OBX_TOKEN=obxi_…
```

On the server itself you can also create a key without the web interface:

```sh
node-go admin create-ingest-token --name "Nightly PIM export"
```

## The quickest start: a spreadsheet and a folder of photos

1. Download the template in the admin (**Imports → Download template**) or copy `testdata/spec/kestrel/products.csv`: one row per size, the product columns repeated on every row. Article data has its own columns (`materials` with `(recycled)`/`(organic)`, `contains_animal_origin`, `footwear_materials`, `certifications`, `item_subtype`, `feature:<key>`, `color_family`, `size_length` and measurement columns such as `weight_kg` or `strap_length_cm`); a column your file does not have leaves that field as it is in the portal.
2. Name photos `{style}_{colour}_{position}.jpg`, for example `KO-1001_NVY_1.jpg`; add `_back`, `_model`, `_detail` … to say what the photo shows. A photo that belongs to every colour of the style uses `ALL` as the colour: `KO-1001_ALL_5_detail.jpg`.
3. Upload both in **Imports**, or run from any computer:

   ```sh
   obx push --csv products.csv --images ./photos
   ```

   or on the server:

   ```sh
   node-go import --csv products.csv --images ./photos
   ```

Running it again only sends what changed; unchanged products and photos cause no work and no updates for retailers.

## Scheduled imports (no code)

**Imports → Scheduled imports → Add** reads from a web address (a CSV or a zip), an SFTP folder or an S3-compatible bucket (AWS S3, Cloudflare R2, MinIO, Backblaze). Choose hourly, daily at a time, weekly or manual. Credentials are stored encrypted and never shown again. Addresses in private networks are refused. Each run appears in the import history with its counts and row errors; unchanged files are skipped.

Choose **Mirror** to also take products offline that are no longer in the file. If a file would take more than half of a brand's products offline, the run stops instead, so a broken export never empties your portal.

## Being notified

**Automation → Webhooks** sends signed notifications when an import finished (`import_run.completed`), an upload batch finished (`ingest_job.completed`) or a retailer requested access (`access_request.created`). Point it at an n8n, Make or Zapier webhook trigger.

## For developers

- Upload API: `/obx/v1/ingest` ([ingest.md](https://github.com/openbrandexchange/spec/blob/main/docs/ingest.md), [obx-ingest.yaml](https://github.com/openbrandexchange/spec/blob/main/openapi/obx-ingest.yaml)).
- This portal renders web, square and thumbnail images itself (`renditions.server_generated: true`): clients only upload originals.
- Writes are idempotent and keyed by your own ids: product = style number, variant = EAN, image = `{style}_{colour}_{position}` (`{style}_ALL_{position}` for an image of every colour).
