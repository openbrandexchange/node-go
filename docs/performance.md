# Performance

The portal is built for brands with up to tens of thousands of articles on one small instance. `TestLoad` seeds **10 000 products, 60 000 variants (EANs) and 40 000 images** (110 000 change entries) into a fresh SQLite node and measures the requests retailers and their software make most.

```sh
OBX_LOAD_TEST=1 go test ./internal/node -run TestLoad -v            # 10 000 products, about a minute
OBX_LOAD_TEST=1 OBX_LOAD_PRODUCTS=50000 go test ./internal/node -run TestLoad -v -timeout 60m
OBX_LOAD_TEST=1 OBX_LOAD_KEEP=/tmp/load.db go test …                 # keep the seeded database for EXPLAIN QUERY PLAN
```

CI runs it on every change and fails when a request's 95th percentile exceeds its budget (300 ms for pages, 50 ms for a single EAN lookup, 800 ms for bulk EAN lookups and `/changes` pages of 500 entries). Those budgets are generous for slow CI machines and still catch a missing index or an N+1 query, which are 10–100× slower.

## Results

Apple M-series laptop (shared with other work, so maxima are noisy), one node process, 2026-09-28. SQLite is the default; PostgreSQL 17 ran in Docker on the same machine (`OBX_TEST_DATABASE_URL=… OBX_LOAD_TEST=1 go test …`).

| Request | runs | SQLite p50 | SQLite p95 | PostgreSQL p50 | PostgreSQL p95 |
|---|---:|---:|---:|---:|---:|
| Portal: first page of the catalog (50 articles with thumbnails, colours, counts) | 30 | 5.5 ms | 10.5 ms | 48.0 ms | 54.5 ms |
| Portal: next pages (walking 50 pages) | 50 | 9.7 ms | 13.8 ms | 50.6 ms | 58.0 ms |
| Portal: filter by season and item type | 30 | 18.6 ms | 28.7 ms | 51.1 ms | 60.0 ms |
| Portal: search a style number | 30 | 26.6 ms | 28.5 ms | 19.7 ms | 23.1 ms |
| Portal: search a word in the names | 30 | 17.5 ms | 29.9 ms | 51.8 ms | 57.0 ms |
| Portal: filter counts (facets) | 30 | 129 ms | 217 ms | 67.7 ms | 74.4 ms |
| Portal: article page | 50 | 27.4 ms | 32.5 ms | 23.2 ms | 27.1 ms |
| Portal: EAN list with 100 EANs (full articles) | 40 | 81.3 ms | 166 ms | 30.1 ms | 33.9 ms |
| API: `/changes` page of 500 entries (full snapshot of 110 000 entries, 222 pages) | 222 | 6.4 ms | 33.0 ms | 9.3 ms | 35.1 ms |
| API: `/products:by-gtin` with 1 EAN | 200 | 0.5 ms | 0.9 ms | 2.5 ms | 4.1 ms |
| API: `/products:by-gtin` POST with 500 EANs (full articles) | 20 | 185 ms | 239 ms | 131 ms | 147 ms |
| API: `/products` page of 100 | 30 | 27.0 ms | 31.5 ms | 28.4 ms | 33.8 ms |
| Admin: catalog list | 30 | 48.1 ms | 51.6 ms | 92.8 ms | 99.9 ms |

Seeding the catalog through the normal write path (validation, change feed, representations) took 23 s on SQLite and 10.5 minutes on PostgreSQL (every write is a network round trip; imports are the same work and run in the background).

## What made it fast

The first run of this test found the portal's per-article reads scanning every visible variant and image: the query planner preferred the `visible` index over `product_id`. Migration 2 adds composite indexes (`variants(product_id, visible, external_id)`, `assets(product_id, visible)`, `products(visible, style_number, id)` for the catalog order) and a covering index for the colour counts (`variants(visible, color_code, product_id, id)`); the colour facet now reads one variant per colour instead of every variant's JSON. The catalog page went from 355 ms to 9 ms at 500 products, the facets from 2.6 s to 130 ms at 10 000. SQLite's planner statistics are refreshed with `PRAGMA optimize` every hour.

## Larger catalogs

- Search is a substring match (`LIKE '%…%'`) over a prepared search text: linear in the number of articles (≈ 20–50 ms at 10 000).
- The facets are aggregate queries over the visible catalog (≈ 70–130 ms at 10 000 articles, linear).
- `/changes`, EAN lookups and article pages are index lookups and do not grow with the catalog.
- The database takes about 30 KB per product with six variants and four images, including the change history (`OBX_CHANGE_RETENTION_DAYS`).
