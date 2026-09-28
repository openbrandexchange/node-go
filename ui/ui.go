// Package ui embeds the built admin-ui (brand admin + retailer portal).
// ui/dist is the admin-ui build output, re-copied with
// (scripts/update-ui.sh).
package ui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the UI files (index.html at the root).
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
