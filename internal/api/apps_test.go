package api_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/proshik/krill/internal/api"
)

func TestResolveAppByPathAndID(t *testing.T) {
	f := newAPIFixture(t) // helper from Step 3 below
	byID, err := f.svc.AppStatus(t.Context(), f.ident, f.appIDString)
	if err != nil {
		t.Fatalf("by id: %v", err)
	}
	byPath, err := f.svc.AppStatus(t.Context(), f.ident, "acme-proj/production/bot")
	if err != nil {
		t.Fatalf("by path: %v", err)
	}
	if byID.ID != byPath.ID {
		t.Fatalf("id and path resolved to different apps: %d vs %d", byID.ID, byPath.ID)
	}
}

func TestResolveAppRejectsForeignOrg(t *testing.T) {
	f := newAPIFixture(t)
	// otherIdent is a token scoped to a different org that owns nothing here.
	_, err := f.svc.AppStatus(t.Context(), f.otherIdent, f.appIDString)
	if err == nil {
		t.Fatal("cross-org app resolved")
	}
	var aerr *api.Error
	if !errors.As(err, &aerr) || aerr.Code != api.CodeNotFound {
		t.Fatalf("want not_found (never forbidden — it confirms the app exists), got %v", err)
	}
}

func TestListEnvNeverReturnsValues(t *testing.T) {
	f := newAPIFixture(t)
	// app fixture has env_text "SECRET_TOKEN=hunter2\nPORT=8080"
	keys, err := f.svc.ListEnv(t.Context(), f.ident, f.appIDString)
	if err != nil {
		t.Fatalf("list env: %v", err)
	}
	blob, err := json.Marshal(keys)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(blob, []byte("hunter2")) {
		t.Fatalf("env value leaked into the API response: %s", blob)
	}
	if len(keys) != 2 {
		t.Fatalf("want 2 keys, got %d", len(keys))
	}
}
