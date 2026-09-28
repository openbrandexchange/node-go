// Command node-go runs an Open Brand Exchange brand node: the retailer
// portal, the OBX Protocol for retailers' software and the OBX Ingest API
// for the brand's own automation, in one binary.
//
//	node-go serve                                   start the node
//	node-go import --csv products.csv --images dir  import from local files
//	node-go admin reset-password --email you@example.com
//	node-go admin create-ingest-token --name "Nightly PIM sync"
//	node-go backup --to DIR | restore --from DIR    backups
//	node-go check-config                            validate the configuration
//	node-go healthcheck                             exit 0 when the node answers
//	node-go version
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // schedules use IANA time zones; images may lack zoneinfo

	"github.com/openbrandexchange/node-go/internal/config"
	"github.com/openbrandexchange/node-go/internal/node"
	"github.com/openbrandexchange/node-go/ui"
)

func usage() {
	fmt.Fprint(os.Stderr, `node-go - Open Brand Exchange brand portal (Go edition)

Usage:
  node-go serve                          Start the portal (configuration via environment, see README)
  node-go import [flags]                 Import a canonical CSV and/or images from local files
      --csv FILE            products.csv
      --assets FILE         assets.csv (rights, AI provenance, alt text)
      --images DIR|ZIP      folder or zip with images named {style}_{color}_{position}.jpg
      --package ZIP         package zip (products.csv, assets.csv and images)
      --mode upsert|mirror  mirror also unpublishes products missing from the file
      --allow-mass-unpublish
      --dry-run             check everything, change nothing
  node-go admin reset-password --email ADDRESS [--password-stdin]
  node-go admin create-ingest-token --name NAME [--scopes ingest:read,ingest:write] [--brands a,b]
  node-go admin audit-log [--limit N]    Print the latest admin actions
  node-go admin personal-data (--email ADDRESS | --retailer ID)
                                         Print everything stored about a person or retailer (JSON)
  node-go backup --to DIR                Consistent copy of the database, files and secret key
  node-go restore --from DIR [--force]   Restore a backup made with "backup" (node must be stopped)
  node-go check-config                   Validate the configuration and exit
  node-go healthcheck                    Exit 0 when the local node answers /healthz
  node-go version
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve()
	case "import":
		err = runImport(os.Args[2:])
	case "admin":
		err = admin(os.Args[2:])
	case "healthcheck":
		err = healthcheck()
	case "check-config":
		err = checkConfig()
	case "backup":
		err = runBackup(os.Args[2:])
	case "restore":
		err = runRestore(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("node-go", node.Version)
	case "help", "--help", "-h":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newLogger(cfg *config.Config) *slog.Logger {
	lvl := slog.LevelInfo
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if cfg.LogFormat == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(node.LogHandler(h)).With("service", "node-go")
}

func open(ctx context.Context) (*node.App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	log := newLogger(cfg)
	slog.SetDefault(log)
	for _, w := range cfg.Warnings() {
		log.Warn(w)
	}
	return node.New(ctx, cfg, ui.FS(), log)
}

func serve() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app, err := open(ctx)
	if err != nil {
		return err
	}
	defer app.Close()
	app.Start()
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", app.Cfg.Port),
		Handler:           app.Handler(),
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(app.Log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	app.Log.Info("node-go is listening", "port", app.Cfg.Port, "public_url", app.Cfg.PublicURL, "blobs", app.Store.Describe(), "database", app.DB.Dialect)
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		stop()
		app.SetDraining()
		if d := app.Cfg.ShutdownDelaySeconds; d > 0 {
			app.Log.Info("shutting down: readiness off, waiting for the load balancer", "seconds", d)
			time.Sleep(time.Duration(d) * time.Second)
		}
		app.Log.Info("shutting down: finishing running requests (at most 25 s)")
		sctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			app.Log.Warn("requests were still running at shutdown", "error", err)
		}
	}
	app.Stop()
	app.Log.Info("stopped")
	return nil
}

func runImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	csv := fs.String("csv", "", "products.csv")
	assets := fs.String("assets", "", "assets.csv")
	images := fs.String("images", "", "folder or zip with images")
	pkg := fs.String("package", "", "package zip")
	mode := fs.String("mode", "upsert", "upsert or mirror")
	mass := fs.Bool("allow-mass-unpublish", false, "allow a mirror import to unpublish more than half")
	dry := fs.Bool("dry-run", false, "check only")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	app, err := open(ctx)
	if err != nil {
		return err
	}
	defer app.Close()
	if !app.SetupDone(ctx) {
		return errors.New("finish the first-run setup in the web interface first (it creates your brand)")
	}
	run, err := app.ImportLocal(ctx, node.LocalImport{CSV: *csv, AssetsCSV: *assets, Images: *images, Package: *pkg,
		Mode: *mode, AllowMassUnpublish: *mass, DryRun: *dry})
	if err != nil {
		return err
	}
	c := run["counts"].(map[string]any)
	fmt.Printf("Import %s: %v rows, %v images; %v created, %v updated, %v unchanged, %v unpublished; %v errors, %v warnings\n",
		run["status"], c["rows"], c["files"], c["created"], c["updated"], c["unchanged"], c["unpublished"], c["errors"], c["warnings"])
	for _, e := range app.ImportErrors(ctx, run["id"].(string), 50) {
		fmt.Println("  " + e)
	}
	if run["status"] != "succeeded" {
		return errors.New("the import failed")
	}
	return nil
}

func admin(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("missing admin command")
	}
	ctx := context.Background()
	switch args[0] {
	case "reset-password":
		fs := flag.NewFlagSet("reset-password", flag.ContinueOnError)
		email := fs.String("email", "", "account e-mail")
		stdin := fs.Bool("password-stdin", false, "read the new password from standard input")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *email == "" {
			return errors.New("--email is required")
		}
		app, err := open(ctx)
		if err != nil {
			return err
		}
		defer app.Close()
		if *stdin {
			pw, err := bufio.NewReader(io.LimitReader(os.Stdin, 1024)).ReadString('\n')
			if err != nil && err != io.EOF {
				return err
			}
			if err := app.SetPassword(ctx, *email, strings.TrimRight(pw, "\r\n")); err != nil {
				return err
			}
			app.Audit(ctx, "password.set", "user", *email)
			fmt.Println("Password changed; all sessions of this account were ended.")
			return nil
		}
		link, err := app.ResetLink(ctx, *email)
		if err != nil {
			return err
		}
		app.Audit(ctx, "password.reset-link", "user", *email)
		fmt.Println("Open this link within one hour to choose a new password (works once):")
		fmt.Println(link)
		return nil
	case "create-ingest-token":
		fs := flag.NewFlagSet("create-ingest-token", flag.ContinueOnError)
		name := fs.String("name", "", "token name")
		scopes := fs.String("scopes", "ingest:read,ingest:write", "comma-separated scopes")
		brands := fs.String("brands", "", "restrict to these brand external ids (comma-separated)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		app, err := open(ctx)
		if err != nil {
			return err
		}
		defer app.Close()
		if !app.SetupDone(ctx) {
			return errors.New("finish the first-run setup first")
		}
		var b []string
		if *brands != "" {
			b = strings.Split(*brands, ",")
		}
		_, secret, p := app.CreateIngestToken(ctx, *name, strings.Split(*scopes, ","), b, nil)
		if p != nil {
			return p
		}
		app.Audit(ctx, "ingest-tokens.create", "ingest-token", *name)
		fmt.Println("Token (shown once; store it as OBX_TOKEN in your automation):")
		fmt.Println(secret)
		fmt.Println("OBX_URL=" + app.Cfg.PublicURL)
		return nil
	case "audit-log":
		fs := flag.NewFlagSet("audit-log", flag.ContinueOnError)
		limit := fs.Int("limit", 50, "number of entries")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		app, err := open(ctx)
		if err != nil {
			return err
		}
		defer app.Close()
		items, err := app.AuditLog(ctx, *limit)
		if err != nil {
			return err
		}
		for _, it := range items {
			actor := it["actor"].(map[string]any)
			who := fmt.Sprint(actor["type"])
			if e, ok := actor["email"].(*string); ok && e != nil {
				who = *e
			}
			target := it["target"].(map[string]any)
			tgt := ""
			if id, ok := target["id"].(*string); ok && id != nil {
				tgt = " " + *id
			}
			fmt.Printf("%s  %-28s %s%s (%v)\n", it["at"], it["action"], who, tgt, it["status"])
		}
		return nil
	case "personal-data":
		fs := flag.NewFlagSet("personal-data", flag.ContinueOnError)
		email := fs.String("email", "", "account e-mail")
		retailer := fs.String("retailer", "", "retailer id")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if (*email == "") == (*retailer == "") {
			return errors.New("pass either --email or --retailer")
		}
		app, err := open(ctx)
		if err != nil {
			return err
		}
		defer app.Close()
		data, err := app.PersonalData(ctx, *email, *retailer)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		app.Audit(ctx, "personal-data.export", map[bool]string{true: "retailer", false: "user"}[*retailer != ""], *email+*retailer)
		return enc.Encode(data)
	}
	usage()
	return fmt.Errorf("unknown admin command %q", args[0])
}

func runBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	to := fs.String("to", "", "new, empty directory for the backup")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *to == "" {
		return errors.New("--to is required, for example: node-go backup --to /backups/obx-$(date +%F)")
	}
	ctx := context.Background()
	app, err := open(ctx)
	if err != nil {
		return err
	}
	defer app.Close()
	m, err := app.Backup(ctx, *to)
	if err != nil {
		return err
	}
	fmt.Printf("Backup written to %s (schema %d, %d files)\n  database:   %s\n  files:      %s\n  secret key: %s\n",
		*to, m.SchemaVersion, m.FileCount, m.Database, m.Files, m.SecretKey)
	return nil
}

func runRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	from := fs.String("from", "", "backup directory made with node-go backup")
	force := fs.Bool("force", false, "replace an existing database")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" {
		return errors.New("--from is required")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	m, err := node.Restore(cfg, *from, *force)
	if err != nil {
		return err
	}
	fmt.Printf("Restored the backup of %s (node %s). Start the node with: node-go serve\n", m.CreatedAt, m.NodeID)
	return nil
}

func checkConfig() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	for _, w := range cfg.Warnings() {
		fmt.Println("warning:", w)
	}
	fmt.Println("The configuration is valid.")
	fmt.Println("  public address:", cfg.PublicURL)
	fmt.Println("  data directory:", cfg.DataDir)
	fmt.Println("  database:      ", strings.SplitN(cfg.DatabaseURL, ":", 2)[0])
	fmt.Println("  files:         ", strings.SplitN(cfg.BlobURL, ":", 2)[0])
	fmt.Println("  e-mail:        ", map[bool]string{true: "SMTP", false: "not sent (written to the log)"}[cfg.SMTPURL != ""])
	return nil
}

func healthcheck() error {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("health check returned %d", resp.StatusCode)
	}
	return nil
}
