package observability

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
	"unicode/utf8"
)

type ExecFunc func(context.Context, string, []string, io.Writer) error
type ModuleHealth struct {
	Healthy bool
	Message string
}
type TargetStatus struct {
	Instance, Node, Health string
	LastScrape             time.Time
	LastError              string
}
type EndpointStatus struct {
	EndpointID     int64
	Resolved, Kept int
	Targets        []TargetStatus
	Found          bool
}

const statusScript = `exec 3<>/dev/tcp/127.0.0.1/12345 || exit 7
printf 'GET %s HTTP/1.0\r\nHost: localhost\r\n\r\n' "$1" >&3
cat <&3`
const (
	statusBase       = "/api/v0/web/components/"
	statusModuleID   = "apps.targets.default"
	statusMaxBody    = 1 << 20
	statusMaxErrText = 300
	statusTimeout    = 5 * time.Second
)

type limitedBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > statusMaxBody {
		b.overflow = true
		return 0, errors.New("collector answer exceeds size limit")
	}
	return b.Buffer.Write(p)
}

func fetch(ctx context.Context, exec ExecFunc, path string) ([]byte, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	var buf limitedBuffer
	if err := exec(ctx, AppsServiceName, []string{"bash", "-c", statusScript, "krill-status", path}, &buf); err != nil {
		return nil, false, fmt.Errorf("query the collector: %w", err)
	}
	if buf.overflow {
		return nil, false, errors.New("collector answer exceeds size limit")
	}
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf.Bytes())), nil)
	if err != nil {
		return nil, false, errors.New("the collector gave an unexpected answer")
	}
	defer response.Body.Close()
	if response.StatusCode == 404 {
		return nil, false, nil
	}
	if response.StatusCode != 200 {
		return nil, false, fmt.Errorf("the collector answered HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, statusMaxBody+1))
	if err != nil || len(body) > statusMaxBody {
		return nil, false, errors.New("the collector gave an unexpected answer")
	}
	return body, true, nil
}

type apiValue struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}
type apiAttr struct {
	Name  string    `json:"name"`
	Type  string    `json:"type"`
	Value apiValue  `json:"value"`
	Body  []apiAttr `json:"body"`
}
type apiPair struct {
	Key   string   `json:"key"`
	Value apiValue `json:"value"`
}
type apiComponent struct {
	Health struct {
		State   string `json:"state"`
		Message string `json:"message"`
	} `json:"health"`
	Exports   []apiAttr `json:"exports"`
	DebugInfo []apiAttr `json:"debugInfo"`
}

func valueString(v apiValue) string { var s string; _ = json.Unmarshal(v.Value, &s); return s }
func shortStatusText(s string) string {
	if len(s) <= statusMaxErrText {
		return s
	}
	s = s[:statusMaxErrText]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
func readComponent(ctx context.Context, exec ExecFunc, id string) (apiComponent, bool, error) {
	var c apiComponent
	body, found, err := fetch(ctx, exec, statusBase+id)
	if err != nil || !found {
		return c, found, err
	}
	if err := json.Unmarshal(body, &c); err != nil || c.Health.State == "" {
		return c, false, errors.New("the collector gave an unexpected answer")
	}
	return c, true, nil
}
func exportCount(c apiComponent, name string) (int, error) {
	for _, a := range c.Exports {
		if a.Name == name && a.Value.Type == "array" {
			var items []json.RawMessage
			if err := json.Unmarshal(a.Value.Value, &items); err != nil {
				return 0, errors.New("the collector gave an unexpected answer")
			}
			return len(items), nil
		}
	}
	return 0, errors.New("the collector gave an unexpected answer: missing discovery export")
}
func scrapeTargets(c apiComponent) ([]TargetStatus, error) {
	var targets []TargetStatus
	for _, block := range c.DebugInfo {
		if block.Name != "target" || block.Type != "block" {
			continue
		}
		t := TargetStatus{Health: "unknown"}
		for _, attr := range block.Body {
			switch attr.Name {
			case "health":
				t.Health = valueString(attr.Value)
			case "last_error":
				t.LastError = shortStatusText(valueString(attr.Value))
			case "last_scrape":
				raw := valueString(attr.Value)
				if raw != "" {
					parsed, err := time.Parse(time.RFC3339Nano, raw)
					if err != nil {
						return nil, errors.New("the collector gave an unexpected answer: invalid scrape time")
					}
					t.LastScrape = parsed
				}
			case "labels":
				var pairs []apiPair
				if err := json.Unmarshal(attr.Value.Value, &pairs); err != nil {
					return nil, errors.New("the collector gave an unexpected answer: invalid target labels")
				}
				for _, p := range pairs {
					switch p.Key {
					case "instance":
						t.Instance = valueString(p.Value)
					case "krill_node":
						t.Node = valueString(p.Value)
					}
				}
			}
		}
		targets = append(targets, t)
	}
	return targets, nil
}

// ReadAppsStatus reads only the requested app's components, never the module's secret content.
func ReadAppsStatus(ctx context.Context, exec ExecFunc, appID int64, endpointIDs []int64) (ModuleHealth, []EndpointStatus, error) {
	// Bound the entire operation, including a tab with ten endpoints.
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	c, found, err := readComponent(ctx, exec, statusModuleID)
	if err != nil {
		return ModuleHealth{}, nil, err
	}
	health := ModuleHealth{Healthy: found && c.Health.State == "healthy", Message: shortStatusText(c.Health.Message)}
	if !found {
		health.Message = "collector module is not loaded"
	}
	if !health.Healthy {
		return health, nil, nil
	}
	var endpoints []EndpointStatus
	for _, id := range endpointIDs {
		ep := EndpointStatus{EndpointID: id}
		label := AppsComponentLabel(appID, id)
		scrape, found, err := readComponent(ctx, exec, statusModuleID+"/prometheus.scrape."+label)
		if err != nil {
			return health, nil, err
		}
		if !found {
			endpoints = append(endpoints, ep)
			continue
		}
		dns, df, err := readComponent(ctx, exec, statusModuleID+"/discovery.dns."+label)
		if err != nil {
			return health, nil, err
		}
		relabel, rf, err := readComponent(ctx, exec, statusModuleID+"/discovery.relabel."+label)
		if err != nil {
			return health, nil, err
		}
		ep.Found = df && rf
		if ep.Found {
			ep.Resolved, err = exportCount(dns, "targets")
			if err != nil {
				return health, nil, err
			}
			ep.Kept, err = exportCount(relabel, "output")
			if err != nil {
				return health, nil, err
			}
			ep.Targets, err = scrapeTargets(scrape)
			if err != nil {
				return health, nil, err
			}
		}
		endpoints = append(endpoints, ep)
	}
	return health, endpoints, nil
}
