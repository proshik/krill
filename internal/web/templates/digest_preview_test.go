package templates

import "testing"

func TestParseDigestKeepsOnlyBold(t *testing.T) {
	lines := ParseDigest([]string{"<b>Daily · harbor</b>\n🟢 api &lt;b&gt;x&lt;/b&gt; <i>y</i>", "second"})
	if len(lines) != 4 {
		t.Fatalf("want 2 lines, a part break and 1 line, got %d: %+v", len(lines), lines)
	}
	if len(lines[0]) != 1 || !lines[0][0].Bold || lines[0][0].Text != "Daily · harbor" {
		t.Fatalf("title: %+v", lines[0])
	}
	// Escaped markup stays text, and any tag other than <b> is shown literally.
	if got := lines[1][0].Text; got != "🟢 api <b>x</b> <i>y</i>" || lines[1][0].Bold {
		t.Fatalf("body: %+v", lines[1])
	}
	if lines[2] != nil || lines[3][0].Text != "second" {
		t.Fatalf("parts: %+v", lines[2:])
	}
}
