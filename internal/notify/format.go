package notify

import (
	"fmt"
	"html"
	"strings"
)

// format renders an Event as a Telegram HTML message. All interpolated user
// content is HTML-escaped (parse_mode=HTML).
func format(ev Event) string {
	var head string
	switch ev.Kind {
	case DeployFailed:
		head = "🔴 Deploy failed"
	case BackupFailed:
		head = "🔴 Backup failed"
	case AppDown:
		head = "🔴 App down"
	case AppRecovered:
		head = "🟢 App recovered"
	default:
		head = "Notification"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>\n", html.EscapeString(head))
	fmt.Fprint(&b, html.EscapeString(ev.Project+"/"+ev.Env+"/"+ev.Target))
	if d := strings.TrimSpace(ev.Detail); d != "" {
		fmt.Fprintf(&b, "\n%s", html.EscapeString(d))
	}
	return b.String()
}
