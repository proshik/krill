package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// LogLine is one parsed log line, sent to the client as a JSON frame.
type LogLine struct {
	Time  string `json:"t"`   // docker RFC3339 timestamp, or "" if absent
	Level string `json:"lvl"` // normalized level, or "" if unknown
	Msg   string `json:"msg"` // the application's line, verbatim
}

var (
	logfmtLevelRe = regexp.MustCompile(`(?i)(?:^|\s)level=("?)([a-zA-Z]+)`)
	tokenLevelRe  = regexp.MustCompile(`(?i)\b(TRACE|DEBUG|INFO|WARN|WARNING|ERROR|ERR|FATAL|SUCCESS)\b`)
)

// ParseLogLine strips an optional leading docker RFC3339 timestamp, detects a
// log level heuristically, and returns the remainder as the message.
func ParseLogLine(raw string) LogLine {
	ts, rest := splitDockerTimestamp(raw)
	return LogLine{Time: ts, Level: detectLevel(rest), Msg: rest}
}

// splitDockerTimestamp peels a leading "2026-06-07T19:31:52.307218Z " token
// (docker --timestamps) off the line. Tolerant of lines without one.
func splitDockerTimestamp(raw string) (ts, rest string) {
	sp := strings.IndexByte(raw, ' ')
	if sp <= 0 {
		return "", raw
	}
	tok := raw[:sp]
	if _, err := time.Parse(time.RFC3339Nano, tok); err != nil {
		return "", raw
	}
	return tok, raw[sp+1:]
}

func detectLevel(s string) string {
	if m := logfmtLevelRe.FindStringSubmatch(s); m != nil {
		return normalizeLevel(m[2])
	}
	head := s
	if len(head) > 64 {
		head = head[:64]
	}
	if m := tokenLevelRe.FindStringSubmatch(head); m != nil {
		return normalizeLevel(m[1])
	}
	return ""
}

func normalizeLevel(l string) string {
	switch strings.ToLower(l) {
	case "trace":
		return "trace"
	case "debug":
		return "debug"
	case "info":
		return "info"
	case "success":
		return "success"
	case "warn", "warning":
		return "warn"
	case "error", "err":
		return "error"
	case "fatal":
		return "fatal"
	}
	return ""
}

// scanEndNotice describes why a log scan stopped, for the viewer. A clean end
// of stream needs no notice; anything else does — a silent stop is worse than
// an error, because the panel keeps looking live while showing nothing.
func scanEndNotice(err error) (LogLine, bool) {
	if err == nil {
		return LogLine{}, false
	}
	if errors.Is(err, bufio.ErrTooLong) {
		return LogLine{Level: "error", Msg: "log stream stopped: a single log line exceeded the 1 MiB limit"}, true
	}
	return LogLine{Level: "error", Msg: "log stream stopped: " + err.Error()}, true
}

// streamParsedLogsToWS reads lines from rc, parses each into a LogLine, and
// sends it as a JSON text frame until the reader is exhausted or a write fails.
func streamParsedLogsToWS(ctx context.Context, conn *websocket.Conn, rc io.Reader) {
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // tolerate long log lines
	for sc.Scan() {
		b, err := json.Marshal(ParseLogLine(sc.Text()))
		if err != nil {
			continue
		}
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		werr := conn.Write(wctx, websocket.MessageText, b)
		cancel()
		if werr != nil {
			return
		}
	}
	if notice, ok := scanEndNotice(sc.Err()); ok {
		if b, err := json.Marshal(notice); err == nil {
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_ = conn.Write(wctx, websocket.MessageText, b)
			cancel()
		}
		conn.Close(websocket.StatusInternalError, "log stream broken")
		return
	}
	conn.Close(websocket.StatusNormalClosure, "")
}
