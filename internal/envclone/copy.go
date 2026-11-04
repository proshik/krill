// Package envclone creates isolated environment configurations from a saved snapshot.
package envclone

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/proshik/krill/internal/appmetrics"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/envtext"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/secret"
)

// CopyApplication produces target values, not a persistence-ready source row.
// Credential references still require same-org and host validation by the store.
func CopyApplication(src db.Application, links []db.ListDBLinksByApplicationRow) (db.Application, error) {
	linked := make([]string, 0, len(links))
	for _, link := range links {
		linked = append(linked, link.VarName)
	}
	// Explicit allow-list: a future identity/secret column must not start
	// cloning merely because it was added to the generated Application type.
	out := db.Application{
		Name: src.Name, Image: src.Image, Tag: src.Tag, Port: src.Port,
		Status: "idle", SourceType: src.SourceType, GitBranch: src.GitBranch,
		DockerfilePath: src.DockerfilePath, RegistryID: src.RegistryID,
		MemoryLimit: src.MemoryLimit, CpuLimit: src.CpuLimit, Replicas: src.Replicas,
		RestartCondition: src.RestartCondition, RestartMaxAttempts: src.RestartMaxAttempts,
		HealthcheckCmd: src.HealthcheckCmd, HealthcheckInterval: src.HealthcheckInterval,
		HealthcheckTimeout: src.HealthcheckTimeout, HealthcheckRetries: src.HealthcheckRetries,
		HealthcheckStartPeriod: src.HealthcheckStartPeriod, Command: src.Command,
		GitCredentialID: src.GitCredentialID, BuildArgs: src.BuildArgs,
		PlacementMode: src.PlacementMode, PlacementNodes: src.PlacementNodes,
		MetricsEnabled: src.MetricsEnabled, MetricsTokenEnv: src.MetricsTokenEnv,
		PredeployCommand: src.PredeployCommand,
	}
	out.EnvText = envtext.Clone(src.EnvText, src.EnvPublicKeys, linked)
	out.EnvPublicKeys = envtext.PublicKeys(out.EnvText, src.EnvPublicKeys)
	out.GitUrl = ""
	if src.GitUrl != "" {
		u, err := url.Parse(src.GitUrl)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return db.Application{}, errors.New("invalid clone Git URL")
		}
		// User-info has explicit credential semantics. Preserve the URL's other
		// components; credential-bearing query parameters are unsupported rather
		// than silently rewritten. Public env values use no such inference.
		for _, component := range []string{u.RawQuery, u.Fragment} {
			values, err := url.ParseQuery(component)
			if err != nil {
				return db.Application{}, errors.New("unsupported clone Git URL parameters")
			}
			for key := range values {
				switch strings.ToLower(key) {
				case "token", "access_token", "private_token", "auth", "authorization", "password", "passwd", "secret", "api_key", "apikey":
					return db.Application{}, errors.New("credential-bearing Git URL parameters are unsupported")
				}
			}
		}
		u.User = nil
		out.GitUrl = u.String()
	}
	out.MetricsToken = nil
	if src.MetricsEnabled || src.MetricsToken != nil {
		token, err := appmetrics.NewToken()
		if err != nil {
			return db.Application{}, err
		}
		enc := secret.Enc(token)
		out.MetricsToken = &enc
	}
	return out, nil
}

var suffixPattern = regexp.MustCompile(`^[a-z0-9]{1,48}$`)
var dnsLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// DatabaseName reserves the entire identity suffix BEFORE shortening the name.
func DatabaseName(name, suffix string) (string, error) {
	if !suffixPattern.MatchString(suffix) {
		return "", errors.New("invalid clone database suffix")
	}
	prefix := dbservice.SanitizeIdent(name)
	limit := 63 - 1 - len(suffix)
	if len(prefix) > limit {
		prefix = prefix[:limit]
	}
	return prefix + "_" + suffix, nil
}

// PrimaryHost uses the new app ID so repeated names in other environments or
// projects cannot alias. Existing manual claims are still checked by hostshare.
func PrimaryHost(name string, appID int64, base string) (string, error) {
	base = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(base), "."))
	for _, label := range strings.Split(base, ".") {
		if !dnsLabelPattern.MatchString(label) {
			return "", errors.New("invalid clone base domain")
		}
	}
	if appID <= 0 {
		return "", errors.New("invalid clone application identity")
	}
	suffix := "-" + strconv.FormatInt(appID, 10)
	limit := min(63, 253-len(base)-1) - len(suffix)
	if limit < 1 {
		return "", errors.New("clone base domain is too long")
	}
	prefix := org.Slugify(name)
	if prefix == "" {
		prefix = "app"
	}
	if len(prefix) > limit {
		prefix = strings.TrimRight(prefix[:limit], "-")
	}
	return prefix + suffix + "." + base, nil
}
