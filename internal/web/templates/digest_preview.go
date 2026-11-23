package templates

import (
	"html"
	"strings"
)

// DigestLine is one line of a digest preview. Break marks the gap between two
// messages of a long digest; Quote marks a line inside a quote block.
type DigestLine struct {
	Runs  []DigestRun
	Quote bool
	Break bool
}

// DigestRun is a run of text in a digest line: bold, or a link when Href is set.
type DigestRun struct {
	Text string
	Bold bool
	Href string
}

// ParseDigest turns the digest's Telegram HTML into lines of plain runs, so
// the page renders it as text and never as markup. It understands what the
// digest uses: <b>, <a href> (kept only for http and https addresses) and
// <blockquote>, which marks its lines as quoted. Any other tag is dropped and
// its text kept; escaped text stays text.
func ParseDigest(parts []string) []DigestLine {
	var out []DigestLine
	quote := false
	for pi, part := range parts {
		if pi > 0 {
			out = append(out, DigestLine{Break: true})
			quote = false
		}
		for _, raw := range strings.Split(part, "\n") {
			line := DigestLine{Quote: quote}
			bold, href := false, ""
			for raw != "" {
				i := strings.Index(raw, "<")
				if i < 0 {
					line.add(html.UnescapeString(raw), bold, href)
					break
				}
				if i > 0 {
					line.add(html.UnescapeString(raw[:i]), bold, href)
				}
				end := strings.Index(raw[i:], ">")
				if end < 0 { // not a tag: keep the rest as text
					line.add(html.UnescapeString(raw[i:]), bold, href)
					break
				}
				tag := raw[i+1 : i+end]
				raw = raw[i+end+1:]
				name, attrs, _ := strings.Cut(tag, " ")
				switch name {
				case "b":
					bold = true
				case "/b":
					bold = false
				case "a":
					href = linkTarget(attrs)
				case "/a":
					href = ""
				case "blockquote":
					quote, line.Quote = true, true
				case "/blockquote":
					quote = false
				}
			}
			out = append(out, line)
		}
	}
	return out
}

// linkTarget reads href="…" from an <a> tag's attributes and returns it when
// it is an http or https address, "" otherwise.
func linkTarget(attrs string) string {
	_, v, ok := strings.Cut(attrs, `href="`)
	if !ok {
		return ""
	}
	v, _, ok = strings.Cut(v, `"`)
	if !ok {
		return ""
	}
	v = html.UnescapeString(v)
	if l := strings.ToLower(v); strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") {
		return v
	}
	return ""
}

func (l *DigestLine) add(text string, bold bool, href string) {
	if text == "" {
		return
	}
	if n := len(l.Runs); n > 0 && l.Runs[n-1].Bold == bold && l.Runs[n-1].Href == href {
		l.Runs[n-1].Text += text
		return
	}
	l.Runs = append(l.Runs, DigestRun{Text: text, Bold: bold, Href: href})
}
