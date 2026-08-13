// Package logparse parses one raw docker log line (optionally prefixed with a
// docker --timestamps RFC3339 stamp) into a structured LogLine, detecting its
// level heuristically. It has no dependencies beyond the standard library, so
// both internal/server (the WebSocket log-streaming handlers) and internal/api
// (the agent-facing AppLogs operation) can import it without creating a cycle
// between them.
package logparse

import (
	"regexp"
	"strings"
	"time"
)

// LogLine is one parsed log line.
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
