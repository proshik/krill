package server_test

import (
	"context"
	"encoding/json"
	"net/url"
	"reflect"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

func TestEnvClassificationPersistsWithoutChangingText(t *testing.T) {
	s := newShareEnv(t)
	ctx := context.Background()
	raw := "# Keep this comment\n PUBLIC =  ordinary  \nTOKEN=secret=value\n"
	if err := s.q.UpdateApplicationEnv(ctx, db.UpdateApplicationEnvParams{ID: s.webID, EnvText: raw}); err != nil {
		t.Fatal(err)
	}
	check := func(want []string) {
		t.Helper()
		app, err := s.q.GetApplication(ctx, s.webID)
		if err != nil {
			t.Fatal(err)
		}
		if app.EnvText != raw {
			t.Fatalf("environment text changed: %q", app.EnvText)
		}
		b, err := json.Marshal(app)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(b, &fields); err != nil {
			t.Fatal(err)
		}
		var keys []string
		if err := json.Unmarshal(fields["env_public_keys"], &keys); err != nil {
			t.Fatalf("missing public-key classification: %v", err)
		}
		if !reflect.DeepEqual(keys, want) {
			t.Fatalf("public keys = %v, want %v", keys, want)
		}
	}
	check([]string{}) // Legacy and unknown keys must remain secret.
	for _, keys := range []string{"PUBLIC\nMISSING", "", "TOKEN"} {
		rec := postForm(t, s.h, s.webURL+"/env", s.cookie, url.Values{"env": {raw}, "env_public_keys": {keys}})
		if hasErrFlash(rec) {
			t.Fatal("environment save refused")
		}
		want := []string{}
		if keys != "" {
			if keys == "TOKEN" {
				want = []string{"TOKEN"}
			} else {
				want = []string{"PUBLIC"}
			}
		}
		check(want)
	}
	other, err := s.q.GetApplication(ctx, s.apiID)
	if err != nil {
		t.Fatal(err)
	}
	if other.EnvText != "" {
		t.Fatal("changed another application")
	}
}

func TestSaveEnvOldClientDoesNotReviveStalePublicKeys(t *testing.T) {
	s := newShareEnv(t)
	ctx := t.Context()
	for _, tc := range []struct {
		name     string
		explicit bool
		want     []string
	}{
		{name: "old client", want: []string{"PUBLIC"}},
		{name: "explicit classification", explicit: true, want: []string{"TOKEN"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.q.UpdateApplicationEnvClassified(ctx, db.UpdateApplicationEnvClassifiedParams{
				ID: s.webID, EnvText: "PUBLIC=ordinary", EnvPublicKeys: []string{"PUBLIC", "TOKEN"},
			}); err != nil {
				t.Fatal(err)
			}
			raw := "PUBLIC=updated\nTOKEN=new-secret"
			form := url.Values{"env": {raw}}
			if tc.explicit {
				form.Set("env_public_keys", "TOKEN")
			}
			rec := postForm(t, s.h, s.webURL+"/env", s.cookie, form)
			if hasErrFlash(rec) {
				t.Fatal("environment save refused")
			}
			app, err := s.q.GetApplication(ctx, s.webID)
			if err != nil {
				t.Fatal(err)
			}
			if app.EnvText != raw {
				t.Fatal("environment text changed")
			}
			if !reflect.DeepEqual(app.EnvPublicKeys, tc.want) {
				t.Fatalf("public keys = %v, want %v", app.EnvPublicKeys, tc.want)
			}
		})
	}
}
