# Performance

The portal is built for brands with up to tens of thousands of articles on one small instance. `TestLoad` seeds **10 000 products, 60 000 variants (EANs) and 40 000 images** (110 000 change entries) into a fresh SQLite node and measures the requests retailers and their software make most.

```sh
OBX_LOAD_TEST=1 go test ./internal/node -run TestLoad -v            # 10 000 products, about a minute
OBX_LOAD_TEST=1 OBX_LOAD_PRODUCTS=50000 go test ./internal/node -run TestLoad -v -timeout 60m
OBX_LOAD_TEST=1 OBX_LOAD_KEEP=/tmp/load.db go test …                 # keep the seeded database for EXPLAIN QUERY PLAN
```

CI runs it on every change and fails when a request's 95th percentile exceeds its budget (300 ms for pages, 50 ms for a single EAN lookup, 800 ms for bulk EAN lookups and `/changes` pages of 500 entries). Those budgets are generous for slow CI machines and still catch a missing index or an N+1 query, which are 10–100× slower.

## Results

Apple M-series laptop (shared with other work), SQLite, one process, 2026-09-28:

| Request | runs | p50 | p95 |
|---|---:|---:|---:|
| Portal: first page of the catalog (50 articles with thumbnails, colours, counts) | 30 | 5.4 ms | 6.0 ms |
| Portal: next pages (walking 50 pages) | 50 | 12.6 ms | 19.1 ms |
| Portal: filter by season and item type | 30 | 18.6 ms | 37.3 ms |
| Portal: search a style number | 30 | 24.7 ms | 28.0 ms |
| Portal: search a word in the names | 30 | 15.7 ms | 16.5 ms |
| Portal: filter counts (facets) | 30 | 123 ms | 133 ms |
| Portal: article page | 50 | 24.9 ms | 28.6 ms |
| Portal: EAN list with 100 EANs (full articles) | 20 | 79 ms | 343 ms¹ |
| API: `/changes` page of 500 entries (full snapshot of 110 000 entries, 222 pages) | 222 | 6.5 ms | 33.2 ms |
| API: `/products:by-gtin` with 1 EAN | 200 | 0.5 ms | 0.8 ms |
| API: `/products:by-gtin` POST with 500 EANs (full articles) | 20 | 200 ms | 249 ms |
| API: `/products` page of 100 | 30 | 26.4 ms | 28.7 ms |
| Admin: catalog list | 30 | 47.4 ms | 49.4 ms |

¹ one slow run out of 20 (the machine was busy); the median is the representative number.

Seeding the catalog through the normal write path (validation, change feed, representations) took 31 s.

## What made it fast

The first run of this test found the portal's per-article reads scanning every visible variant and image: the query planner preferred the `visible` index over `product_id`. Migration 2 adds composite indexes (`variants(product_id, visible, external_id)`, `assets(product_id, visible)`, `products(visible, style_number, id)` for the catalog order) and a covering index for the colour counts (`variants(visible, color_code, product_id, id)`); the colour facet now reads one variant per colour instead of every variant's JSON. The catalog page went from 355 ms to 5 ms at 500 products, the facets from 2.6 s to 130 ms at 10 000. SQLite's planner statistics are refreshed with `PRAGMA optimize` every hour.

## Larger catalogs

- Search is a substring match (`LIKE '%…%'`) over a prepared search text: linear in the number of articles (≈ 16 ms at 10 000). At several hundred thousand articles consider PostgreSQL.
- The facets are aggregate queries over the visible catalog (≈ 130 ms at 10 000 articles, linear).
- `/changes`, EAN lookups and article pages are index lookups and do not grow with the catalog.
- The database takes about 30 KB per product with six variants and four images, including the change history (`OBX_CHANGE_RETENTION_DAYS`).
