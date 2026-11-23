package templates

import (
	"bytes"
	"context"
	"regexp"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

// "send a test message" and "send the digest now" were offered with no saved
// channel, and the handlers refused them. They are disabled until a bot token
// and a chat are stored.
func TestNotificationsButtonsNeedSavedChannel(t *testing.T) {
	button := func(v NotifyView, form string) string {
		var buf bytes.Buffer
		if err := Notifications(db.Organization{ID: 1}, "owner", v).Render(context.Background(), &buf); err != nil {
			t.Fatal(err)
		}
		return regexp.MustCompile(`<button[^>]*form="` + form + `"[^>]*>`).FindString(buf.String())
	}
	for _, form := range []string{"notif-test", "notif-digest"} {
		if b := button(NotifyView{}, form); b == "" || !regexp.MustCompile(`\sdisabled`).MatchString(b) {
			t.Errorf("%s with no channel: want a disabled button, got %q", form, b)
		}
		if b := button(NotifyView{HasToken: true, ChatID: "42", DigestEnabled: true}, form); b == "" || regexp.MustCompile(`\sdisabled`).MatchString(b) {
			t.Errorf("%s with a saved channel: want an enabled button, got %q", form, b)
		}
	}
}
