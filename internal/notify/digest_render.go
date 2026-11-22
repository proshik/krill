package notify

import (
	"context"
	"fmt"
	"html"
	"sort"
	"strings"
	"time"

	"github.com/proshik/krill/internal/web/i18n"
)

// The collapsed node details are one Telegram expandable quote spanning
// several lines; splitParts keeps it balanced across message parts.
const (
	quoteOpen  = "<blockquote expandable>"
	quoteClose = "</blockquote>"
)

// digestLines accumulates the digest's lines (Telegram HTML, every tag but
// the node quote opened and closed on one line).
type digestLines struct {
	lines []string
}

func (b *digestLines) add(format string, args ...any) {
	b.lines = append(b.lines, fmt.Sprintf(format, args...))
}

// group adds a bold "project · environment" heading.
func (b *digestLines) group(project, env string) {
	if g := groupLine(project, env); g != "" {
		b.add("<b>%s</b>", html.EscapeString(g))
	}
}

// renderDigest renders the digest as Telegram HTML, split into parts at line
// boundaries.
func renderDigest(d digestData) []string {
	ctx := msgCtx(d.Lang)
	esc := html.EscapeString
	b := &digestLines{}

	b.add("<b>%s</b>", esc(i18n.T(ctx, "tg.dg.title")))
	when := dateLabel(d.Now, d.Loc, d.Lang) + " (" + d.Loc.String() + ")"
	if d.Instance != "" {
		when = d.Instance + " · " + when
	}
	b.add("%s", esc(when))
	if att := attention(ctx, d); len(att) > 0 {
		b.add("")
		b.add("%s <b>%s</b>", att[0].lvl.mark(), esc(i18n.T(ctx, "tg.dg.attention")))
		for i, a := range att {
			if i == digestListMax {
				b.add("%s", esc(i18n.Tf(ctx, "tg.dg.more", len(att)-i)))
				break
			}
			b.add("%s %s", a.lvl.mark(), esc(a.text))
		}
	} else {
		b.add("")
		b.add("%s %s", lvlOK.mark(), esc(i18n.T(ctx, "tg.dg.all_ok")))
	}

	renderDeploys(ctx, b, d)
	renderBackups(ctx, b, d)
	if d.Infra {
		renderNodes(ctx, b, d)
	}
	if d.Infra && d.State != nil {
		b.add("")
		b.add("<b>%s</b>", esc(i18n.T(ctx, "tg.dg.state")))
		for _, l := range stateLines(ctx, d) {
			if l.lvl == nil {
				b.add("%s", esc(l.text))
			} else {
				b.add("%s %s", l.lvl.mark(), esc(l.text))
			}
		}
	}
	renderApps(ctx, b, d)
	return splitParts(b.lines, digestPartLimit)
}

// summary is a section's "N, all succeeded" / "N: x ok, y failed", or
// "none" with nothing to report.
func summary(ctx context.Context, ok, failed int) string {
	switch {
	case ok+failed == 0:
		return i18n.T(ctx, "tg.dg.none")
	case failed == 0:
		return i18n.Tf(ctx, "tg.dg.sum_ok", ok)
	}
	return i18n.Tf(ctx, "tg.dg.sum_mixed", ok+failed, ok, failed)
}

// pluralForm is the CLDR cardinal category of n the catalogs carry: "one",
// "few" (Russian 2–4) or "many".
func pluralForm(lang string, n int) string {
	if lang != "ru" {
		if n == 1 {
			return "one"
		}
		return "many"
	}
	switch m10, m100 := n%10, n%100; {
	case m10 == 1 && m100 != 11:
		return "one"
	case m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14):
		return "few"
	}
	return "many"
}

// countOf renders "n <noun>" with the noun in the right plural form
// (key.one / key.few / key.many, each a %d format).
func countOf(ctx context.Context, lang, key string, n int) string {
	return i18n.Tf(ctx, key+"."+pluralForm(lang, n), n)
}

// appDeploys is one app's deploys in the digest window.
type appDeploys struct {
	name, url string
	ok        int
	last      DeploymentInfo // the newest successful one
	failed    []DeploymentInfo
}

// deployVersion is how a digest or a message names what a deploy shipped:
// its recorded version, or the shortened image of an older deployment.
func deployVersion(dep DeploymentInfo) string {
	if dep.Version != "" {
		return dep.Version
	}
	return shortImage(dep.ImageTag)
}

