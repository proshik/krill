package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
)

type draftDuringResolveEngine struct {
	*releaseEngine
	edit            func() error
	reference, auth string
}

func (e *draftDuringResolveEngine) ResolveDigest(ctx context.Context, ref, auth string) (string, error) {
	e.reference, e.auth = ref, auth
	if err := e.edit(); err != nil {
		return "", err
	}
	return e.releaseEngine.ResolveDigest(ctx, ref, auth)
}
func TestPlacementCheckPreservesCapturedJobDuringFutureDraftEdit(t *testing.T) {
	q, st, d, engine, appID := releaseFixture(t)
	ctx := t.Context()
	ident, err := q.GetApplicationIdentity(ctx, appID)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := q.CreateRegistry(ctx, db.CreateRegistryParams{OrganizationID: ident.OrgID, Name: "private", RegistryUrl: "registry.fixture.test", Username: "fixture", Password: "fixture-password"})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetApplicationRegistry(ctx, db.SetApplicationRegistryParams{ID: appID, RegistryID: &reg.ID}); err != nil {
		t.Fatal(err)
	}
	original := "registry.fixture.test/app"
	if err := q.UpdateApplicationImage(ctx, db.UpdateApplicationImageParams{ID: appID, Image: original, Tag: "v1"}); err != nil {
		t.Fatal(err)
	}
	engine.digest = original + "@sha256:" + strings.Repeat("1", 64)
	e := &draftDuringResolveEngine{releaseEngine: engine, edit: func() error {
		return q.UpdateApplicationImage(ctx, db.UpdateApplicationImageParams{ID: appID, Image: "future.fixture.test/app", Tag: "v2"})
	}}
	d.engine = e
	id := d.EnqueueImage(appID, TriggerManual, original, "v1")
	runReleaseJob(t, d, id)
	requireDeployment(t, q, id, "done")
	want, err := docker.EncodeRegistryAuth("fixture", "fixture-password", "registry.fixture.test")
	if err != nil {
		t.Fatal(err)
	}
	if e.reference != original+":v1" || e.auth != want || len(e.deployed) != 1 || e.deployed[0].Image != engine.digest || e.deployed[0].RegistryAuth != want {
		t.Fatal("captured image/credential was not used")
	}
	draft, err := q.GetApplication(ctx, appID)
	if err != nil || draft.Image != "future.fixture.test/app" || draft.Tag != "v2" {
		t.Fatal("future draft overwritten")
	}
	if _, err := st.GetApplication(ctx, appID); !errors.Is(err, ErrCredentialHostMismatch) {
		t.Fatal("fixture draft should remain mismatched")
	}
}
