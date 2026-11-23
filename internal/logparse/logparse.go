// Package logparse parses one raw docker log line (optionally prefixed with a
// docker --timestamps RFC3339 stamp) into a structured LogLine, detecting its
// level heuristically. It has no dependencies beyond the standard library, so
// both internal/server (the WebSocket log-streaming handlers) and internal/api
// (the agent-facing AppLogs operation) can import it without creating a cycle
// between them.
package logparse

import (
	"bufio"
	"errors"
	"regexp"
	"strings"
	"time"
)

// LogLine is one parsed log line.
type LogLine struct {
	Time  string `json:"t"`   // docker RFC3339 timestamp, or "" if absent
	Level string `json:"lvl"` // normalized level, or "" if unknown
	Msg   string `json:"msg"` // the application's line, verbatim
	// Task is the Swarm task the line came from, when the stream carries
	// docker's log details (see docker.Engine.ServiceLogs); not sent on.
	Task string `json:"-"`
}

var (
	logfmtLevelRe = regexp.MustCompile(`(?i)(?:^|\s)level=("?)([a-zA-Z]+)`)
	tokenLevelRe  = regexp.MustCompile(`(?i)\b(TRACE|DEBUG|INFO|WARN|WARNING|ERROR|ERR|FATAL|SUCCESS)\b`)
)

// ParseLogLine strips an optional leading docker RFC3339 timestamp, detects a
// log level heuristically, and returns the remainder as the message.
func ParseLogLine(raw string) LogLine {
	ts, rest := splitDockerTimestamp(raw)
	task, rest := splitDockerDetails(rest)
	return LogLine{Time: ts, Level: detectLevel(rest), Msg: rest, Task: task}
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

// ScanEndNotice describes why a bufio.Scanner reading a docker log stream
// stopped, as a synthetic error-level LogLine to surface in-band — so a read
// that ends early (an oversized line, a broken connection) is never silently
// indistinguishable from a clean end of stream. A nil err (clean EOF, or a
// bounded non-follow read that simply ran out of log) needs no notice, so
// ok is false in that case; every other error produces one, with a dedicated
// message for the 1 MiB scanner-buffer case.
//
// Shared by internal/server's WebSocket log-streaming handlers and
// internal/api's AppLogs — both read a docker log stream through the same
// bufio.Scanner pattern and need the same signal when it breaks early.
func ScanEndNotice(err error) (LogLine, bool) {
	if err == nil {
		return LogLine{}, false
	}
	if errors.Is(err, bufio.ErrTooLong) {
		return LogLine{Level: "error", Msg: "log stream stopped: a single log line exceeded the 1 MiB limit"}, true
	}
	// Deliberately generic: this line is handed to whoever is reading the log —
	// including an agent, whose context reaches a model provider — and the
	// underlying error can name a docker socket path or a host. Callers log the
	// real cause.
	return LogLine{Level: "error", Msg: "log stream stopped unexpectedly"}, true
}

// splitDockerDetails strips the attributes docker puts before a line when a
// service's logs are read with details ("com.docker.swarm.node.id=…,
// com.docker.swarm.service.id=…,com.docker.swarm.task.id=… message") and
// returns the task ID. A line without them is returned as it is.
func splitDockerDetails(s string) (task, rest string) {
	if !strings.HasPrefix(s, "com.docker.swarm.") {
		return "", s
	}
	attrs, rest, ok := strings.Cut(s, " ")
	if !ok {
		attrs, rest = s, ""
	}
	for _, kv := range strings.Split(attrs, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return "", s // not docker's attribute list after all
		}
		if k == "com.docker.swarm.task.id" {
			task = v
		}
	}
	return task, rest
}
