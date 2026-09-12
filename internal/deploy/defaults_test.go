package deploy

import "testing"

func TestApplyResourceDefaults(t *testing.T) {
	d := &Deployer{}
	d.SetResourceDefaults(512*1024*1024, 1_000_000_000)

	got := d.withResourceDefaults(App{})
	if got.MemoryLimitBytes != 512*1024*1024 || got.NanoCPUs != 1_000_000_000 {
		t.Fatalf("defaults not applied: %+v", got)
	}

	explicit := App{MemoryLimitBytes: 64 * 1024 * 1024, NanoCPUs: 250_000_000}
	got = d.withResourceDefaults(explicit)
	if got.MemoryLimitBytes != explicit.MemoryLimitBytes || got.NanoCPUs != explicit.NanoCPUs {
		t.Fatalf("explicit values must win: %+v", got)
	}
}
