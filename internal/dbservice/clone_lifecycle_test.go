package dbservice

import (
	"errors"
	"testing"

	"github.com/proshik/krill/internal/oplock"
)

func TestInstanceLifecycleExcludesCloneAndRetainsDeleteLock(t *testing.T) {
	ctx := t.Context()
	inst := samplePGInstance()
	eng := newMockEngine()
	svc := newSvc(eng, newFakeStore(inst))
	key := oplock.DBInstance(inst.AppName)
	if !oplock.TryAcquire(key) {
		t.Fatal("fixture lock busy")
	}
	for _, action := range []func() error{
		func() error { return svc.DeployInstance(inst.ID) },
		func() error { return svc.StartInstance(ctx, inst.ID) },
		func() error { return svc.StopInstance(ctx, inst.ID) },
		func() error { return svc.ParkInstance(ctx, inst.ID, 0) },
		func() error { return svc.RemoveInstanceContainers(ctx, inst.ID, false) },
	} {
		if err := action(); !errors.Is(err, ErrInstanceBusy) {
			oplock.Release(key)
			t.Fatalf("lifecycle raced clone: %v", err)
		}
	}
	oplock.Release(key)
	if len(eng.removed) != 0 || len(eng.scaled) != 0 || len(eng.deployed) != 0 {
		t.Fatal("busy lifecycle mutated engine")
	}
	if err := svc.RemoveInstanceThen(ctx, inst.ID, false, func() error {
		if err := svc.StartInstance(ctx, inst.ID); !errors.Is(err, ErrInstanceBusy) {
			t.Fatalf("delete released lock before row cleanup: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !oplock.TryAcquire(key) {
		t.Fatal("delete leaked lock")
	}
	oplock.Release(key)
}
