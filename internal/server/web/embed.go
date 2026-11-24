package web

import (
	"embed"
	"io/fs"
)

//go:embed index.html style.css app.js
var assets embed.FS

// StaticFS returns the embedded web UI filesystem.
func StaticFS() (fs.FS, error) {
	return assets, nil
}
