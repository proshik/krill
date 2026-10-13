package server_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
)

// TestEnvEditorBrowser serves the real router against a disposable testcontainer
// for scripts/env-editor.browser.js. No Docker engine or deployer is wired.
// Run with KRILL_ENV_BROWSER=1 and the usual Colima test environment, then pass
// the logged URL to the browser script. POST /_test/stop releases all resources.
func TestEnvEditorBrowser(t *testing.T) {
	if os.Getenv("KRILL_ENV_BROWSER") != "1" {
		t.Skip("opt-in browser fixture")
	}
	s := newShareEnv(t)
	raw := "# Keep comment=unchanged\n PUBLIC =  ordinary  \nTOKEN=private=one\nOTHER=private-two\n"
	if err := s.q.UpdateApplicationEnvClassified(t.Context(), db.UpdateApplicationEnvClassifiedParams{ID: s.webID, EnvText: raw, EnvPublicKeys: []string{"PUBLIC"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.q.UpdateApplicationEnv(t.Context(), db.UpdateApplicationEnvParams{ID: s.apiID, EnvText: "LEGACY=private-legacy"}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /_test/stop", func(w http.ResponseWriter, r *http.Request) {
		select {
		case stop <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("/", s.h)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Logf("Browser fixture: %s%s?tab=env; second app: %s%s?tab=env; login: share-owner@k.local / pw", srv.URL, s.webURL, srv.URL, s.apiURL)
	select {
	case <-stop:
	case <-time.After(20 * time.Minute):
		t.Fatal("browser fixture timed out")
	}
}
