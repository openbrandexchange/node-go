package db

import (
	"context"
	"fmt"
	"strings"
)

// migrations are applied in order; never edit a released one, append.
var migrations = []string{
	// 1: initial schema
	`
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);

CREATE TABLE brands (
  id TEXT PRIMARY KEY, external_id TEXT NOT NULL UNIQUE, status TEXT NOT NULL, name TEXT NOT NULL,
  data TEXT NOT NULL, etag TEXT NOT NULL, visible INTEGER NOT NULL DEFAULT 0, rep TEXT NOT NULL DEFAULT '{}',
  rep_hash TEXT NOT NULL DEFAULT '', last_seq BIGINT NOT NULL DEFAULT 0, created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL, record_updated_at TEXT NOT NULL);

CREATE TABLE products (
  id TEXT PRIMARY KEY, external_id TEXT NOT NULL UNIQUE, brand_id TEXT NOT NULL, status TEXT NOT NULL,
  data TEXT NOT NULL, etag TEXT NOT NULL, visible INTEGER NOT NULL DEFAULT 0, rep TEXT NOT NULL DEFAULT '{}',
  rep_hash TEXT NOT NULL DEFAULT '', last_seq BIGINT NOT NULL DEFAULT 0, created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL, record_updated_at TEXT NOT NULL, touched_at TEXT NOT NULL,
  style_number TEXT NOT NULL, name_sort TEXT NOT NULL DEFAULT '', item_type TEXT, target_group TEXT,
  season TEXT, collection TEXT, search_text TEXT NOT NULL DEFAULT '');
CREATE INDEX products_brand ON products(brand_id);
CREATE INDEX products_visible ON products(visible, id);
CREATE INDEX products_style ON products(style_number);

CREATE TABLE variants (
  id TEXT PRIMARY KEY, external_id TEXT NOT NULL UNIQUE, product_id TEXT NOT NULL, brand_id TEXT NOT NULL,
  status TEXT NOT NULL, data TEXT NOT NULL, etag TEXT NOT NULL, visible INTEGER NOT NULL DEFAULT 0,
  rep TEXT NOT NULL DEFAULT '{}', rep_hash TEXT NOT NULL DEFAULT '', last_seq BIGINT NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL, record_updated_at TEXT NOT NULL,
  gtin TEXT UNIQUE, color_code TEXT NOT NULL, sku TEXT);
CREATE INDEX variants_product ON variants(product_id);
CREATE INDEX variants_visible ON variants(visible, id);

CREATE TABLE assets (
  id TEXT PRIMARY KEY, external_id TEXT NOT NULL UNIQUE, product_id TEXT NOT NULL, brand_id TEXT NOT NULL,
  status TEXT NOT NULL, data TEXT NOT NULL, etag TEXT NOT NULL, visible INTEGER NOT NULL DEFAULT 0,
  rep TEXT NOT NULL DEFAULT '{}', rep_hash TEXT NOT NULL DEFAULT '', last_seq BIGINT NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL, record_updated_at TEXT NOT NULL,
  color_code TEXT, position INTEGER NOT NULL, original_sha TEXT, live_files TEXT, pending INTEGER NOT NULL DEFAULT 0,
  expires_at TEXT);
CREATE INDEX assets_product ON assets(product_id);
CREATE INDEX assets_visible ON assets(visible, id);
CREATE INDEX assets_original ON assets(original_sha);
CREATE INDEX assets_expires ON assets(expires_at);

CREATE TABLE blobs (sha256 TEXT PRIMARY KEY, bytes BIGINT NOT NULL, content_type TEXT NOT NULL, width INTEGER,
  height INTEGER, created_at TEXT NOT NULL);
CREATE TABLE renditions (original_sha TEXT NOT NULL, name TEXT NOT NULL, sha256 TEXT NOT NULL, width INTEGER NOT NULL,
  height INTEGER NOT NULL, bytes BIGINT NOT NULL, PRIMARY KEY (original_sha, name));
CREATE TABLE render_errors (original_sha TEXT PRIMARY KEY, error TEXT NOT NULL, at TEXT NOT NULL);

CREATE TABLE changes (seq {{SERIAL}}, type TEXT NOT NULL, op TEXT NOT NULL, object_id TEXT NOT NULL,
  external_id TEXT, brand_id TEXT, product_id TEXT, retailer_id TEXT, resync INTEGER NOT NULL DEFAULT 0,
  at TEXT NOT NULL);
CREATE INDEX changes_at ON changes(at);

CREATE TABLE retailers (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, reference TEXT, website TEXT, vat_number TEXT, country TEXT, notes TEXT,
  status TEXT NOT NULL, brand_ids TEXT, allow_original INTEGER, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
  last_activity_at TEXT, grant_updated_at TEXT NOT NULL);

CREATE TABLE users (
  id TEXT PRIMARY KEY, email TEXT NOT NULL UNIQUE, name TEXT NOT NULL, role TEXT NOT NULL, retailer_id TEXT,
  status TEXT NOT NULL, language TEXT NOT NULL, password_hash TEXT, invite_hash TEXT, invite_expires_at TEXT,
  reset_hash TEXT, reset_expires_at TEXT, last_login_at TEXT, created_at TEXT NOT NULL,
  selection TEXT, selection_updated_at TEXT);
CREATE INDEX users_invite ON users(invite_hash);
CREATE INDEX users_reset ON users(reset_hash);
CREATE INDEX users_retailer ON users(retailer_id);

CREATE TABLE sessions (id_hash TEXT PRIMARY KEY, user_id TEXT NOT NULL, csrf TEXT NOT NULL, created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL, last_seen_at TEXT NOT NULL);
CREATE INDEX sessions_user ON sessions(user_id);

CREATE TABLE api_clients (
  id TEXT PRIMARY KEY, client_id TEXT NOT NULL UNIQUE, secret_hash TEXT NOT NULL, secret_version INTEGER NOT NULL DEFAULT 1,
  retailer_id TEXT NOT NULL, name TEXT NOT NULL, scopes TEXT NOT NULL, status TEXT NOT NULL,
  created_by TEXT NOT NULL DEFAULT 'admin', created_at TEXT NOT NULL, last_used_at TEXT);
CREATE INDEX api_clients_retailer ON api_clients(retailer_id);

CREATE TABLE ingest_tokens (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, prefix TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE, scopes TEXT NOT NULL,
  brand_external_ids TEXT, status TEXT NOT NULL, expires_at TEXT, created_at TEXT NOT NULL, last_used_at TEXT);

CREATE TABLE access_requests (
  id TEXT PRIMARY KEY, status TEXT NOT NULL, token_hash TEXT NOT NULL, retailer TEXT NOT NULL, contact TEXT NOT NULL,
  access TEXT NOT NULL, requested_brand_ids TEXT, message TEXT, created_at TEXT NOT NULL, decided_at TEXT, reason TEXT,
  retailer_id TEXT, api_client_id TEXT, client_secret_enc TEXT, claimed INTEGER NOT NULL DEFAULT 0,
  invited INTEGER NOT NULL DEFAULT 0);

CREATE TABLE jobs (
  id TEXT PRIMARY KEY, kind TEXT NOT NULL, status TEXT NOT NULL, owner_type TEXT NOT NULL, owner_id TEXT,
  trigger_kind TEXT, source_id TEXT, dedupe_key TEXT, payload TEXT NOT NULL, result TEXT,
  progress_done INTEGER NOT NULL DEFAULT 0, progress_total INTEGER, created_at TEXT NOT NULL, started_at TEXT,
  finished_at TEXT, expires_at TEXT, lease_until TEXT, attempts INTEGER NOT NULL DEFAULT 0, error TEXT);
CREATE INDEX jobs_status ON jobs(status, created_at);
CREATE INDEX jobs_kind ON jobs(kind, created_at);
CREATE INDEX jobs_owner ON jobs(owner_type, owner_id, created_at);
CREATE INDEX jobs_source ON jobs(source_id, created_at);
CREATE TABLE job_items (job_id TEXT NOT NULL, idx INTEGER NOT NULL, item TEXT NOT NULL,
  is_error INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (job_id, idx));

CREATE TABLE import_sources (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, kind TEXT NOT NULL, enabled INTEGER NOT NULL, schedule TEXT NOT NULL,
  mode TEXT NOT NULL, allow_mass_unpublish INTEGER NOT NULL DEFAULT 0, config TEXT NOT NULL, credentials_enc TEXT,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL, next_run_at TEXT, last_run_id TEXT, last_fingerprint TEXT,
  deleted INTEGER NOT NULL DEFAULT 0);

CREATE TABLE uploads (
  id TEXT PRIMARY KEY, sha256 TEXT NOT NULL, bytes BIGINT NOT NULL, content_type TEXT NOT NULL, part_bytes BIGINT NOT NULL,
  parts_total INTEGER NOT NULL, received TEXT NOT NULL, status TEXT NOT NULL, expires_at TEXT NOT NULL, owner TEXT NOT NULL);

CREATE TABLE webhooks (
  id TEXT PRIMARY KEY, owner_type TEXT NOT NULL, owner_id TEXT, url TEXT NOT NULL, events TEXT NOT NULL,
  secret_enc TEXT NOT NULL, status TEXT NOT NULL, created_at TEXT NOT NULL, last_delivery_at TEXT, last_status INTEGER,
  failing_since TEXT, last_notified_seq BIGINT NOT NULL DEFAULT 0);
CREATE TABLE webhook_deliveries (
  id TEXT PRIMARY KEY, webhook_id TEXT NOT NULL, event_id TEXT NOT NULL, event_type TEXT NOT NULL, payload TEXT NOT NULL,
  attempt INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL, next_attempt_at TEXT NOT NULL, status_code INTEGER,
  duration_ms INTEGER, error TEXT, created_at TEXT NOT NULL, at TEXT);
CREATE INDEX webhook_deliveries_due ON webhook_deliveries(status, next_attempt_at);
CREATE INDEX webhook_deliveries_hook ON webhook_deliveries(webhook_id, created_at);

CREATE TABLE download_events (
  id TEXT PRIMARY KEY, at TEXT NOT NULL, kind TEXT NOT NULL, retailer_id TEXT, retailer_name TEXT, actor_type TEXT NOT NULL,
  actor_id TEXT, actor_name TEXT, variant TEXT, product_id TEXT, style_number TEXT, asset_id TEXT, usage_event TEXT,
  files INTEGER NOT NULL DEFAULT 0, bytes BIGINT);
CREATE INDEX download_events_at ON download_events(at);
CREATE INDEX download_events_retailer ON download_events(retailer_id, at);

CREATE TABLE idempotency (scope TEXT NOT NULL, key TEXT NOT NULL, request_hash TEXT NOT NULL, status INTEGER NOT NULL,
  body TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY (scope, key));
`,
	// 2: audit log, e-mail outbox with retries, indexes for the portal's
	// per-product reads and its style-number order (docs/performance.md)
	`
CREATE INDEX variants_product_visible ON variants(product_id, visible, external_id);
CREATE INDEX assets_product_visible ON assets(product_id, visible);
CREATE INDEX products_visible_style ON products(visible, style_number, id);
CREATE INDEX variants_visible_color ON variants(visible, color_code, product_id, id);

CREATE TABLE audit_log (
  id TEXT PRIMARY KEY, at TEXT NOT NULL, actor_type TEXT NOT NULL, actor_id TEXT, actor_name TEXT, actor_email TEXT,
  actor_role TEXT, action TEXT NOT NULL, method TEXT NOT NULL, route TEXT NOT NULL, target_type TEXT, target_id TEXT,
  status INTEGER NOT NULL, request_id TEXT, details TEXT);
CREATE INDEX audit_log_at ON audit_log(at);
CREATE INDEX audit_log_actor ON audit_log(actor_id, at);
CREATE INDEX audit_log_action ON audit_log(action, at);
CREATE INDEX access_requests_retailer ON access_requests(retailer_id);
CREATE INDEX access_requests_decided ON access_requests(status, decided_at);

CREATE TABLE mail_outbox (
  id TEXT PRIMARY KEY, recipient TEXT NOT NULL, subject TEXT NOT NULL, body_enc TEXT NOT NULL, kind TEXT NOT NULL,
  status TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at TEXT NOT NULL, last_error TEXT,
  created_at TEXT NOT NULL, sent_at TEXT);
CREATE INDEX mail_outbox_due ON mail_outbox(status, next_attempt_at);
`,
	// 3: spec 0.2 article data. Products and variants keep their write model
	// as JSON in data, so no column changes; this marks the stored objects
	// for the one-time rewrite node.App does on start (0.1 fibre keys
	// normalised, representations with the new properties).
	`
INSERT INTO meta (key, value) VALUES ('upgrade_article_data', '0.2');
`,
}

