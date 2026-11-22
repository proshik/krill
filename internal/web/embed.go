package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
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

// AssetVersion is a short hash of the stylesheet and script the pages load,
// appended to their URLs so a browser fetches them again after an upgrade.
var AssetVersion = func() string {
	h := sha256.New()
	for _, name := range []string{"static/app.css", "static/app.js"} {
		b, err := staticEmbed.ReadFile(name)
		if err != nil {
			continue
		}
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:10]
}()
