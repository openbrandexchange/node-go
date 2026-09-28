// Package ui embeds the built admin-ui (brand admin + retailer portal).
// The integrate step replaces ui/dist with the admin-ui build output
// (scripts/vendor-ui.sh); until then a small placeholder page is served.
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
