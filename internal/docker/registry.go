package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/docker/docker/api/types/registry"

	"github.com/proshik/krill/internal/netguard"
)

// EncodeRegistryAuth returns the base64url(JSON) auth blob Swarm expects in
// EncodedRegistryAuth for pulling a private image from serverAddr.
func EncodeRegistryAuth(username, password, serverAddr string) (string, error) {
	b, err := json.Marshal(registry.AuthConfig{
		Username:      username,
		Password:      password,
		ServerAddress: registryHost(serverAddr),
	})
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

var bearerChallengeRe = regexp.MustCompile(`(\w+)="([^"]*)"`)

// parseBearerChallenge extracts realm/service/scope from a WWW-Authenticate: Bearer ... header.
func parseBearerChallenge(h string) map[string]string {
	out := map[string]string{}
	for _, m := range bearerChallengeRe.FindAllStringSubmatch(h, -1) {
		out[m[1]] = m[2]
	}
	return out
}

// registryHost normalizes a registry value to a bare host: strips a scheme and
// any path the user may have pasted (e.g. "https://ghcr.io/proshik/x" -> "ghcr.io").
func registryHost(registryURL string) string {
	h := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(registryURL, "https://"), "http://"), "/")
	if i := strings.IndexByte(h, '/'); i >= 0 {
		h = h[:i]
	}
	return h
}

// RegistryRepo strips the registry host prefix from image to derive the repo
// path used in the v2 API (e.g. "ghcr.io/proshik/x" -> "proshik/x").
func RegistryRepo(registryURL, image string) string {
	return strings.TrimPrefix(image, registryHost(registryURL)+"/")
}

// RegistryHost is the host part of a stored registry URL (exported for the
// deployer's credential-host check).
func RegistryHost(registryURL string) string { return registryHost(registryURL) }

// ImageHost returns the registry host an image reference points at. A
// reference without a host belongs to Docker Hub, which is what Docker itself
// assumes; the first segment counts as a host only when it looks like one.
func ImageHost(image string) string {
	first, rest, ok := strings.Cut(image, "/")
	if !ok {
		return "docker.io"
	}
	_ = rest
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return strings.ToLower(first)
	}
	return "docker.io"
}

// RegistryListTags lists the tags for repo in a registry, following the v2
// token (WWW-Authenticate: Bearer) flow. Anonymous if username is empty.
// allowPrivate mirrors KRILL_ALLOW_PRIVATE_EGRESS: when false, both the tags
// request and the bearer-realm token request refuse private/loopback/
// link-local destinations (internal/netguard) — a malicious/compromised
// registry_url or WWW-Authenticate realm can't be used to probe internal
// hosts (SSRF egress guard).
func RegistryListTags(ctx context.Context, registryURL, username, password, repo string, allowPrivate bool) ([]string, error) {
	host := registryHost(registryURL)
	tagsURL := fmt.Sprintf("https://%s/v2/%s/tags/list", host, repo)
	client := netguard.HTTPClient(allowPrivate)
	do := func(bearer string) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, tagsURL, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		return client.Do(req)
	}
	resp, err := do("")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		ch := parseBearerChallenge(resp.Header.Get("WWW-Authenticate"))
		resp.Body.Close()
		if ch["realm"] == "" {
			return nil, fmt.Errorf("registry requires auth but no bearer realm")
		}
		treq, _ := http.NewRequestWithContext(ctx, http.MethodGet, ch["realm"], nil)
		q := treq.URL.Query()
		if ch["service"] != "" {
			q.Set("service", ch["service"])
		}
		if ch["scope"] != "" {
			q.Set("scope", ch["scope"])
		}
		treq.URL.RawQuery = q.Encode()
		if username != "" {
			treq.SetBasicAuth(username, password)
		}
		tresp, terr := client.Do(treq)
		if terr != nil {
			return nil, terr
		}
		defer tresp.Body.Close()
		if tresp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("registry token request failed: %s", tresp.Status)
		}
		var tok struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
		}
		if err := json.NewDecoder(tresp.Body).Decode(&tok); err != nil {
			return nil, err
		}
		bearer := tok.Token
		if bearer == "" {
			bearer = tok.AccessToken
		}
		resp, err = do(bearer)
		if err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry tags request failed: %s", resp.Status)
	}
	var body struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Tags, nil
}
