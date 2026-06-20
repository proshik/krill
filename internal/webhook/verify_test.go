package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestVerifyHMAC(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	sec := "topsecret"
	if !VerifyHMAC(sec, body, sign(sec, body)) {
		t.Error("valid signature rejected")
	}
	if VerifyHMAC("wrong", body, sign(sec, body)) {
		t.Error("wrong secret accepted")
	}
	if VerifyHMAC(sec, body, "sha256=deadbeef") {
		t.Error("bad signature accepted")
	}
	if VerifyHMAC(sec, body, "") {
		t.Error("empty signature accepted")
	}
	if VerifyHMAC(sec, body, "md5=abc") {
		t.Error("non-sha256 prefix accepted")
	}
}

func TestParsePushEvent(t *testing.T) {
	e, err := ParsePushEvent([]byte(`{"ref":"refs/heads/main","deleted":false}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.Branch() != "main" {
		t.Errorf("Branch()=%q want main", e.Branch())
	}
	del, _ := ParsePushEvent([]byte(`{"ref":"refs/heads/x","deleted":true}`))
	if !del.Deleted {
		t.Error("Deleted not parsed")
	}
	tag, _ := ParsePushEvent([]byte(`{"ref":"refs/tags/v1"}`))
	if tag.Branch() != "" {
		t.Errorf("tag ref should yield empty Branch(), got %q", tag.Branch())
	}
	if _, err := ParsePushEvent([]byte(`not json`)); err == nil {
		t.Error("bad json should error")
	}
}

func TestNewSecretAndEqual(t *testing.T) {
	a, b := NewSecret(), NewSecret()
	if len(a) != 64 {
		t.Errorf("secret len=%d want 64", len(a))
	}
	if a == b {
		t.Error("two secrets equal — not random")
	}
	if !ConstantTimeEqual(a, a) || ConstantTimeEqual(a, b) {
		t.Error("ConstantTimeEqual broken")
	}
}

func TestValidTag(t *testing.T) {
	for _, ok := range []string{"latest", "v1.2.3", "sha-abc_DEF", "1"} {
		if !ValidTag(ok) {
			t.Errorf("ValidTag(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "a b", "a/b", "a:b", "tag$", string(make([]byte, 129))} {
		if ValidTag(bad) {
			t.Errorf("ValidTag(%q) = true, want false", bad)
		}
	}
}
