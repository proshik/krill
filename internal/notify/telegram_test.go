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

// botReply serves a Bot API answer; sendMessage echoes the whole message
// back, so a long digest gets a long reply.
func botReply(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// longOK is a successful sendMessage reply echoing a long Cyrillic text.
func longOK(n int) string {
	return `{"ok":true,"result":{"message_id":7,"chat":{"id":42},"text":"` + strings.Repeat("Сводка ", n) + `","entities":[{"offset":0,"length":6,"type":"bold"}]}}`
}

// Production 2026-11-19 (msk, v0.9.3): a digest's reply was over 4096
// bytes, the cut body did not parse, and a delivered message was reported
// as "telegram: 200 OK".
func TestSendTelegramLongSuccessfulReply(t *testing.T) {
	body := longOK(2000)
	if len(body) <= 4096 {
		t.Fatal("fixture too short")
	}
	if err := sendTelegramTo(context.Background(), botReply(t, 200, body), "TOKEN", "42", "x"); err != nil {
		t.Fatalf("a delivered message reported as failed: %v", err)
	}
}

func TestSendTelegramReplies(t *testing.T) {
	for _, c := range []struct {
		name    string
		status  int
		body    string
		wantErr string // "" = sent
	}{
		{"long ok", 200, longOK(200000), ""}, // over the 1 MiB read cap: still sent
		{"refused with a reason", 400, `{"ok":false,"error_code":400,"description":"Bad Request: message is too long"}`, "telegram: Bad Request: message is too long"},
		{"refused without a body", 502, ``, "telegram: 502 Bad Gateway"},
		{"refused, body not JSON", 502, `<html>bad gateway</html>`, "telegram: 502 Bad Gateway"},
		{"200 but ok:false", 200, `{"ok":false,"description":"odd"}`, "telegram: odd"},
		{"200, body cut", 200, `{"ok":true,"result":{"text":"Свод`, ""}, // Bot API accepted it
		{"200, empty body", 200, ``, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := sendTelegramTo(context.Background(), botReply(t, c.status, c.body), "SECRET-TOKEN", "42", "x")
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("want sent, got %v", err)
			case c.wantErr != "" && (err == nil || err.Error() != c.wantErr):
				t.Fatalf("want %q, got %v", c.wantErr, err)
			}
		})
	}
}
