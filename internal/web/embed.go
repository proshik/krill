package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"

	"github.com/proshik/krill/internal/buildinfo"
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

// versionedAssets are the files AssetVersion covers, relative to Static().
var versionedAssets = []string{"app.css", "app.js"}

// IsVersioned reports whether AssetVersion covers the static file name
// (relative to Static()), so a URL carrying the current version names exactly
// this content and may be cached for good.
func IsVersioned(name string) bool {
	for _, v := range versionedAssets {
		if v == name {
			return true
		}
	}
	return false
}

// AssetVersion is a short hash of the build's version and of the stylesheet
// and script the pages load. It is appended to their URLs, so a browser
// fetches them again after an upgrade, and pages send it with every htmx
// request, so a tab still running an older release's assets is reloaded.
var AssetVersion = func() string {
	h := sha256.New()
	h.Write([]byte(buildinfo.Version + "\n"))
	for _, name := range versionedAssets {
		b, err := staticEmbed.ReadFile("static/" + name)
		if err != nil {
			continue
		}
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:10]
}()
