// Package ui carries the console module: web/dist/plugin copied here by
// `make web` and embedded into the binary. Without a build (dist holds
// only .keep) the service declares no UI and the console shows the
// platform's service pages alone.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var assets embed.FS

// Bundle is the module's files, or nil when the binary carries none.
func Bundle() fs.FS {
	dist, err := fs.Sub(assets, "dist")
	if err != nil {
		return nil
	}

	if _, err := fs.Stat(dist, "plugin.json"); err != nil {
		return nil
	}

	return dist
}
