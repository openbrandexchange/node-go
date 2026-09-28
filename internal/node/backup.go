package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openbrandexchange/node-go/internal/blobstore"
	"github.com/openbrandexchange/node-go/internal/config"
	"github.com/openbrandexchange/node-go/internal/db"
)

// BackupManifest describes a backup directory made by "node-go backup".
type BackupManifest struct {
	Format            int    `json:"format"`
	CreatedAt         string `json:"created_at"`
	NodeGoVersion     string `json:"node_go_version"`
	SchemaVersion     int    `json:"schema_version"`
	NodeID            string `json:"node_id"`
	Database          string `json:"database"` // "obx.db" or a note
	Files             string `json:"files"`    // "blobs" or a note
	FileCount         int    `json:"file_count"`
	SecretKey         string `json:"secret_key"` // "secret.key" or a note
	SecretFingerprint string `json:"secret_fingerprint"`
}

func secretFingerprint(key []byte) string {
	h := sha256.Sum256(append([]byte("obx-backup-fingerprint/"), key...))
	return hex.EncodeToString(h[:8])
}

// Backup writes a consistent copy of the node to dir (which must be empty
// or missing): the SQLite database as one file (VACUUM INTO, safe while the
// node runs), the local files and the secret key. Files in a bucket and a
// PostgreSQL database are not copied; the manifest says so.
func (a *App) Backup(ctx context.Context, dir string) (*BackupManifest, error) {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("%s is not empty; choose a new directory for each backup", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	m := &BackupManifest{Format: 1, CreatedAt: a.now().Format(time.RFC3339), NodeGoVersion: Version, SchemaVersion: db.SchemaVersion(),
		NodeID: a.nodeID, SecretFingerprint: secretFingerprint(a.Cfg.SecretKey)}
	if a.DB.Dialect == db.SQLite {
		target := filepath.Join(dir, "obx.db")
		if _, err := a.DB.SQL.ExecContext(ctx, `VACUUM INTO ?`, target); err != nil {
			return nil, fmt.Errorf("database snapshot: %w", err)
		}
		m.Database = "obx.db"
	} else {
		m.Database = "not included: back up PostgreSQL with pg_dump (see docs/operations.md)"
	}
	if l, ok := a.Store.(*blobstore.Local); ok {
		n, err := copyTree(l.Root(), filepath.Join(dir, "blobs"))
		if err != nil {
			return nil, fmt.Errorf("files: %w", err)
		}
		m.Files, m.FileCount = "blobs", n
	} else {
		m.Files = "not included: the files are in " + a.Store.Describe() + " (use bucket versioning or replication)"
	}
	if !a.Cfg.SecretFromEnv {
		// The key in use (normally $OBX_DATA_DIR/secret.key).
		if err := os.WriteFile(filepath.Join(dir, "secret.key"), append(append([]byte(nil), a.Cfg.SecretKey...), '\n'), 0o600); err != nil {
			return nil, fmt.Errorf("secret key: %w", err)
		}
		m.SecretKey = "secret.key"
	} else {
		m.SecretKey = "not included: OBX_SECRET_KEY is set in the environment; keep it with this backup"
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(mustJSON(m), '\n'), 0o600); err != nil {
		return nil, err
	}
	return m, nil
}

// Restore puts a backup made by Backup in place for cfg. The node must not
// be running. Without force it refuses to overwrite an existing database.
// It does not start the node; the next start opens the restored data.
func Restore(cfg *config.Config, dir string, force bool) (*BackupManifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("%s has no manifest.json; is it a backup made with node-go backup? %w", dir, err)
	}
	var m BackupManifest
	if err := json.Unmarshal(raw, &m); err != nil || m.Format != 1 {
		return nil, fmt.Errorf("%s/manifest.json is not a backup manifest this version understands", dir)
	}
	if m.SchemaVersion > db.SchemaVersion() {
		return nil, fmt.Errorf("the backup has schema version %d; this node-go knows only up to %d. Restore it with node-go %s or newer", m.SchemaVersion, db.SchemaVersion(), m.NodeGoVersion)
	}
	if cfg.SecretFromEnv && secretFingerprint(cfg.SecretKey) != m.SecretFingerprint && !force {
		return nil, errors.New("OBX_SECRET_KEY differs from the key the backup was made with: stored import credentials and webhook secrets would be unreadable. Set the original key, or pass --force")
	}
	target := db.SQLitePath(cfg.DatabaseURL)
	if m.Database == "obx.db" {
		if target == "" {
			return nil, errors.New("the backup holds a SQLite database but OBX_DATABASE_URL points elsewhere; restore it with OBX_DATABASE_URL=sqlite:/path/obx.db")
		}
		if _, err := os.Stat(target); err == nil && !force {
			return nil, fmt.Errorf("%s already exists; stop the node and pass --force to replace it", target)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return nil, err
		}
		for _, suffix := range []string{"-wal", "-shm"} {
			_ = os.Remove(target + suffix)
		}
		if err := copyFile(filepath.Join(dir, "obx.db"), target); err != nil {
			return nil, fmt.Errorf("database: %w", err)
		}
	}
	if m.Files == "blobs" {
		st, err := blobstore.Open(cfg.BlobURL)
		if err != nil {
			return nil, err
		}
		l, ok := st.(*blobstore.Local)
		if !ok {
			return nil, fmt.Errorf("the backup holds local files but OBX_BLOB_URL is %s; restore with the default file store, or copy %s/blobs to the bucket yourself", st.Describe(), dir)
		}
		if _, err := copyTree(filepath.Join(dir, "blobs"), l.Root()); err != nil {
			return nil, fmt.Errorf("files: %w", err)
		}
	}
	if m.SecretKey == "secret.key" && !cfg.SecretFromEnv {
		if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
			return nil, err
		}
		if err := copyFile(filepath.Join(dir, "secret.key"), filepath.Join(cfg.DataDir, "secret.key")); err != nil {
			return nil, fmt.Errorf("secret key: %w", err)
		}
	}
	return &m, nil
}

// copyTree copies regular files below src into dst (existing files with the
// same name are replaced; files are content-addressed, so this is safe).
func copyTree(src, dst string) (int, error) {
	n := 0
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		if !d.Type().IsRegular() || strings.HasPrefix(d.Name(), ".tmp-") {
			return nil
		}
		n++
		return copyFile(p, filepath.Join(dst, rel))
	})
	return n, err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp := dst + ".restore-tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
