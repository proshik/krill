package web

import (
	"embed"
	"io/fs"
)

//go:embed all:static
var staticEmbed embed.FS

// Static возвращает поддерево со статикой (без префикса "static").
func Static() fs.FS {
	sub, err := fs.Sub(staticEmbed, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
