package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/proshik/krill/internal/config"
	"github.com/proshik/krill/internal/web/flash"
)

func TestSetTakeFlashRoundTrip(t *testing.T) {
	s := &Server{cfg: config.Config{CookieSecure: false}}
	rec := httptest.NewRecorder()
	s.setFlash(rec, httptest.NewRequest(http.MethodPost, "/", nil), "err", "boom")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	k, msg := s.takeFlash(rec2, req)
	if k != "err" || msg != "boom" {
		t.Fatalf("got kind=%q msg=%q, want err/boom", k, msg)
	}
	cleared := false
	for _, c := range rec2.Result().Cookies() {
		if c.Name == flashCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("flash cookie was not cleared")
	}
}

func TestFlashMiddlewareInjectsContext(t *testing.T) {
	s := &Server{cfg: config.Config{CookieSecure: false}}
	rec := httptest.NewRecorder()
	s.setFlash(rec, httptest.NewRequest(http.MethodPost, "/", nil), "ok", "saved")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	var got *flash.Flash
	h := s.flashMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = flash.From(r.Context())
	}))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got == nil || got.Kind != "ok" || got.Msg != "saved" {
		t.Fatalf("middleware did not inject flash: %+v", got)
	}
}
