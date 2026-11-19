package notify

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/proshik/krill/internal/web/i18n"
)

// msgCtx is a context carrying only the channel's language, for the i18n
// catalogs (messages are rendered off any request).
func msgCtx(lang string) context.Context {
	if !i18n.Supported(lang) {
		lang = i18n.DefaultLocale
	}
	return i18n.WithLocale(context.Background(), lang)
}

// format renders an Event as a Telegram HTML message in the channel's
// language. All interpolated user content is HTML-escaped (parse_mode=HTML).
func format(ev Event, lang string) string {
	ctx := msgCtx(lang)
	var head string
	switch ev.Kind {
	case DeployFailed:
		head = i18n.T(ctx, "tg.deploy_failed")
	case BackupFailed:
		head = i18n.T(ctx, "tg.backup_failed")
	case AppDown:
		head = i18n.T(ctx, "tg.app_down")
	case AppRecovered:
		head = i18n.T(ctx, "tg.app_recovered")
	case MigrateFailed:
		head = i18n.T(ctx, "tg.migrate_failed")
	case DeploySucceeded:
		head = i18n.T(ctx, "tg.deploy_ok")
	case DiskHigh:
		head = i18n.T(ctx, "tg.disk_high")
	case DiskRecovered:
		head = i18n.T(ctx, "tg.disk_ok")
	case MemOvercommit:
		head = i18n.T(ctx, "tg.mem_over")
	default:
		head = i18n.T(ctx, "tg.notification")
	}
	if ev.Instance != "" {
		head += " · " + ev.Instance
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>", html.EscapeString(head))
	if p := targetPath(ev.Project, ev.Env, ev.Target); p != "" {
		fmt.Fprintf(&b, "\n%s", html.EscapeString(p))
	}
	switch {
	case ev.Deploy != nil:
		parts := []string{}
		if ev.Deploy.Image != "" {
			parts = append(parts, i18n.T(ctx, "tg.image")+" "+ev.Deploy.Image)
		}
		if ev.Deploy.Duration > 0 {
			parts = append(parts, ev.Deploy.Duration.Round(time.Second).String())
		}
		if tr := triggerLabel(ctx, ev.Deploy.Trigger); tr != "" {
			parts = append(parts, tr)
		}
		if len(parts) > 0 {
			fmt.Fprintf(&b, "\n%s", html.EscapeString(strings.Join(parts, " · ")))
		}
	case ev.Disk != nil:
		d := ev.Disk
		fmt.Fprintf(&b, "\n%s", html.EscapeString(i18n.Tf(ctx, "tg.disk_line",
			d.Node, d.Mount, d.Pct, bytesPair(ctx, "tg.pair", d.Used, d.Total), humanBytes(d.Avail), d.Threshold)))
	case ev.Mem != nil:
		m := ev.Mem
		fmt.Fprintf(&b, "\n%s", html.EscapeString(i18n.Tf(ctx, "tg.mem_line",
			m.Node, humanBytes(m.Limits), m.Pct, humanBytes(m.Total), m.Threshold)))
		if m.App != "" {
			fmt.Fprintf(&b, "\n%s", html.EscapeString(i18n.Tf(ctx, "tg.mem_after", m.App)))
		}
		if m.Unlimited > 0 {
			fmt.Fprintf(&b, "\n%s", html.EscapeString(i18n.Tf(ctx, "tg.mem_unlimited", m.Unlimited)))
		}
	}
	if d := strings.TrimSpace(ev.Detail); d != "" {
		fmt.Fprintf(&b, "\n%s", html.EscapeString(d))
	}
	return b.String()
}

// targetPath joins the non-empty parts of project/env/target.
func targetPath(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "/")
}

// triggerLabel names what started a deploy; an unknown trigger is shown raw.
func triggerLabel(ctx context.Context, trigger string) string {
	if trigger == "" {
		return ""
	}
	key := "tg.trigger." + trigger
	if s := i18n.T(ctx, key); s != key {
		return s
	}
	return trigger
}

// shortImage trims a deployed image reference for a phone screen: the
// registry and repository path go, a digest shrinks to 12 characters (and is
// dropped when a tag is present), and an image Krill built itself
// ("krill-<app>:<deployment>") becomes "#<deployment>".
func shortImage(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	name, digest, _ := strings.Cut(ref, "@")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	repo, tag, hasTag := strings.Cut(name, ":")
	if hasTag && strings.HasPrefix(repo, "krill-") {
		return "#" + tag
	}
	if !hasTag && digest != "" {
		d := strings.TrimPrefix(digest, "sha256:")
		if len(d) > 12 {
			d = d[:12]
		}
		return name + "@" + d
	}
	return name
}

// humanBytes formats a byte count with binary units, as docker stats does.
// bytesPair renders "a of b" through the i18n key (two %s), writing the
// unit once when both sizes share it: "12.9 of 148 GiB", but
// "584 MiB of 1.5 GiB".
func bytesPair(ctx context.Context, key string, a, b int64) string {
	as, bs := humanBytes(a), humanBytes(b)
	if ai, bi := strings.LastIndexByte(as, ' '), strings.LastIndexByte(bs, ' '); ai > 0 && bi > 0 && as[ai:] == bs[bi:] {
		as = as[:ai]
	}
	return i18n.Tf(ctx, key, as, bs)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	v := float64(n) / unit
	i := 0
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	if v >= 100 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
