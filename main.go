// Command node-go runs an Open Brand Exchange brand node: the retailer
// portal, the OBX Protocol for retailers' software and the OBX Ingest API
// for the brand's own automation, in one binary.
//
//	node-go serve                                   start the node
//	node-go import --csv products.csv --images dir  import from local files
//	node-go admin reset-password --email you@example.com
//	node-go admin create-ingest-token --name "Nightly PIM sync"
//	node-go healthcheck                             exit 0 when the node answers
//	node-go version
package main

import (
	"bufio"
	"context"
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
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func open(ctx context.Context) (*node.App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return node.New(ctx, cfg, ui.FS(), newLogger(cfg))
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
		app.Log.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}
	app.Stop()
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
			fmt.Println("Password changed; all sessions of this account were ended.")
			return nil
		}
		link, err := app.ResetLink(ctx, *email)
		if err != nil {
			return err
		}
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
		fmt.Println("Token (shown once; store it as OBX_TOKEN in your automation):")
		fmt.Println(secret)
		fmt.Println("OBX_URL=" + app.Cfg.PublicURL)
		return nil
	}
	usage()
	return fmt.Errorf("unknown admin command %q", args[0])
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
