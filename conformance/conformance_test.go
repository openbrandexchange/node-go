// Package conformance runs the OBX conformance suite (obx-go/conformance)
// against an in-process node-go on a fresh SQLite database, the way the
// spec's reference path describes: setup, fixtures, then every profile.
package conformance

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbrandexchange/node-go/internal/config"
	"github.com/openbrandexchange/node-go/internal/node"
	"github.com/openbrandexchange/node-go/ui"
	"github.com/openbrandexchange/obx-go/conformance"
)

func TestConformance(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewUnstartedServer(nil)
	base := "http://" + srv.Listener.Addr().String()
	cfg := &config.Config{
		PublicURL: base, Port: 8080, DataDir: dir,
		DatabaseURL: "sqlite:" + filepath.Join(dir, "obx.db"), BlobURL: "file://" + filepath.Join(dir, "blobs"),
		SecretKey: []byte("conformance-secret-key-0123456789abcdef"), SetupCode: "CONF-TEST-CODE",
		AllowPrivateURLs: true, Workers: 2, ChangeRetentionDays: 400, MailFrom: "no-reply@example.test",
	}
	if pg := os.Getenv("OBX_TEST_DATABASE_URL"); pg != "" {
		cfg.DatabaseURL = pg
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	app, err := node.New(context.Background(), cfg, ui.FS(), log)
	if err != nil {
		t.Fatal(err)
	}
	app.Start()
	srv.Config.Handler = app.Handler()
	srv.Start()
	defer func() {
		srv.Close()
		app.Stop()
		app.Close()
	}()
	var progress io.Writer = io.Discard
	if testing.Verbose() {
		progress = os.Stderr
	}
	rep, err := conformance.Run(context.Background(), []string{conformance.ProfileAll}, conformance.Config{BaseURL: base, SetupCode: "CONF-TEST-CODE", Log: progress})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Results {
		switch r.Status {
		case conformance.StatusFail:
			t.Errorf("%s FAIL: %s\n    %s", r.ID, r.Title, strings.Join(r.Messages, "\n    "))
		case conformance.StatusSkip:
			t.Logf("%s skipped: %s (%s)", r.ID, r.Title, strings.Join(r.Messages, "; "))
		}
	}
	t.Logf("passed %d, failed %d, skipped %d in %d ms", rep.Passed, rep.Failed, rep.Skipped, rep.Millis)
}
