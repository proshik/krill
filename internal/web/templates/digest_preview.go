package templates

import (
	"html"
	"strings"
)

// DigestLine is one line of a digest preview: text runs, some bold.
type DigestLine []DigestRun

// DigestRun is a run of text in a digest line.
type DigestRun struct {
	Text string
	Bold bool
}

// ParseDigest turns the digest's Telegram HTML (escaped text and <b> only)
// into lines of plain runs, so the page renders it as text and never as
// markup: anything but <b> and </b> is kept as literal text.
func ParseDigest(parts []string) []DigestLine {
	var out []DigestLine
	for pi, part := range parts {
		if pi > 0 {
			out = append(out, nil)
		}
		for _, raw := range strings.Split(part, "\n") {
			var line DigestLine
			bold := false
			for raw != "" {
				i := strings.Index(raw, "<")
				if i < 0 {
					line = appendRun(line, html.UnescapeString(raw), bold)
					break
				}
				if i > 0 {
					line = appendRun(line, html.UnescapeString(raw[:i]), bold)
				}
				switch {
				case strings.HasPrefix(raw[i:], "<b>"):
					bold, raw = true, raw[i+3:]
				case strings.HasPrefix(raw[i:], "</b>"):
					bold, raw = false, raw[i+4:]
				default:
					line = appendRun(line, "<", bold)
					raw = raw[i+1:]
				}
			}
			out = append(out, line)
		}
	}
	return out
}

func appendRun(line DigestLine, text string, bold bool) DigestLine {
	if text == "" {
		return line
	}
	if n := len(line); n > 0 && line[n-1].Bold == bold {
		line[n-1].Text += text
		return line
	}
	return append(line, DigestRun{Text: text, Bold: bold})
}
