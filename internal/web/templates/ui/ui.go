// Package ui holds the design system's small shared pieces: the icon sprite,
// status signals, meters and copy fields.
package ui

// SigKind normalizes a status kind to one the sprite draws.
func SigKind(kind string) string {
	switch kind {
	case "ok", "err", "warn", "idle":
		return kind
	}
	return "idle"
}

// GlyphKind is the plate kind of a card glyph: an app, a database or storage
// resource, or anything else (the gateway, the internet), drawn neutral.
func GlyphKind(id string) string {
	switch id {
	case "i-app":
		return "app"
	case "i-pg", "i-kv", "i-bucket":
		return "db"
	}
	return "sys"
}

func clamp(pct int) int {
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

// spriteSVG is the icon sprite, drawn once per page.
const spriteSVG = `<svg class="sprite" aria-hidden="true"><defs>
<symbol id="i-app" viewBox="0 0 16 16"><rect x="2.5" y="3" width="11" height="10" rx="2.5"/><path d="M2.5 6.5h11"/></symbol>
<symbol id="i-pg" viewBox="0 0 16 16"><ellipse cx="8" cy="4.5" rx="5" ry="2"/><path d="M3 4.5v7c0 1.1 2.2 2 5 2s5-.9 5-2v-7M3 8c0 1.1 2.2 2 5 2s5-.9 5-2"/></symbol>
<symbol id="i-kv" viewBox="0 0 16 16"><path d="M9 2L4 9h4l-1 5 5-7H8l1-5z"/></symbol>
<symbol id="i-bucket" viewBox="0 0 16 16"><path d="M2.5 5h11l-1.4 8a1 1 0 01-1 .8H4.9a1 1 0 01-1-.8L2.5 5z"/><path d="M4.5 5a3.5 2 0 017 0"/></symbol>
<symbol id="i-node" viewBox="0 0 16 16"><rect x="2.5" y="2.5" width="11" height="4.5" rx="1.5"/><rect x="2.5" y="9" width="11" height="4.5" rx="1.5"/><path d="M5 4.75h.01M5 11.25h.01"/></symbol>
<symbol id="s-ok" viewBox="0 0 10 10"><circle cx="5" cy="5" r="4"/></symbol>
<symbol id="s-err" viewBox="0 0 10 10"><path d="M5 .8L9.2 5 5 9.2.8 5z"/></symbol>
<symbol id="s-warn" viewBox="0 0 10 10"><path d="M5 1l4.4 8H.6z"/></symbol>
<symbol id="s-idle" viewBox="0 0 10 10"><circle cx="5" cy="5" r="3.3" fill="none" stroke="currentColor" stroke-width="1.4"/></symbol>
<symbol id="i-lock" viewBox="0 0 16 16"><rect x="3.5" y="7" width="9" height="6.5" rx="1.5"/><path d="M5.5 7V5a2.5 2.5 0 015 0v2"/></symbol>
<symbol id="i-unlock" viewBox="0 0 16 16"><rect x="3.5" y="7" width="9" height="6.5" rx="1.5"/><path d="M5.5 7V5a2.5 2.5 0 014.8-1"/></symbol>
<symbol id="i-trash" viewBox="0 0 16 16"><path d="M3 4.5h10M6.5 4.5V3h3v1.5M4.5 4.5l.6 8.2a1 1 0 001 .8h3.8a1 1 0 001-.8l.6-8.2"/></symbol>
<symbol id="i-eye" viewBox="0 0 16 16"><path d="M1.5 8S4 3.5 8 3.5 14.5 8 14.5 8 12 12.5 8 12.5 1.5 8 1.5 8z"/><circle cx="8" cy="8" r="2"/></symbol>
<symbol id="i-chev" viewBox="0 0 16 16"><path d="M4.5 6.2L8 9.8l3.5-3.6"/></symbol>
<symbol id="i-plus" viewBox="0 0 16 16"><path d="M8 3.5v9M3.5 8h9"/></symbol>
<symbol id="i-minus" viewBox="0 0 16 16"><path d="M3.5 8h9"/></symbol>
<symbol id="i-fit" viewBox="0 0 16 16"><path d="M2.5 6V3.5a1 1 0 011-1H6M10 2.5h2.5a1 1 0 011 1V6M13.5 10v2.5a1 1 0 01-1 1H10M6 13.5H3.5a1 1 0 01-1-1V10"/></symbol>
<symbol id="i-search" viewBox="0 0 16 16"><circle cx="7" cy="7" r="4.5"/><path d="M10.5 10.5L14 14"/></symbol>
<symbol id="i-close" viewBox="0 0 16 16"><path d="M4 4l8 8M12 4l-8 8"/></symbol>
<symbol id="i-expand" viewBox="0 0 16 16"><path d="M9.5 2.5h4v4M13.5 2.5L9 7M6.5 13.5h-4v-4M2.5 13.5L7 9"/></symbol>
<symbol id="i-collapse" viewBox="0 0 16 16"><path d="M9 3v4h4M9 7l4.5-4.5M7 13V9H3M7 9l-4.5 4.5"/></symbol>
<symbol id="i-link" viewBox="0 0 16 16"><path d="M6.5 9.5l3-3M7 4.5l1-1a2.8 2.8 0 014 4l-1 1M9 11.5l-1 1a2.8 2.8 0 01-4-4l1-1"/></symbol>
<symbol id="i-copy" viewBox="0 0 16 16"><rect x="5.5" y="5.5" width="7.5" height="7.5" rx="1.5"/><path d="M3 10.5V4a1 1 0 011-1h6.5"/></symbol>
<symbol id="i-menu" viewBox="0 0 16 16"><path d="M2.5 4.5h11M2.5 8h11M2.5 11.5h11"/></symbol>
<symbol id="i-term" viewBox="0 0 16 16"><rect x="2" y="3" width="12" height="10" rx="2"/><path d="M5 6.5l2 1.5-2 1.5M8.5 10h2.5"/></symbol>
<symbol id="i-down" viewBox="0 0 16 16"><path d="M8 2.5v8M4.5 7.5L8 11l3.5-3.5M3 13.5h10"/></symbol>
<symbol id="i-refresh" viewBox="0 0 16 16"><path d="M13 8a5 5 0 11-1.5-3.6M13 2.5v2.8h-2.8"/></symbol>
</defs></svg>`