// renderDeploys lists the window's deploys collapsed per app: one line for
// an app's successful deploys (how many, the version it runs now and when it
// went out) and a line of its own for every failed one.
func renderDeploys(ctx context.Context, b *digestLines, d digestData) {
	esc := html.EscapeString
	type gkey struct{ project, env string }
	groups := map[gkey]map[int64]*appDeploys{}
	okN, errN := 0, 0
	for _, dep := range d.Deploys {
		k := gkey{dep.Project, dep.Env}
		if groups[k] == nil {
			groups[k] = map[int64]*appDeploys{}
		}
		a := groups[k][dep.AppID]
		if a == nil {
			a = &appDeploys{name: dep.App, url: appURL(d.Base, dep.OrgID, dep.ProjectID, dep.EnvID, dep.AppID)}
			groups[k][dep.AppID] = a
		}
		if dep.Status == "error" {
			errN++
			a.failed = append(a.failed, dep)
			continue
		}
		okN++
		a.ok++
		if !dep.FinishedAt.Before(a.last.FinishedAt) {
			a.last = dep
		}
	}
	b.add("")
	b.add("🚀 <b>%s</b>: %s", esc(i18n.T(ctx, "tg.dg.deploys")), esc(summary(ctx, okN, errN)))
	keys := make([]gkey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].project != keys[j].project {
			return keys[i].project < keys[j].project
		}
		return keys[i].env < keys[j].env
	})
	shown, total := 0, 0
	for _, k := range keys {
		for _, a := range groups[k] {
			total += len(a.failed)
			if a.ok > 0 {
				total++
			}
		}
	}
	for _, k := range keys {
		if shown == digestListMax {
			break
		}
		apps := make([]*appDeploys, 0, len(groups[k]))
		for _, a := range groups[k] {
			apps = append(apps, a)
		}
		sort.Slice(apps, func(i, j int) bool { return apps[i].name < apps[j].name })
		b.group(k.project, k.env)
		for _, a := range apps {
			if a.ok > 0 && shown < digestListMax {
				count := countOf(ctx, d.Lang, "tg.dg.n_deploys", a.ok)
				at := whenLabel(a.last.FinishedAt, d.Now, d.Loc, d.Lang)
				var text string
				if v := deployVersion(a.last); v != "" {
					text = i18n.Tf(ctx, "tg.dg.dep_now", count, v, at)
				} else {
					text = i18n.Tf(ctx, "tg.dg.dep_last", count, at)
				}
				b.add("%s %s — %s", lvlOK.mark(), linkName(a.url, a.name), esc(text))
				shown++
			}
			for _, f := range a.failed {
				if shown == digestListMax {
					break
				}
				parts := []string{}
				if v := deployVersion(f); v != "" {
					parts = append(parts, v)
				}
				parts = append(parts, whenLabel(f.FinishedAt, d.Now, d.Loc, d.Lang))
				if e := strings.TrimSpace(f.Error); e != "" {
					parts = append(parts, oneLine(e, 120))
				}
				b.add("%s %s — %s", lvlCrit.mark(), linkName(a.url, a.name), esc(strings.Join(parts, " · ")))
				shown++
			}
		}
	}
	if shown < total {
		b.add("%s", esc(i18n.Tf(ctx, "tg.dg.more", total-shown)))
	}
}

// renderBackups lists the window's backup runs grouped by project and
// environment, in the order they finished.
func renderBackups(ctx context.Context, b *digestLines, d digestData) {
	esc := html.EscapeString
	okN, errN := 0, 0
	type gkey struct{ project, env string }
	var keys []gkey
	groups := map[gkey][]BackupRun{}
	for _, r := range d.Runs {
		if r.Status == "error" {
			errN++
		} else {
			okN++
		}
		k := gkey{r.Project, r.Env}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], r)
	}
	b.add("")
	b.add("💾 <b>%s</b>: %s", esc(i18n.T(ctx, "tg.dg.backups")), esc(summary(ctx, okN, errN)))
	sort.SliceStable(keys, func(i, j int) bool {
		if keys[i].project != keys[j].project {
			return keys[i].project < keys[j].project
		}
		return keys[i].env < keys[j].env
	})
	shown := 0
	for _, k := range keys {
		if shown == digestListMax {
			break
		}
		b.group(k.project, k.env)
		for _, r := range groups[k] {
			if shown == digestListMax {
				break
			}
			parts := []string{kindLabel(ctx, r.Kind) + " " + r.Target}
			if r.Status != "error" && r.SizeBytes > 0 {
				parts = append(parts, humanBytes(r.SizeBytes))
			}
			parts = append(parts, whenLabel(r.FinishedAt, d.Now, d.Loc, d.Lang))
			if r.Status == "error" && strings.TrimSpace(r.Error) != "" {
				parts = append(parts, oneLine(r.Error, 120))
			}
			mark := lvlOK.mark()
			if r.Status == "error" {
				mark = lvlCrit.mark()
			}
			b.add("%s %s", mark, esc(strings.Join(parts, " · ")))
			shown++
		}
	}
	if shown < len(d.Runs) {
		b.add("%s", esc(i18n.Tf(ctx, "tg.dg.more", len(d.Runs)-shown)))
	}
}

