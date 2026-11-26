package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"

	"github.com/proshik/krill/internal/web"
)

// Cache policies for /static. A URL carrying the current AssetVersion names
// fixed content, so it is cached for good; fonts change only with a release
// and are kept for a month; everything else is revalidated on every use and
// answered with 304 while its ETag matches.
const (
	cacheImmutable  = "public, max-age=31536000, immutable"
	cacheFonts      = "public, max-age=2592000"
	cacheRevalidate = "no-cache"
)

// embeddedStatic is the handler for the embedded assets, built once: hashing
// them is cheap but needless to repeat for every router.
var embeddedStatic = sync.OnceValues(func() (http.Handler, error) {
	return staticHandler(web.Static())
})

// staticHandler serves fsys (the embedded static assets) under a prefix that
// the caller has already stripped. It sets a strong content ETag and a
// Cache-Control policy on every file it knows and leaves the rest (ranges,
// conditional requests, 404s, directories) to http.FileServerFS, which honours
// a pre-set ETag in its precondition checks.
func staticHandler(fsys fs.FS) (http.Handler, error) {
	etags := map[string]string{}
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		etags[name] = `"` + hex.EncodeToString(sum[:])[:16] + `"`
		return nil
	})
	if err != nil {
		return nil, err
	}
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if etag, ok := etags[name]; ok && !strings.HasSuffix(r.URL.Path, "/") {
			h := w.Header()
			h.Set("ETag", etag)
			switch {
			case web.IsVersioned(name) && r.URL.Query().Get("v") == web.AssetVersion:
				h.Set("Cache-Control", cacheImmutable)
			case strings.HasPrefix(name, "fonts/"):
				h.Set("Cache-Control", cacheFonts)
			default:
				h.Set("Cache-Control", cacheRevalidate)
			}
		}
		files.ServeHTTP(w, r)
	}), nil
}
