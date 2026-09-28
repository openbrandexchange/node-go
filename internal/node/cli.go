package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/openbrandexchange/node-go/internal/ids"
	"github.com/openbrandexchange/node-go/internal/importfmt"
)

// LocalImport describes an import from local files (CLI).
type LocalImport struct {
	CSV, AssetsCSV, Images, Package string
	Mode                            string
	AllowMassUnpublish, DryRun      bool
}

// ImportLocal stages local files, runs the import and returns the ImportRun.
func (a *App) ImportLocal(ctx context.Context, li LocalImport) (map[string]any, error) {
	req := &importRequest{jobID: ids.New("job"), options: importOptions{Mode: li.Mode, AllowMassUnpublish: li.AllowMassUnpublish, DryRun: li.DryRun}}
	stage := func(field, p string) error {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		imf, err := a.stageFile(ctx, req.jobID, field, filepath.Base(p), f, MaxImportBytes)
		if err != nil {
			return err
		}
		req.files = append(req.files, *imf)
		return nil
	}
	if li.CSV != "" {
		if err := stage("csv", li.CSV); err != nil {
			return nil, err
		}
	}
	if li.AssetsCSV != "" {
		if err := stage("assets_csv", li.AssetsCSV); err != nil {
			return nil, err
		}
	}
	if li.Package != "" {
		if err := stage("package", li.Package); err != nil {
			return nil, err
		}
	}
	if li.Images != "" {
		st, err := os.Stat(li.Images)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			if err := stage("images", li.Images); err != nil {
				return nil, err
			}
		} else {
			err := filepath.WalkDir(li.Images, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || importfmt.Ignored(p) || !importfmt.IsImage(d.Name()) {
					return err
				}
				return stage("images", p)
			})
			if err != nil {
				return nil, err
			}
		}
	}
	if len(req.files) == 0 {
		return nil, fmt.Errorf("nothing to import: pass --csv, --images or --package")
	}
	if req.options.Mode == "" {
		req.options.Mode = "upsert"
	}
	job, err := a.enqueueImport(ctx, req, "cli", "cli", "", "", nil, true)
	if err != nil {
		return nil, err
	}
	a.RunJobsNow(ctx)
	j, err := a.loadJob(ctx, job.ID)
	if err != nil {
		return nil, err
	}
	return a.importRunJSON(ctx, j), nil
}

// ImportErrors returns human-readable problems of an import run.
func (a *App) ImportErrors(ctx context.Context, jobID string, max int) []string {
	rs, err := a.DB.Query(ctx, `SELECT item FROM job_items WHERE job_id = ? ORDER BY idx LIMIT ?`, jobID, max)
	if err != nil {
		return nil
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		var s string
		if rs.Scan(&s) != nil {
			continue
		}
		var e ImportRowError
		_ = json.Unmarshal([]byte(s), &e)
		loc := e.File
		if e.Row != nil {
			loc += fmt.Sprintf(" row %d", *e.Row)
		}
		if e.Column != nil {
			loc += " (" + *e.Column + ")"
		}
		out = append(out, strings.ToUpper(e.Severity[:1])+e.Severity[1:]+": "+loc+": "+e.Detail)
	}
	return out
}
