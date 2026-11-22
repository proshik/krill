package notify

import (
	"context"
	"fmt"
	"html"
	"net/url"
	"strconv"
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
	case DBDown:
		head = i18n.T(ctx, "tg.db_down")
	case DBRecovered:
		head = i18n.T(ctx, "tg.db_recovered")
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
	case StateBackupFailed:
		head = i18n.T(ctx, "tg.state_backup_failed")
	default:
		head = i18n.T(ctx, "tg.notification")
	}
	if ev.Instance != "" {
		head += " · " + ev.Instance
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>", html.EscapeString(head))
	// The subject on its own line (a link to its page when there is one),
	// then where it lives.
	if ev.Target != "" {
		fmt.Fprintf(&b, "\n%s", linkName(ev.URL, clip(ev.Target)))
	}
	if p := placeLine(ctx, clip(ev.Project), clip(ev.Env)); p != "" {
		fmt.Fprintf(&b, "\n%s", html.EscapeString(p))
	}
	switch {
	case ev.Deploy != nil:
		parts := []string{}
		if ev.Deploy.Version != "" {
			parts = append(parts, i18n.T(ctx, "tg.version")+" "+clip(ev.Deploy.Version))
		} else if ev.Deploy.Image != "" {
			parts = append(parts, i18n.T(ctx, "tg.image")+" "+clip(ev.Deploy.Image))
		}
		if ev.Deploy.Duration > 0 {
			parts = append(parts, humanDuration(ctx, ev.Deploy.Duration))
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
	case ev.State != nil:
		if line := stateTriggerLine(ctx, ev.State.Trigger, ev.State.From, ev.State.To); line != "" {
			fmt.Fprintf(&b, "\n%s", html.EscapeString(line))
		}
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
		if r := []rune(d); len(r) > detailMax {
			d = strings.TrimSpace(string(r[:detailMax])) + " …"
		}
		fmt.Fprintf(&b, "\n%s", html.EscapeString(d))
	}
	return b.String()
}

// fieldMax caps a name, a place or a version in an alert, so that with the
// detail (detailMax) the message stays under Telegram's 4096 characters
// whatever a user named things.
const fieldMax = 200

// clip shortens s to fieldMax runes.
func clip(s string) string {
	if r := []rune(s); len(r) > fieldMax {
		return string(r[:fieldMax-1]) + "…"
	}
	return s
}

// placeLine names a subject's project and environment ("project p ·
// environment e"); "" when it has neither (a database server, a node).
func placeLine(ctx context.Context, project, env string) string {
	switch {
	case project != "" && env != "":
		return i18n.Tf(ctx, "tg.place", project, env)
	case project != "":
		return i18n.Tf(ctx, "tg.place_project", project)
	}
	return ""
}

// groupLine names a digest group: "project · environment".
func groupLine(project, env string) string {
	switch {
	case project != "" && env != "":
		return project + " · " + env
	case project != "":
		return project
	}
	return env
}

// linkName renders name as a link to href, or as plain text without one.
// Both are escaped (the href as an attribute value).
func linkName(href, name string) string {
	if href == "" {
		return html.EscapeString(name)
	}
	return `<a href="` + html.EscapeString(href) + `">` + html.EscapeString(name) + `</a>`
}

// linkBase returns base when it is an absolute https URL, without a trailing
// slash; "" otherwise. Messages link only to a public https address: a plain
// http or internal one is not worth sending to a phone.
func linkBase(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	u, err := url.Parse(base)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	return base
}

// appURL is an app's page under base ("" when base is empty or an id is
// missing).
func appURL(base string, org, project, env, app int64) string {
	if base == "" || org <= 0 || project <= 0 || env <= 0 || app <= 0 {
		return ""
	}
	return base + "/orgs/" + strconv.FormatInt(org, 10) + "/projects/" + strconv.FormatInt(project, 10) +
		"/environments/" + strconv.FormatInt(env, 10) + "/apps/" + strconv.FormatInt(app, 10)
}

// humanDuration renders a deploy's duration for a phone: "52s", "3m 5s",
// "1h 2m" (in the channel's language).
func humanDuration(ctx context.Context, d time.Duration) string {
	d = d.Round(time.Second)
	h, m, sec := int(d/time.Hour), int(d%time.Hour/time.Minute), int(d%time.Minute/time.Second)
	switch {
	case h > 0:
		return i18n.Tf(ctx, "tg.dur.hm", h, m)
	case m > 0 && sec == 0:
		return i18n.Tf(ctx, "tg.dur.m", m)
	case m > 0:
		return i18n.Tf(ctx, "tg.dur.ms", m, sec)
	}
	return i18n.Tf(ctx, "tg.dur.s", sec)
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

// detailMax caps an alert's detail (a deploy's or a backup's error, which
// may quote a tool's output): Telegram refuses a message over 4096
// characters, and the alert would then be lost altogether. The full error
// stays in the deployment's or the backup's record.
const detailMax = 1500

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

// humanBytes formats a byte count with binary units, as docker stats does.
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