// nodeShort is a node's figures in one line: the fullest disk, container
// memory and the sum of limits against the node, and CPU.
func (d digestData) nodeShort(ctx context.Context, n nodeRow) string {
	parts := []string{}
	if len(n.Disks) == 0 {
		parts = append(parts, i18n.T(ctx, "tg.dg.short_disk_unknown"))
	} else {
		top := 0.0
		for _, dk := range n.Disks {
			if p := dk.UsedPct(); p > top {
				top = p
			}
		}
		parts = append(parts, i18n.Tf(ctx, "tg.dg.short_disk", top))
	}
	parts = append(parts, i18n.Tf(ctx, "tg.dg.short_mem", pct(n.MemUsed, n.MemTotal), pct(n.Limits, n.MemTotal)))
	if n.CPUKnown {
		parts = append(parts, i18n.Tf(ctx, "tg.dg.short_cpu", n.CPUAvg, n.CPUPeak))
	}
	return strings.Join(parts, " · ")
}

// renderNodes writes one heading per node with its figures in short (and,
// for a yellow or red one, what made it so), then every node's full lines in
// one collapsed quote.
func renderNodes(ctx context.Context, b *digestLines, d digestData) {
	esc := html.EscapeString
	b.add("")
	b.add("🖥 <b>%s</b>", esc(i18n.T(ctx, "tg.dg.nodes")))
	if len(d.Nodes) == 0 {
		b.add("%s", esc(i18n.T(ctx, "tg.dg.no_data")))
		return
	}
	for _, n := range d.Nodes {
		mark := d.nodeLevel(n).mark()
		if n.Stale {
			b.add("%s <b>%s</b> — %s", mark, esc(n.Name), esc(i18n.T(ctx, "tg.dg.stale")))
			continue // its last figures would read as current
		}
		b.add("%s <b>%s</b> — %s", mark, esc(n.Name), esc(d.nodeShort(ctx, n)))
		if why := d.nodeWhy(ctx, n); why != "" {
			b.add("↳ %s", esc(why))
		}
	}
	// metric is a figure's full line, marked only when it is not green, so
	// the line behind a yellow or red node stands out.
	var details []string
	metric := func(l level, text string) {
		if l > lvlOK {
			details = append(details, l.mark()+" "+esc(text))
		} else {
			details = append(details, esc(text))
		}
	}
	for _, n := range d.Nodes {
		if n.Stale {
			continue
		}
		details = append(details, "<b>"+esc(n.Name)+"</b>")
		for _, dk := range n.Disks {
			metric(d.diskLevel(dk), i18n.Tf(ctx, "tg.dg.node_disk", dk.Mount, dk.UsedPct(), bytesPair(ctx, "tg.pair", dk.Used, dk.Total), humanBytes(dk.Avail)))
		}
		if len(n.Disks) == 0 {
			metric(lvlWarn, i18n.T(ctx, "tg.dg.node_disk_unknown"))
		}
		mem := i18n.Tf(ctx, "tg.dg.node_mem", bytesPair(ctx, "tg.pair", n.MemUsed, n.MemTotal), humanBytes(n.Limits), pct(n.Limits, n.MemTotal))
		if n.Unlimited > 0 {
			mem += " · " + i18n.Tf(ctx, "tg.dg.unlimited", n.Unlimited)
		}
		metric(worst(d.limitsLevel(n), memUsedLevel(n)), mem)
		if n.CPUKnown {
			metric(cpuLevel(n), i18n.Tf(ctx, "tg.dg.node_cpu", n.CPUAvg, n.CPUPeak, n.NCPU))
		}
	}
	if len(details) == 0 {
		return
	}
	details[0] = quoteOpen + details[0]
	details[len(details)-1] += quoteClose
	b.lines = append(b.lines, details...)
}

