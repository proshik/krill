package templates

import "testing"

func TestParseDigestBoldAndParts(t *testing.T) {
	lines := ParseDigest([]string{"<b>Daily · harbor</b>\n🟢 api &lt;b&gt;x&lt;/b&gt; <i>y</i>", "second"})
	if len(lines) != 4 {
		t.Fatalf("want 2 lines, a part break and 1 line, got %d: %+v", len(lines), lines)
	}
	if len(lines[0].Runs) != 1 || !lines[0].Runs[0].Bold || lines[0].Runs[0].Text != "Daily · harbor" {
		t.Fatalf("title: %+v", lines[0])
	}
	// Escaped markup stays text; a tag the preview does not draw is dropped,
	// its text kept.
	if got := lines[1].Runs[0].Text; got != "🟢 api <b>x</b> y" || lines[1].Runs[0].Bold {
		t.Fatalf("body: %+v", lines[1])
	}
	if !lines[2].Break || lines[3].Runs[0].Text != "second" {
		t.Fatalf("parts: %+v", lines[2:])
	}
}

// The digest links apps and folds long lists into an expandable quote; the
// preview keeps the link (http and https only) and marks the quoted lines.
func TestParseDigestLinksAndQuotes(t *testing.T) {
	lines := ParseDigest([]string{
		"<b>side · prod</b>\n🔴 Not running: <a href=\"https://krill.example.com/orgs/7/apps/4\">demo-app</a>\n" +
			"<blockquote expandable>first\nsecond</blockquote>\n<a href=\"javascript:alert(1)\">bad</a>",
	})
	if len(lines) != 5 {
		t.Fatalf("lines: %+v", lines)
	}
	link := lines[1].Runs[len(lines[1].Runs)-1]
	if link.Text != "demo-app" || link.Href != "https://krill.example.com/orgs/7/apps/4" {
		t.Fatalf("link: %+v", lines[1])
	}
	if lines[1].Quote || !lines[2].Quote || !lines[3].Quote || lines[4].Quote {
		t.Fatalf("quote marks: %+v", lines)
	}
	if lines[2].Runs[0].Text != "first" || lines[3].Runs[0].Text != "second" {
		t.Fatalf("quoted text: %+v", lines[2:4])
	}
	if r := lines[4].Runs[0]; r.Href != "" || r.Text != "bad" {
		t.Fatalf("a non-http link must stay plain text: %+v", r)
	}
}
