package api_test

import (
	"encoding/json"
	"reflect"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

func TestSetEnvPreservesClassificationAndDefaultsNewKeysToSecret(t *testing.T) {
	f := newAPIFixture(t)
	ctx := t.Context()
	raw := "# retained\nPORT=8080\nTOKEN=private"
	if err := f.q.UpdateApplicationEnvClassified(ctx, db.UpdateApplicationEnvClassifiedParams{ID: f.appID, EnvText: raw, EnvPublicKeys: []string{"PORT", "GHOST"}}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		key, value string
		remove     bool
		public     []string
	}{
		{"GHOST", "new-private", false, []string{"PORT"}},
		{"PORT", "9090", false, []string{"PORT"}},
		{"NEW", "new-private", false, []string{"PORT"}},
		{"TOKEN", "changed-private", false, []string{"PORT"}},
		{"PORT", "", true, []string{}},
		{"PORT", "recreated-private", false, []string{}},
	} {
		if err := f.svc.SetEnv(ctx, f.ident, f.appIDString, step.key, step.value, step.remove); err != nil {
			t.Fatal(err)
		}
		app, err := f.q.GetApplication(ctx, f.appID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(app.EnvPublicKeys, step.public) {
			t.Fatalf("public keys = %v, want %v", app.EnvPublicKeys, step.public)
		}
	}
	keys, err := f.svc.ListEnv(ctx, f.ident, f.appIDString)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(keys)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `[{"key":"TOKEN","source":"literal"},{"key":"GHOST","source":"literal"},{"key":"NEW","source":"literal"},{"key":"PORT","source":"literal"}]` {
		t.Fatalf("ListEnv contract changed: %s", b)
	}
}