// renderApps lists each app's memory grouped by project and environment,
// then the apps without fresh data by what Swarm says about them.
func renderApps(ctx context.Context, b *digestLines, d digestData) {
	esc := html.EscapeString
	b.add("")
	b.add("📦 <b>%s</b>", esc(i18n.T(ctx, "tg.dg.apps")))
	var project, env string
	for i, a := range d.Apps {
		if i == 0 || a.Project != project || a.Env != env {
			project, env = a.Project, a.Env
			b.group(project, env)
		}
		var text string
		if a.Limit > 0 {
			text = i18n.Tf(ctx, "tg.dg.app_mem_limit", humanBytes(a.Now), bytesPair(ctx, "tg.pair", a.Peak, a.Limit), pct(a.Peak, a.Limit))
		} else {
			text = i18n.Tf(ctx, "tg.dg.app_mem", humanBytes(a.Now), humanBytes(a.Peak))
		}
		if d.RestartsKnown && a.Restarts > 0 {
			text += " · " + i18n.Tf(ctx, "tg.dg.app_restarts", a.Restarts)
		}
		b.add("%s %s — %s", a.level().mark(), linkName(a.URL, a.Name), esc(text))
	}
	names := func(mark, key string, list []string) {
		if len(list) == 0 {
			return
		}
		more := ""
		if len(list) > digestListMax {
			list, more = list[:digestListMax], " "+i18n.Tf(ctx, "tg.dg.more", len(list)-digestListMax)
		}
		b.add("%s %s", mark, esc(i18n.T(ctx, key)+": "+strings.Join(list, ", ")+more))
	}
	names(lvlCrit.mark(), "tg.dg.down", d.Down)
	names("⚪", "tg.dg.no_samples", d.NoData)
	names("⚪", "tg.dg.stopped", d.Stopped)
	if len(d.Apps) == 0 && len(d.Down) == 0 && len(d.NoData) == 0 && len(d.Stopped) == 0 {
		b.add("%s", esc(i18n.T(ctx, "tg.dg.no_data")))
	}
}

// dateLabel is the digest's own time: "22.11, 11:00" / "Nov 22, 11:00".
func dateLabel(t time.Time, loc *time.Location, lang string) string {
	l := t.In(loc)
	if lang == "ru" {
		return l.Format("02.01, 15:04")
	}
	return l.Format("Jan 2, 15:04")
}

// cutLine shortens an escaped line to at most limit runes without cutting
// inside an HTML entity or tag, and closes the inline tags (<b>, <a>) the
// cut left open: Telegram rejects the whole message otherwise. The node
// quote is not closed here; splitParts balances it.
func cutLine(r []rune, limit int) string {
	cut := string(r[:limit])
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	return cut + closeInlineTags(cut)
}

// closeInlineTags returns the closing tags for the <a> and <b> elements s
// leaves open, innermost first.
func closeInlineTags(s string) string {
	var open []string
	for {
		lt := strings.Index(s, "<")
		if lt < 0 {
			break
		}
		gt := strings.Index(s[lt:], ">")
		if gt < 0 {
			break
		}
		tag := s[lt+1 : lt+gt]
		s = s[lt+gt+1:]
		closing := strings.HasPrefix(tag, "/")
		name, _, _ := strings.Cut(strings.TrimPrefix(tag, "/"), " ")
		if name != "a" && name != "b" {
			continue
		}
		if !closing {
			open = append(open, name)
		} else if len(open) > 0 && open[len(open)-1] == name {
			open = open[:len(open)-1]
		}
	}
	var out strings.Builder
	for i := len(open) - 1; i >= 0; i-- {
		out.WriteString("</" + open[i] + ">")
	}
	return out.String()
}

// oneLine flattens and shortens an error message for a digest line.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

// splitParts joins lines into messages of at most limit characters, never
// splitting a line (a longer single line is cut). A part boundary inside
// the node quote closes it at the end of the part and opens it again at the
// start of the next one, so every part is balanced HTML.
func splitParts(lines []string, limit int) []string {
	var parts []string
	var b strings.Builder
	size := 0 // runes in b
	inQuote := false
	flush := func() {
		text := strings.TrimRight(b.String(), "\n")
		if inQuote {
			text += quoteClose
		}
		if strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		}
		b.Reset()
		size = 0
	}
	for _, l := range lines {
		if size == 0 && l == "" {
			continue // a part never starts with a blank line
		}
		// A line inside the quote may get it reopened in front and closed
		// behind; leave room for both.
		lineLimit := limit
		if inQuote || strings.Contains(l, quoteOpen) {
			lineLimit = max(limit-len(quoteOpen)-len(quoteClose), 1)
		}
		if r := []rune(l); len(r) > lineLimit {
			l = cutLine(r, lineLimit)
		}
		after := inQuote
		if strings.Contains(l, quoteOpen) {
			after = true
		}
		if strings.Contains(l, quoteClose) {
			after = false
		}
		n := len([]rune(l))
		reserve := 0
		if after {
			reserve = len(quoteClose)
		}
		if size > 0 && size+1+n+reserve > limit {
			flush()
		}
		if size == 0 && inQuote {
			l = quoteOpen + l
			n += len(quoteOpen)
		}
		if size > 0 {
			b.WriteByte('\n')
			size++
		}
		b.WriteString(l)
		size += n
		inQuote = after
	}
	if b.Len() > 0 {
		flush()
	}
	return parts
}