// SchemaVersion is the newest schema this binary knows.
func SchemaVersion() int { return len(migrations) }

// ErrSchemaTooNew means the database was migrated by a newer release.
type ErrSchemaTooNew struct{ Database, Binary int }

func (e *ErrSchemaTooNew) Error() string {
	return fmt.Sprintf("the database has schema version %d but this node-go knows only up to %d: it was upgraded by a newer release. "+
		"Run that release (or newer) again; to go back to this version, restore the backup made before the upgrade (docs/operations.md)", e.Database, e.Binary)
}

// Migrate applies pending migrations. Migrations only move forward: each
// runs once, in its own transaction, and a database newer than this binary
// is refused instead of being used with the wrong schema.
func (d *DB) Migrate(ctx context.Context) error {
	if _, err := d.SQL.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	var current int
	if err := d.SQL.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}
	if current > len(migrations) {
		return &ErrSchemaTooNew{Database: current, Binary: len(migrations)}
	}
	for i := current; i < len(migrations); i++ {
		ddl := migrations[i]
		if d.Dialect == Postgres {
			ddl = strings.ReplaceAll(ddl, "{{SERIAL}}", "BIGSERIAL PRIMARY KEY")
		} else {
			ddl = strings.ReplaceAll(ddl, "{{SERIAL}}", "INTEGER PRIMARY KEY AUTOINCREMENT")
		}
		err := d.WriteTx(ctx, func(tx *Tx) error {
			for _, stmt := range splitStatements(ddl) {
				if _, err := tx.tx.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf("migration %d: %w\n%s", i+1, err, stmt)
				}
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, i+1, Now())
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func splitStatements(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ";") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
