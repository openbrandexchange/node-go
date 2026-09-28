package node

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/problem"
)

var errRollback = errors.New("rollback")

// WriteOp runs one catalog operation in its own transaction. A failed
// operation is rolled back completely (no change entries).
func (a *App) WriteOp(ctx context.Context, allowedBrands []string, fn func(w *writer) (*IngestResult, *problem.Problem)) (*IngestResult, *problem.Problem) {
	var res *IngestResult
	var prob *problem.Problem
	err := a.DB.WriteTx(ctx, func(tx *db.Tx) error {
		w := a.newWriter(ctx, tx)
		w.allowedBrands = allowedBrands
		res, prob = fn(w)
		if prob != nil {
			return errRollback
		}
		return nil
	})
	if err != nil && !errors.Is(err, errRollback) {
		return res, problem.As(err)
	}
	if prob != nil && res != nil {
		res.Result = "error"
		res.Error = prob
	}
	return res, prob
}

// writeResult sends an IngestResult (201 for created).
func (a *App) writeResult(w http.ResponseWriter, r *http.Request, res *IngestResult, prob *problem.Problem, locationBase string) {
	if prob != nil {
		problem.Write(w, prob)
		return
	}
	if res.ETag != nil {
		w.Header().Set("ETag", *res.ETag)
	}
	status := http.StatusOK
	if res.Result == "created" {
		status = http.StatusCreated
		if locationBase != "" {
			w.Header().Set("Location", locationBase+"/"+url.PathEscape(res.ExternalID))
		}
	}
	writeJSON(w, status, res)
}

// runOperation applies one IngestOperation (batch element).
func (w *writer) runOperation(op, kind, ext string, data map[string]any, ifMatch string) (*IngestResult, *problem.Problem) {
	switch kind {
	case KindBrand, KindProduct, KindVariant, KindAsset:
	default:
		return &IngestResult{Op: op, Kind: kind, ExternalID: ext}, problem.New("validation_failed", "Unknown kind %q.", kind)
	}
	switch op {
	case "upsert":
		return w.Upsert(kind, ext, data, ifMatch)
	case "patch":
		return w.Patch("patch", kind, ext, data, ifMatch)
	case "delete":
		return w.Delete(kind, ext)
	case "publish", "unpublish":
		return w.SetStatus(op, kind, ext)
	}
	return &IngestResult{Op: op, Kind: kind, ExternalID: ext}, problem.New("validation_failed", "Unknown op %q.", op)
}
