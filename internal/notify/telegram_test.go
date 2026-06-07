package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSendTelegramOK(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	err := sendTelegramTo(context.Background(), srv.URL, "TOKEN", "123", "hello")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(gotPath, "/botTOKEN/sendMessage") {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(gotBody, `"chat_id":"123"`) || !strings.Contains(gotBody, `"text":"hello"`) {
		t.Errorf("body = %q", gotBody)
	}
}

// TestSendTelegramErrorResponse covers the API-error path (HTTP 200 reached but
// the API returns ok:false). The error is built from the description, so it
// cannot contain the token regardless.
func TestSendTelegramErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"ok":false,"description":"chat not found"}`))
	}))
	defer srv.Close()

	err := sendTelegramTo(context.Background(), srv.URL, "SECRET-TOKEN", "123", "hi")
	if err == nil {
		t.Fatal("expected error on ok:false")
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Fatalf("token leaked in error: %v", err)
	}
}

// TestSendTelegramNetworkErrorRedactsToken covers the security-critical path:
// a transport error returns a *url.Error whose text embeds the request URL
// (which contains the bot token). redactToken must strip it.
func TestSendTelegramNetworkErrorRedactsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed server → connection refused on Do()

	err := sendTelegramTo(context.Background(), srv.URL, "SECRET-TOKEN", "123", "hi")
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Fatalf("token leaked in network error: %v", err)
	}
}
