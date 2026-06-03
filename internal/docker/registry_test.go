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
