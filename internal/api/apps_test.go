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

// TestResolveAppRejectsForeignOrg is one property (a cross-org reference
// resolves as not_found, never forbidden — telling the caller an app exists
// in someone else's org would turn id enumeration into recon) proven in both
// forms resolveApp accepts. The numeric form is guarded explicitly by the
// GetApplicationChain org check; the path form is guarded only incidentally,
// by listAppsRaw already scoping from ListProjects(id.OrgID) — a foreign
// caller never even sees the rows to match against. Both need a test, or a
// future rewrite of the path branch (e.g. into a targeted SQL lookup) could
// drop the org predicate with nothing here to catch it.
func TestResolveAppRejectsForeignOrg(t *testing.T) {
	f := newAPIFixture(t)
	// otherIdent is a token scoped to a different org that owns nothing here.
	cases := []struct {
		name string
		ref  string
	}{
		{"numeric id", f.appIDString},
		{"project/env/app path", "acme-proj/production/bot"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.AppStatus(t.Context(), f.otherIdent, tc.ref)
			if err == nil {
				t.Fatal("cross-org app resolved")
			}
			var aerr *api.Error
			if !errors.As(err, &aerr) || aerr.Code != api.CodeNotFound {
				t.Fatalf("want not_found (never forbidden — it confirms the app exists), got %v", err)
			}
		})
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
