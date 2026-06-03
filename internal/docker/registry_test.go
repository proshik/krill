package docker

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestEncodeRegistryAuth(t *testing.T) {
	enc, err := EncodeRegistryAuth("proshik", "tok", "ghcr.io")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.URLEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("not base64url: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if m["username"] != "proshik" || m["password"] != "tok" || m["serveraddress"] != "ghcr.io" {
		t.Fatalf("fields = %+v", m)
	}
}

func TestParseBearerChallenge(t *testing.T) {
	h := `Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:proshik/x:pull"`
	c := parseBearerChallenge(h)
	if c["realm"] != "https://ghcr.io/token" || c["service"] != "ghcr.io" || c["scope"] != "repository:proshik/x:pull" {
		t.Fatalf("parsed = %+v", c)
	}
}
