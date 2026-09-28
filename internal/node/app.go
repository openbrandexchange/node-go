// Package node implements an OBX brand node: the OBX Protocol (read side),
// the OBX Ingest API (write side), the Node Admin + Portal API used by the
// shared admin-ui, imports, renditions, exports, webhooks and statistics.
package node

import (
	"context"
	"crypto/rand"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openbrandexchange/node-go/internal/blobstore"
	"github.com/openbrandexchange/node-go/internal/config"
	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/ids"
	"github.com/openbrandexchange/node-go/internal/safehttp"
	"github.com/openbrandexchange/node-go/internal/secure"
)

// Software and Version identify this implementation in discovery.
const (
	Software    = "obx-node-go"
	SpecVersion = "0.1.0-draft"
	OBXVersion  = "0.1"
)

// Version is set at build time (-ldflags "-X .../node.Version=...").
var Version = "0.1.0-dev"

// App holds the node's shared state.
type App struct {
	Cfg    *config.Config
	DB     *db.DB
	Store  blobstore.Store
	Keys   *secure.Keys
	Log    *slog.Logger
	Mail   Mailer
	Policy safehttp.Policy
	UI     fs.FS // admin-ui build (index.html, assets, locales)

	settingsMu sync.RWMutex
	settings   *Settings
	setupDone  atomic.Bool
	nodeID     string

	limiter *rateLimiter
	wake    chan struct{} // job workers
	notify  chan struct{} // webhook change notifier
	renderQ chan string   // original sha256 to render
	stop    chan struct{}
	wg      sync.WaitGroup
	locales *localeCache

	// Now can be replaced in tests.
	Now func() time.Time
}

// New opens the database and blob store, migrates, and prepares the node.
func New(ctx context.Context, cfg *config.Config, ui fs.FS, log *slog.Logger) (*App, error) {
	d, err := db.Open(cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if err := d.Migrate(ctx); err != nil {
		d.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	store, err := blobstore.Open(cfg.BlobURL)
	if err != nil {
		d.Close()
		return nil, err
	}
	a := &App{
		Cfg:     cfg,
		DB:      d,
		Store:   store,
		Keys:    secure.NewKeys(cfg.SecretKey),
		Log:     log,
		Policy:  safehttp.Policy{AllowPrivate: cfg.AllowPrivateURLs},
		UI:      ui,
		limiter: newRateLimiter(),
		wake:    make(chan struct{}, 1),
		notify:  make(chan struct{}, 1),
		renderQ: make(chan string, 1024),
		stop:    make(chan struct{}),
		Now:     time.Now,
	}
	a.locales = newLocaleCache(ui)
	a.Mail = newMailer(cfg, log)
	if err := a.initMeta(ctx); err != nil {
		d.Close()
		return nil, err
	}
	return a, nil
}

// Close releases resources (after Stop).
func (a *App) Close() error { return a.DB.Close() }

func (a *App) now() time.Time { return a.Now().UTC() }

func (a *App) nowStr() string { return db.Fmt(a.now()) }

// initMeta creates the node id and, before setup, the setup code.
func (a *App) initMeta(ctx context.Context) error {
	return a.DB.WriteTx(ctx, func(tx *db.Tx) error {
		id, err := getMeta(ctx, tx, "node_id")
		if err != nil {
			return err
		}
		if id == "" {
			id = ids.New("nod")
			if err := setMeta(ctx, tx, "node_id", id); err != nil {
				return err
			}
		}
		a.nodeID = id
		done, _ := getMeta(ctx, tx, "setup_done")
		if done == "1" {
			a.setupDone.Store(true)
			return nil
		}
		if a.Cfg.SetupCode != "" {
			return setMeta(ctx, tx, "setup_code_hash", secure.HashToken(a.Cfg.SetupCode))
		}
		existing, _ := getMeta(ctx, tx, "setup_code_hash")
		code, _ := getMeta(ctx, tx, "setup_code_plain_once")
		if existing == "" {
			code = newSetupCode()
			if err := setMeta(ctx, tx, "setup_code_hash", secure.HashToken(code)); err != nil {
				return err
			}
			// Kept only until setup completes so a restart can print it again.
			if err := setMeta(ctx, tx, "setup_code_plain_once", a.mustEncrypt(code)); err != nil {
				return err
			}
		} else if code != "" {
			if b, err := a.Keys.Decrypt(code); err == nil {
				code = string(b)
			} else {
				code = ""
			}
		}
		if code != "" {
			tx.AfterCommit(func() { a.printSetupCode(code) })
		}
		return nil
	})
}

func (a *App) printSetupCode(code string) {
	msg := fmt.Sprintf("\n  First-run setup: open %s and enter this setup code: %s\n  (set OBX_SETUP_CODE to choose it yourself)\n", a.Cfg.PublicURL, code)
	a.Log.Warn("node is not set up yet; the setup code is needed once in the setup wizard", "setup_url", a.Cfg.PublicURL, "setup_code", code)
	fmt.Fprint(logWriter, msg)
}

// newSetupCode returns an easy to type code like "K7QX-9M2P-VT4C".
func newSetupCode() string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	var b strings.Builder
	for i := 0; i < 12; i++ {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b.WriteByte(alphabet[n.Int64()])
	}
	return b.String()
}

func (a *App) mustEncrypt(s string) string {
	v, err := a.Keys.Encrypt([]byte(s))
	if err != nil {
		panic(err)
	}
	return v
}

// NodeID returns the publisher id.
func (a *App) NodeID() string { return a.nodeID }

// kick wakes the job workers.
func (a *App) kick() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// changed signals the webhook notifier that change entries were written.
func (a *App) changed() {
	select {
	case a.notify <- struct{}{}:
	default:
	}
}
