package web

import (
	"embed"
	"io/fs"
)

//go:embed all:static
var staticEmbed embed.FS

// Static returns the static assets subtree (without the "static" prefix).
func Static() fs.FS {
	sub, err := fs.Sub(staticEmbed, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
